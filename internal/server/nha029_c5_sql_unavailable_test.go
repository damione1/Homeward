package server

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// Unlike the post-COMMIT lost-ACK oracle, this fault refuses configuration
// connections before Owner.Activate can read A or begin its write transaction.
// The existing proxy preserves only Run's actual singleton024 runtime session.
func TestNHA029C5OwnerSQLUnavailableBeforeBeginPreservesA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	environment := nha042ServerEnvironmentFor(t)
	backendPort := int(environment.observer.Config().Port)
	proxy := nha029C5StartSQLProxy(t, backendPort)
	captured, err := environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(string(captured.Bytes()), "  port: "+strconv.Itoa(backendPort)+"\n", "  port: "+strconv.Itoa(proxy.port())+"\n", 1)
	if raw == string(captured.Bytes()) {
		t.Fatal("isolated PostgreSQL locator was not routed through the real proxy")
	}
	environment.config = nha029RunFixtureFromRaw(t, []byte(raw))
	r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	original, controller := r.composition.scripts.interpreter, r.controller()
	ids := nha029C5HistoryWaits(t, original)
	waits := nha029C5PrecommitWaitingNodes(t, original, ids)
	a := nha029C5HistoryActive(t, ctx, r)
	stamp, err := r.owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The independent observer bypasses only the test fault, never the Store
	// under test. It also identifies the real024 session which must survive.
	var runtimeCount int
	var runtimePID uint32
	if err := environment.observer.QueryRow(ctx, `SELECT count(*),COALESCE(min(pid),0) FROM pg_locks
WHERE locktype='advisory' AND classid=481316 AND objid=24 AND objsubid=2 AND granted`).Scan(&runtimeCount, &runtimePID); err != nil || runtimeCount != 1 || runtimePID == 0 {
		t.Fatalf("missing Run-owned singleton024 session: count=%d pid=%d err=%v", runtimeCount, runtimePID, err)
	}
	if _, err := environment.observer.Exec(ctx, `CREATE SEQUENCE public.nha029_unavailable_pointer_updates START WITH 1;
CREATE FUNCTION public.nha029_count_unavailable_pointer_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM nextval('public.nha029_unavailable_pointer_updates'); RETURN NEW; END $$;
CREATE TRIGGER nha029_count_unavailable_pointer_update BEFORE UPDATE ON public.config_active
FOR EACH ROW EXECUTE FUNCTION public.nha029_count_unavailable_pointer_update()`); err != nil {
		t.Fatal(err)
	}
	assertUpdates := func(want int64) {
		t.Helper()
		var value int64
		var called bool
		if err := environment.observer.QueryRow(ctx, `SELECT last_value,is_called FROM public.nha029_unavailable_pointer_updates`).Scan(&value, &called); err != nil || called != (want != 0) || value != max(int64(1), want) {
			t.Fatalf("real pointer UPDATE count: value=%d called=%t wanted=%d err=%v", value, called, want, err)
		}
	}
	assertSQLA := func() {
		t.Helper()
		var revision int64
		var manifest []byte
		if err := environment.observer.QueryRow(ctx, `SELECT a.generation_revision,g.manifest_hash
FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision WHERE a.singleton`).Scan(&revision, &manifest); err != nil || revision != a.revision.Number() || !bytes.Equal(manifest, a.manifest[:]) {
			t.Fatalf("independent SQL observer lost A authority: revision=%d err=%v", revision, err)
		}
	}
	before := nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5AtomicDigestSQL)
	counts := nha029C5HistoryCounts(t, ctx, r)
	r.edit(t, "# B remains uncommitted while SQL is inaccessible before Begin\n")
	var callback atomic.Bool
	request := r.request(t, nil, nil, func(*registry.ConfigWriter) error { callback.Store(true); return nil })
	cutConnections := proxy.setUnavailable(t, true, runtimePID)
	if cutConnections == 0 {
		t.Fatal("C5 fault did not cut any real configuration-pool transport")
	}
	t.Cleanup(func() { proxy.setUnavailable(t, false, 0) })
	refused, refusal := r.owner.Activate(ctx, request)
	if ctx.Err() != nil {
		t.Fatal("caller cancellation manufactured the SQL-unavailable refusal")
	}
	var unavailable *storage.Error
	if !errors.Is(refusal, configactivation.ErrStartupConflict) || !errors.As(refusal, &unavailable) || unavailable.Kind() != storage.Unavailable || refused.Outcome() != configactivation.RejectedBeforeCommit || refused.Attempt().Valid() {
		t.Fatalf("real pre-Begin SQL read did not produce a typed rejection: outcome=%v transaction=%v err=%v", refused.Outcome(), refused.AttemptOutcome(), refusal)
	}
	// An activation transaction never sent to SQL is known not committed.
	// Keep this assertion while checking all invariants and the positive case.
	if refused.AttemptOutcome() != storage.TransactionNotCommitted {
		t.Errorf("pre-Begin rejection did not report known not-committed transaction: %v", refused.AttemptOutcome())
	}
	if callback.Load() {
		t.Fatal("SQL-unavailable preparation reached the configuration writer")
	}
	assertUpdates(0)
	assertSQLA()
	nha029C5PrecommitNoSQLWriter(t, ctx, r)
	if nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5AtomicDigestSQL) != before || nha029C5HistoryCounts(t, ctx, r) != counts {
		t.Fatal("pre-Begin SQL failure changed sources/revisions/manifest/registry/audits or command intents")
	}
	if after, err := r.owner.CurrentConfig(ctx); err != nil || after != stamp || r.controller() != controller || !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) {
		t.Fatalf("pre-Begin failure changed A stamp/controller/original waits: %v", err)
	}
	if _, err := r.identity.Sequence().Capture(); err != nil || len(r.driver.snapshot()) != 0 {
		t.Fatalf("pre-Begin failure closed old capture or dispatched a command: %v", err)
	}
	// Restore only the real transport fault and retry the same valid request.
	// Its single actual pointer write proves the observation trigger works.
	proxy.setUnavailable(t, false, 0)
	acceptedBeforeRecovery := proxy.acceptedConnections.Load()
	t.Logf("C5 transport reopened before pool recovery: %s", proxy.diagnostic())
	recovered, refusedReads, firstRefusal, recoveryErr := nha029C5ReadRecoveredPool(ctx, r.store, cutConnections)
	t.Logf("C5 Owner pool readiness: refused=%d first={%s} proxy={%s}", refusedReads, firstRefusal, proxy.diagnostic())
	if recoveryErr != nil || recovered.Revision() != a.revision || recovered.ManifestSHA256() != a.manifest || proxy.acceptedConnections.Load() <= acceptedBeforeRecovery {
		t.Fatalf("Store pool recovery lacks complete A on a new transport: revision=%d wanted=%d diagnostic={%s} proxy={%s}", recovered.Revision().Number(), a.revision.Number(), nha029C5SQLDiagnostic(recoveryErr), proxy.diagnostic())
	}
	// This discriminates recovery from a replayed mutation: readiness is only
	// a full authority read, and all SQL/memory/wait invariants still equal A.
	assertUpdates(0)
	assertSQLA()
	if nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5AtomicDigestSQL) != before || nha029C5HistoryCounts(t, ctx, r) != counts {
		t.Fatal("read-only pool recovery changed sources/revisions/manifest/registry/audits or command intents")
	}
	if after, err := r.owner.CurrentConfig(ctx); err != nil || after != stamp || r.controller() != controller || !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) {
		t.Fatalf("read-only pool recovery changed A stamp/controller/original waits: %v", err)
	}
	if callback.Load() || len(r.driver.snapshot()) != 0 {
		t.Fatal("read-only pool recovery reached the writer or dispatched a command")
	}
	if nha029C5HistoryActive(t, ctx, r) != a {
		t.Fatal("fresh Store read after reconnect differs from independent SQL A")
	}
	published, err := r.owner.Activate(ctx, request)
	if err != nil || published.Outcome() != configactivation.Published || published.AttemptOutcome() != storage.TransactionCommitted || published.Revision().Number() <= a.revision.Number() || !callback.Load() {
		t.Fatalf("same candidate after reconnect failed real activation: outcome=%v transaction=%v err=%v", published.Outcome(), published.AttemptOutcome(), err)
	}
	assertUpdates(1)
	b := nha029C5HistoryActive(t, ctx, r)
	if b.revision != published.Revision() || b.manifest == a.manifest {
		t.Fatal("positive control did not publish a new complete SQL authority")
	}
	if receipt, present, err := r.store.ReadActivationAttempt(ctx, published.Attempt()); err != nil || !present || receipt.Target() != b.revision || receipt.ManifestSHA256() != b.manifest {
		t.Fatalf("positive control lacks its real committed target audit: present=%t err=%v", present, err)
	}
	var retainedCount int
	var retainedPID uint32
	if err := environment.observer.QueryRow(ctx, `SELECT count(*),COALESCE(min(pid),0) FROM pg_locks
WHERE locktype='advisory' AND classid=481316 AND objid=24 AND objsubid=2 AND granted`).Scan(&retainedCount, &retainedPID); err != nil || retainedCount != 1 || retainedPID != runtimePID {
		t.Fatalf("configuration SQL recovery replaced the singleton024 session: count=%d pid=%d err=%v", retainedCount, retainedPID, err)
	}
	if after, err := r.owner.CurrentConfig(ctx); err != nil || after == stamp || r.controller() != controller || !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) {
		t.Fatalf("positive publication replaced the intact A controller/waits or retained old stamp: %v", err)
	}
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("SQL recovery dispatched or replayed a command")
	}
	r.close(t)
}
