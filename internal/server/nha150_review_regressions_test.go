package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
)

// The existing C5 proxy pauses real SQL responses, not parsing or publication.
// Barrier one is precisely before 029 CaptureWorkspace; barrier two is 149's
// post-attempt Refresh before it observes the filesystem again.
func nha150CaptureRetryRegression(t *testing.T, unavailable bool) {
	t.Helper()
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
	first := proxy.arm(t, nha029C5CutAfterStatement, nha029C5SQLActiveRead)
	defer first.finish(true)
	a := append(bytes.Clone(r.raw), []byte("# valid candidate A, same bytes/date after transient capture\n")...)
	if err := os.WriteFile(r.path, a, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.path)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case held := <-first.held:
		if held.statement != nha029C5SQLActiveRead {
			t.Fatal("wrong pre-capture SQL barrier")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("watcher never reached real029 pre-capture query")
	}
	if unavailable {
		canary := filepath.Join(filepath.Dir(r.path), "private-canary.yaml")
		if err := os.WriteFile(canary, []byte("NHA150_PRIVATE_CAPTURE_CANARY"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(r.path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(canary, r.path); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(r.path, []byte("# different B: NHA150_PRIVATE_CAPTURE_CANARY\nexternal_workers: [\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The first barrier has been claimed and remains held by its own ticket.
	// Replace only this proxy's future one-shot ticket, under its actual mutex.
	proxy.mu.Lock()
	if proxy.ticket != first || !first.claimed.Load() {
		proxy.mu.Unlock()
		t.Fatal("first capture barrier was not actually claimed")
	}
	proxy.ticket = nil
	proxy.mu.Unlock()
	second := proxy.arm(t, nha029C5CutAfterStatement, nha029C5SQLDocumentRead)
	defer second.finish(true)
	first.finish(true)
	select {
	case held := <-second.held:
		if held.statement != nha029C5SQLDocumentRead {
			t.Fatal("wrong post-attempt refresh barrier")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("capture refusal never returned through real149 Refresh")
	}
	if err := os.Remove(r.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.path, a, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(r.path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second.finish(true)
	digest := sha256.Sum256(a)
	nha042ServerWait(t, "same valid A automatically retried after actual capture refusal", func() bool {
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
	actual, err := os.ReadFile(r.path)
	if err != nil || !bytes.Equal(actual, a) {
		t.Fatal("capture recovery overwrote source A")
	}
	_, commands := nha150Counts(t, r.store)
	if commands != 0 || len(r.driver.snapshot()) != 0 {
		t.Fatal("capture retry replayed an action")
	}
}
func TestNHA150ReviewCaptureUnavailableDoesNotPermanentlyRejectValidObservation(t *testing.T) {
	nha150CaptureRetryRegression(t, true)
}
func TestNHA150ReviewForeignParsedBytesCannotRejectPreviousValidObservation(t *testing.T) {
	nha150CaptureRetryRegression(t, false)
}

// Only watcher waiting is controlled. Every read, native API mutation, source
// parse, SQL transaction and publication below uses the actual default Run.
type nha150StepClock struct {
	mu      sync.Mutex
	now     time.Time
	waiting chan struct{}
	advance chan struct{}
}

func (c *nha150StepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *nha150StepClock) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case c.waiting <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.advance:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *nha150StepClock) paused(t *testing.T) {
	t.Helper()
	select {
	case <-c.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("real watcher did not reach its clock wait")
	}
}
func (c *nha150StepClock) tick(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	c.now = c.now.Add(250 * time.Millisecond)
	c.mu.Unlock()
	select {
	case c.advance <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("real watcher did not consume its scheduled tick")
	}
	c.paused(t)
}
func nha150StartClockTLS(t *testing.T, clock *nha150StepClock) nha149TLSFixture {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	run := &nha162Running{stop: stop, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	observed := make(chan nha162ObservedRun, 1)
	dependencies := runDependencies{configurationWatcherClock: clock, observeLists: func(lists *listComposition, sessions *sessionCoordinator) {
		observed <- nha162ObservedRun{lists, sessions}
	}}
	go func() {
		run.done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil, dependencies)
	}()
	t.Cleanup(func() {
		if !run.joined {
			if err := run.join(t); err != nil {
				t.Error("clock Run cleanup", err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("clock Run exited before ready", err)
	case <-time.After(12 * time.Second):
		t.Fatal("clock Run did not become ready")
	}
	select {
	case run.observed = <-observed:
	default:
		t.Fatal("default Run did not expose its actual owners")
	}
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	const password = "NHA150 isolated synthetic clock password"
	nha162RunBootstrap(t, ctx, data, "NHA150 admin", password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA150 admin", password)
	return nha149TLSFixture{api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx}
}
func TestNHA150ReviewAPIPublicationInvalidatesCompletedSameSourceAndDate(t *testing.T) {
	clock := &nha150StepClock{now: time.Now(), waiting: make(chan struct{}, 1), advance: make(chan struct{})}
	f := nha150StartClockTLS(t, clock)
	clock.paused(t)
	clock.tick(t)
	clock.tick(t)
	// A has now crossed the actual watcher stability path and been cached clean.
	a := f.read(t)
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	b := append(bytes.Clone(f.raw), []byte("# API-published B entirely between watcher ticks\n")...)
	response, err := f.write(a, b)
	if err != nil {
		t.Fatal("real API149 publication B failed", err)
	}
	if response.Msg.Activation.GenerationRevision == nil {
		t.Fatal("API B has no published generation")
	}
	bGeneration := *response.Msg.Activation.GenerationRevision
	if err := os.WriteFile(f.path, f.raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	// No watcher observation of B was possible. Its owner notification must
	// invalidate the completed(A) cache before this identical A observation.
	clock.tick(t)
	after := f.read(t)
	if !nha150CleanHash(f.raw)(after) || after.ActiveGeneration.Revision <= bGeneration {
		t.Fatal("completed A suppressed refresh after owner published B")
	}
	_, commands := nha150Counts(t, f.run.observed.sessions.store)
	if commands != 0 {
		t.Fatal("authority resync submitted an action")
	}
}
