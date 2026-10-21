package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha150AwaitDocument(t *testing.T, f nha149TLSFixture, accept func(*wire.ReadDocumentResponse) bool) *wire.ReadDocumentResponse {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		response, err := f.api.ReadDocument(f.ctx, nha056SessionRequest(&wire.ReadDocumentRequest{Id: &wire.DocumentId{Value: "operational.main"}}, f.token))
		if err == nil {
			if err := contracts.AdmitConfigurationResponse(response.Msg); err != nil {
				t.Fatal("watcher returned an inadmissible public document")
			}
			if accept(response.Msg) {
				return response.Msg
			}
		} else if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatal("document read failed outside a publication cut", connect.CodeOf(err))
		}
		select {
		case <-deadline.C:
			t.Fatal("watcher document condition not reached")
		case <-tick.C:
		}
	}
}
func nha150Counts(t *testing.T, store *storage.Store) (attempts, commands int) {
	t.Helper()
	err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM public.config_activation_attempts),(SELECT count(*) FROM public.command_intents)`).Scan(&attempts, &commands)
	})
	if err != nil {
		t.Fatal("isolated SQL witness unavailable")
	}
	return
}
func nha150CleanHash(raw []byte) func(*wire.ReadDocumentResponse) bool {
	hash := sha256.Sum256(raw)
	return func(read *wire.ReadDocumentResponse) bool {
		workspace := read.Document.GetWorkspace()
		return workspace != nil && workspace.Dirty == wire.DirtyState_DIRTY_STATE_CLEAN && bytes.Equal(read.Document.Version.SourceSha256, hash[:]) && bytes.Equal(workspace.SourceSha256, hash[:]) && workspace.Diagnostic == nil
	}
}

func TestNHA150ActualRunTLSCoalescesEditsAndAdmitsAtomicRenameWithSameDate(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	beforeAttempts, beforeCommands := nha150Counts(t, f.run.observed.sessions.store)
	var raw []byte
	for i := 0; i < 20; i++ {
		raw = append(bytes.Clone(f.raw), []byte(fmt.Sprintf("# external burst %02d\n", i))...)
		if err := os.WriteFile(f.path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	after := nha150AwaitDocument(t, f, nha150CleanHash(raw))
	if after.ActiveGeneration.Revision <= before.ActiveGeneration.Revision {
		t.Fatal("direct edit did not activate through029")
	}
	attempts, commands := nha150Counts(t, f.run.observed.sessions.store)
	if attempts != beforeAttempts+1 || commands != beforeCommands {
		t.Fatal("burst duplicated publication or submitted a command")
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := append(bytes.Clone(f.raw), []byte("# atomic rename with deliberately identical mtime\n")...)
	tmp := filepath.Join(filepath.Dir(f.path), ".editor-swap")
	if err := os.WriteFile(tmp, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		t.Fatal(err)
	}
	nha150AwaitDocument(t, f, nha150CleanHash(replacement))
	// This interval spans more than one full stability window. A watcher that
	// mistakes its own active bytes for another edit would publish again here.
	<-time.After(1200 * time.Millisecond)
	attempts, commands = nha150Counts(t, f.run.observed.sessions.store)
	if attempts != beforeAttempts+2 || commands != beforeCommands {
		t.Fatal("stable source looped or produced an action")
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, replacement) {
		t.Fatal("watcher rewrote editor bytes")
	}
}

func TestNHA150ActualRunTLSRejectedDiagnosticsTrackCurrentSourceThenDisappear(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	for _, raw := range [][]byte{[]byte("# NHA150_PRIVATE_CANARY\nexternal_workers: [\n"), []byte("workday: {timezone: 'NHA150_PRIVATE_CANARY/not-a-timezone'}\n")} {
		if err := os.WriteFile(f.path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		info, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		dirty := nha150AwaitDocument(t, f, func(read *wire.ReadDocumentResponse) bool {
			w := read.Document.GetWorkspace()
			return w != nil && w.Dirty == wire.DirtyState_DIRTY_STATE_REJECTED && w.Diagnostic != nil && bytes.Equal(w.SourceSha256, hash[:])
		})
		w := dirty.Document.GetWorkspace()
		d := w.Diagnostic
		if dirty.ActiveGeneration.Revision != before.ActiveGeneration.Revision || dirty.Document.Version.Revision != before.Document.Version.Revision || d.Document.Value != "operational.main" || !bytes.Equal(d.ObservedSourceSha256, hash[:]) || !d.ObservedAt.AsTime().Equal(info.ModTime().UTC()) || !d.ObservedAt.AsTime().Equal(w.UpdatedAt.AsTime()) || d.Code != wire.DiagnosticCode_DIAGNOSTIC_CODE_CANDIDATE_REJECTED {
			t.Fatal("rejection mixed active authority and observed source")
		}
		if bytes.Contains([]byte(fmt.Sprintf("%v", dirty)), []byte("NHA150_PRIVATE_CANARY")) {
			t.Fatal("diagnostic disclosed rejected source")
		}
	}
	corrected := append(bytes.Clone(f.raw), []byte("# correction replaces both rejected sources\n")...)
	if err := os.WriteFile(f.path, corrected, 0600); err != nil {
		t.Fatal(err)
	}
	clean := nha150AwaitDocument(t, f, nha150CleanHash(corrected))
	if clean.ActiveGeneration.Revision <= before.ActiveGeneration.Revision {
		t.Fatal("correction never became active")
	}
	_, commands := nha150Counts(t, f.run.observed.sessions.store)
	if commands != 0 {
		t.Fatal("automatic correction submitted an action")
	}
}

func TestNHA150ActualRunTLSSourceUnavailableNeverBecomesClean(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	unavailable := nha150AwaitDocument(t, f, func(read *wire.ReadDocumentResponse) bool {
		return read.Document.GetWorkspaceUnobserved().GetReason() == wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_SOURCE_UNAVAILABLE
	})
	if unavailable.ActiveGeneration.Revision != before.ActiveGeneration.Revision {
		t.Fatal("missing source changed SQL authority")
	}
	replacement := append(bytes.Clone(f.raw), []byte("# source recreated\n")...)
	if err := os.WriteFile(f.path, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	nha150AwaitDocument(t, f, nha150CleanHash(replacement))
}

func TestNHA150ActualRunTLSAPIAndWatcherShareTheExternalHashPrecondition(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	external := append(bytes.Clone(f.raw), []byte("# independent editor wins before stale API request\n")...)
	if err := os.WriteFile(f.path, external, 0600); err != nil {
		t.Fatal(err)
	}
	answer := make(chan error, 1)
	go func() {
		_, err := f.write(before, append(bytes.Clone(f.raw), []byte("# stale API candidate\n")...))
		answer <- err
	}()
	nha150AwaitDocument(t, f, nha150CleanHash(external))
	select {
	case err := <-answer:
		if connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal("stale API request was not an exact-hash conflict", connect.CodeOf(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("API/watcher serialization did not drain")
	}
	actual, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(actual, external) {
		t.Fatal("stale API request overwrote external bytes")
	}
}

func TestNHA150ActualWatcherRejectsExternalChangeInsideSQLStagingAndRebuilds(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	backendPort := int(environment.observer.Config().Port)
	proxy := nha029C5StartSQLProxy(t, backendPort)
	captured, err := environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Replace(captured.Bytes(), []byte("  port: "+strconv.Itoa(backendPort)+"\n"), []byte("  port: "+strconv.Itoa(proxy.port())+"\n"), 1)
	if bytes.Equal(raw, captured.Bytes()) {
		t.Fatal("isolated SQL proxy not selected")
	}
	environment.config = nha029RunFixtureFromRaw(t, raw)
	r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
	defer r.close(t)
	before := r.generation(t)
	ticket := proxy.arm(t, nha029C5CutAfterStatement, nha029C5SQLProtectedSource)
	defer ticket.finish(true)
	b := append(bytes.Clone(r.raw), []byte("# watcher candidate B before real SQL staging\n")...)
	c := append(bytes.Clone(r.raw), []byte("# external candidate C during real SQL staging\n")...)
	if err := os.WriteFile(r.path, b, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case held := <-ticket.held:
		if held.statement != nha029C5SQLProtectedSource {
			t.Fatal("wrong staging witness")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("watcher did not reach real029 protected-source SQL staging")
	}
	if err := os.WriteFile(r.path, c, 0600); err != nil {
		t.Fatal(err)
	}
	ticket.finish(true)
	digest := sha256.Sum256(c)
	nha042ServerWait(t, "fresh external C published automatically", func() bool {
		active, found, err := r.store.ReadActiveConfig(context.Background())
		if err != nil || !found || active.Revision().Number() <= int64(before) {
			return false
		}
		for _, version := range active.Versions() {
			if version.Ref().Document().String() == "operational.main" {
				return version.SHA256() == digest
			}
		}
		return false
	})
	bHash := sha256.Sum256(b)
	var retainedB, intents int
	if err := environment.observer.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM public.config_versions WHERE document_id='operational.main' AND source_hash=$1),(SELECT count(*) FROM public.command_intents)`, bHash[:]).Scan(&retainedB, &intents); err != nil {
		t.Fatal(err)
	}
	if retainedB != 0 || intents != 0 || len(r.driver.snapshot()) != 0 {
		t.Fatal("stale staging retained B or replayed an action")
	}
	actual, err := os.ReadFile(r.path)
	if err != nil || !bytes.Equal(actual, c) {
		t.Fatal("watcher overwrote the independent editor")
	}
}

func TestNHA150ColdStartupWithoutActiveRejectsInvalidBeforeFactoryOrListener(t *testing.T) {
	cluster := testpg.Start(t)
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", shortPrivateDataDir(t), cluster.Port))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("workday: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	err = RunWithStartupFactory(context.Background(), startup, func(context.Context, string, string) (net.Listener, error) {
		called = true
		return nil, errors.New("listener must stay closed")
	}, func(net.Addr) { called = true }, func(context.Context, *storage.Store) (RuntimePlan, error) {
		called = true
		return RuntimePlan{}, errors.New("factory must stay closed")
	})
	if err == nil || called {
		t.Fatal("invalid first source reached operational composition")
	}
}

func TestNHA150ActualWatcherActivationAndShutdownDoNotReplayRuns(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	before := r.generation(t)
	r.edit(t, "# automatic operational activation with existing internal programs\n")
	nha042ServerWait(t, "automatic activation with real internal engines", func() bool {
		stamp, err := r.owner.CurrentConfig(context.Background())
		if err != nil {
			return false
		}
		number, _, err := stamp.Components()
		return err == nil && number > before
	})
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("refresh replayed an action")
	}
	_, commands := nha150Counts(t, r.store)
	if commands != 0 {
		t.Fatal("refresh claimed a durable action")
	}
	r.close(t)
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("shutdown replayed an action")
	}
}

func TestNHA150WatcherCancellationDuringSQLStagingJoinsBeforeStoreClose(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	backendPort := int(environment.observer.Config().Port)
	proxy := nha029C5StartSQLProxy(t, backendPort)
	captured, err := environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Replace(captured.Bytes(), []byte("  port: "+strconv.Itoa(backendPort)+"\n"), []byte("  port: "+strconv.Itoa(proxy.port())+"\n"), 1)
	if bytes.Equal(raw, captured.Bytes()) {
		t.Fatal("isolated SQL proxy not selected")
	}
	environment.config = nha029RunFixtureFromRaw(t, raw)
	r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
	ticket := proxy.arm(t, nha029C5CutAfterStatement, nha029C5SQLProtectedSource)
	defer ticket.finish(true)
	r.edit(t, "# canceled watcher SQL staging\n")
	select {
	case <-ticket.held:
	case <-time.After(8 * time.Second):
		t.Fatal("watcher never reached SQL staging")
	}
	r.run.cancel()
	ticket.finish(true)
	r.close(t)
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("canceled watcher dispatched a command")
	}
}
