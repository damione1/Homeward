package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	history "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historycoverage"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha164RegressionStore(t *testing.T) *storage.Store {
	t.Helper()
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

// Controlled context boundary: the first cause check is live; the following
// PrincipalFromContext Err check observes deadline expiry. Done closes at the
// same transition, so the oracle targets that exact early-return interleaving.
type nha164DuringCaptureDeadline struct {
	context.Context
	checks atomic.Int32
	once   sync.Once
	done   chan struct{}
}

func (c *nha164DuringCaptureDeadline) Done() <-chan struct{} { return c.done }
func (c *nha164DuringCaptureDeadline) Err() error {
	if c.checks.Add(1) < 2 {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.DeadlineExceeded
}
func TestNHA164DeadlineBetweenCaptureAndPrincipalKeepsItsCause(t *testing.T) {
	h, base, _, _ := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	selection, err := contracts.AdmitHistoryV2Selection(nha164TraceRequest().Msg)
	if err != nil {
		t.Fatal(err)
	}
	q, err := selection.NativeQuery(domain.EntityID{}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := &nha164DuringCaptureDeadline{Context: base, done: make(chan struct{})}
	_, err = h.native.capture(ctx, q, nil)
	if !errors.Is(err, context.DeadlineExceeded) || connect.CodeOf(historyFault(err).Connect()) != connect.CodeDeadlineExceeded {
		t.Fatalf("early principal failure hid deadline: %v", err)
	}
}

func TestNHA164FrozenCoverageClipsLateCoalescedRetention(t *testing.T) {
	store := nha164RegressionStore(t)
	ctx := context.Background()
	epochText := "00000000000000000000000000000172"
	epoch, err := storage.ParseRecorderEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-7 * 24 * time.Hour)
	if _, _, err = store.OpenRecorderEpoch(ctx, epoch, now); err != nil {
		t.Fatal(err)
	}
	initial, err := storage.NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &storage.RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PersistRecorderBatch(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.PurgeRecorderRaw(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("nha164-coalesced-retention")
	instance, _ := domain.ParseIntegrationInstanceID("nha164-retention")
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	source, _ := domain.NewSourceStamp("nha164-retention", nil)
	persist := func(seq uint64, at time.Time) {
		value := domain.NewSwitchValue(seq%2 == 0)
		observed, err := domain.NewObservedStateSnapshot(entity, quality, &value, domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, source, seq, nil)
		if err != nil {
			t.Fatal(err)
		}
		row, err := storage.NewRecorderRecord(seq, instance, observed, domain.Causality{}, now, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		batch, err := storage.NewRecorderBatch(epoch, seq-1, seq, []storage.RecorderRecord{row}, nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.PersistRecorderBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	persist(1, now)
	persist(2, now)
	persist(3, cutoff.Add(-time.Hour))
	q, err := historyquery.NewQuery(historyquery.Observations, "nha164-coalesced-retention", now.Add(-time.Hour), now.Add(time.Hour), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	live, err := storage.NewRecorderLiveHigh(epochText, 4)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ReadHistoricalRecorderPageAt(ctx, q, nil, &live, nha203RecordedScopeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.Next
	first.Release()
	if cursor == nil {
		t.Fatal("oracle requires first page high3/live4")
	}
	persist(4, cutoff.Add(-2*time.Hour))
	request, err := storage.NewRecorderCoverageRequest(epoch, 0, epoch, nil)
	if err != nil {
		t.Fatal(err)
	}
	native, err := store.ReadRecorderCoverage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	ranges := native.Retention()
	if len(ranges) != 1 {
		t.Fatalf("writer did not create coalesced retention: %+v", ranges)
	}
	from, through := ranges[0].Bounds()
	if from != 3 || through != 4 {
		t.Fatalf("oracle did not coalesce3..4: %d..%d", from, through)
	}
	second, err := store.ReadHistoricalRecorderPageAt(ctx, q, cursor, &live, nha203RecordedScopeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	coverage := publicHistoryCoverage(second.ProvenanceStartedAt)
	if len(second.Observations) != 1 || second.Observations[0].Sequence != 2 || second.Next != nil {
		t.Fatal("global retention changed frozen point prefix")
	}
	response := &history.QueryObservationsResponse{Scope: publicHistoryScope(q, second.RawCutoff, second.AsOf), Coverage: coverage}
	home, _ := apitypes.ParseHomeName("homes/nha164-fixture")
	if contracts.AdmitHistoryV2Response(response, home) != nil {
		t.Fatal("clipped frozen coverage did not produce an admitted incomplete response")
	}
}

func TestNHA164ReservedTracePageReleasesAfterCancellationAndRevocation(t *testing.T) {
	store := nha164RegressionStore(t)
	epoch, err := storage.ParseHistoryEpoch("00000000000000000000000000000170")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.OpenExecutionHistoryEpoch(context.Background(), epoch); err != nil {
		t.Fatal(err)
	}
	for _, revoke := range []bool{false, true} {
		handler, base, source, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
		for i := 0; i < 3; i++ {
			ctx, cancel := context.WithCancel(base)
			spy.traces = func(ctx context.Context, q storage.HistoryPageQuery) (storage.HistoryPage, error) {
				page, err := store.ReadExecutionHistoryPage(ctx, q)
				if err != nil {
					return page, err
				}
				if revoke {
					if err := handler.native.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_USER, uint64(2+i*2))); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				return page, nil
			}
			response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest())
			cancel()
			want := connect.CodeCanceled
			if revoke {
				want = connect.CodePermissionDenied
			}
			if response != nil || connect.CodeOf(err) != want {
				t.Fatalf("reserved page termination escaped: revoke=%v iteration=%d err=%v", revoke, i, err)
			}
			if revoke {
				if err := handler.native.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, uint64(3+i*2))); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Three acquired real reservations followed by a successful acquisition
		// distinguishes release from silently consuming the two shared slots.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		page, err := store.ReadExecutionHistoryPage(ctx, storage.HistoryPageQuery{Epoch: epoch, Since: time.Now().UTC().Add(-time.Hour), Limit: 1, ClipRetention: true, HoldReservation: true})
		page.Release()
		cancel()
		if err != nil {
			t.Fatalf("reader slots leaked after termination: %v", err)
		}
	}
	held := make([]storage.HistoryPage, 0, 2)
	for i := 0; i < 2; i++ {
		page, err := store.ReadExecutionHistoryPage(context.Background(), storage.HistoryPageQuery{Epoch: epoch, Since: time.Now().UTC().Add(-time.Hour), Limit: 1, ClipRetention: true, HoldReservation: true})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, page)
		defer page.Release()
	}
	handler, base, _, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	spy.traces = store.ReadExecutionHistoryPage
	ctx, cancel := context.WithTimeout(base, 20*time.Millisecond)
	defer cancel()
	response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest())
	if response != nil || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("saturated shared reader gate became a denial: %v", err)
	}
}

func TestNHA164DeadlineIsDistinctFromRightsRevocation(t *testing.T) {
	handler, base, _, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	spy.traces = func(ctx context.Context, _ storage.HistoryPageQuery) (storage.HistoryPage, error) {
		<-ctx.Done()
		return storage.HistoryPage{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(base, 20*time.Millisecond)
	defer cancel()
	response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest())
	if response != nil || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("deadline turned into a rights decision: %v", err)
	}
	response, err = handler.QueryExecutionTraces(ctx, nha164TraceRequest())
	if response != nil || connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("already expired request turned into denial: %v", err)
	}
}

// The existing Recorder Writer seam holds a real admitted batch before its
// Store commit. It supplies neither a synthetic sequence nor a second authority.
type nha164HeldRecorderWriter struct {
	*storage.Store
	once             sync.Once
	entered, unblock chan struct{}
}

func (w *nha164HeldRecorderWriter) PersistRecorderBatch(ctx context.Context, b storage.RecorderBatch) (storage.TransactionOutcome, error) {
	if b.Through() > 0 {
		w.once.Do(func() { close(w.entered) })
		select {
		case <-w.unblock:
		case <-ctx.Done():
			return storage.TransactionNotCommitted, ctx.Err()
		}
	}
	return w.Store.PersistRecorderBatch(ctx, b)
}

func TestNHA164AdmittedRecorderQueueIsUnprovenUntilCommit(t *testing.T) {
	store := nha164RegressionStore(t)
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	writer := &nha164HeldRecorderWriter{Store: store, entered: make(chan struct{}), unblock: make(chan struct{})}
	recording, err := recorder.Start(context.Background(), fixture.Sequencer(), writer, recorder.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	unblock := func() { release.Do(func() { close(writer.unblock) }) }
	defer unblock()
	defer func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := recording.Stop(ctx); err != nil {
			t.Error(err)
		}
	}()
	instance, _ := domain.ParseIntegrationInstanceID("nha164-tail")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	driver, err := fake.NewDriver(at)
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("switch.nha164_tail")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Publish(context.Background(), binding, observation); err != nil {
		t.Fatal(err)
	}

	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("real Recorder did not hold an admitted batch")
	}
	capture, err := fixture.Sequencer().CaptureEntityCatalogue(entity, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	epochText, sequence, err := capture.Cursor().Components()
	if err != nil || sequence == 0 {
		t.Fatalf("no actual admission cursor: %v", err)
	}
	high, err := storage.NewRecorderLiveHigh(epochText, sequence)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := entity.Value()
	q, err := historyquery.NewQuery(historyquery.Observations, name, at.Add(-time.Hour), at.Add(time.Hour), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadHistoricalRecorderPageAt(context.Background(), q, nil, &high, nha203RecordedScopeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	coverage := publicHistoryCoverage(before.ProvenanceStartedAt)
	before.Release()
	if len(before.Observations) != 0 || coverage.Verdict != history.HistoryCoverageVerdict_HISTORY_COVERAGE_VERDICT_NOT_PROVEN || !nha203NativeTail(nha203NativeCoverage(t, store, epochText, sequence), 0, sequence) {
		t.Fatal("native pending tail lost or public completeness invented")
	}
	progress := &history.QueryObservationsResponse{Scope: publicHistoryScope(q, before.RawCutoff, before.AsOf), Coverage: coverage, NextPageToken: "bounded-physical-progress"}
	home, _ := apitypes.ParseHomeName("homes/nha164-fixture")
	if contracts.AdmitHistoryV2Response(progress, home) != nil {
		t.Fatal("raw empty progression/tail was rejected by the wire contract")
	}
	if err = host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = recording.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadHistoricalRecorderPageAt(context.Background(), q, nil, &high, nha203RecordedScopeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	coverage = publicHistoryCoverage(after.ProvenanceStartedAt)
	after.Release()
	diagnostic, err := historycoverage.FromRecorderSnapshot(nha203NativeCoverage(t, store, epochText, sequence), nil)
	if err != nil || len(after.Observations) == 0 || diagnostic.Verdict() != historycoverage.CompleteUnderPolicy || coverage.Verdict != history.HistoryCoverageVerdict_HISTORY_COVERAGE_VERDICT_NOT_PROVEN {
		t.Fatalf("native commit/public confidentiality: %v", err)
	}
	wrong, err := storage.NewRecorderLiveHigh("00000000000000000000000000000171", sequence)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadHistoricalRecorderPageAt(context.Background(), q, nil, &wrong, nha203RecordedScopeFixture{})
	page.Release()
	if err == nil {
		t.Fatal("a live sequence was attached to another epoch")
	}
}

func nha203NativeTail(snapshot storage.RecorderCoverageSnapshot, from, through uint64) bool {
	diagnostic, err := historycoverage.FromRecorderSnapshot(snapshot, nil)
	if err != nil {
		return false
	}
	for _, issue := range diagnostic.Issues() {
		if issue.Kind() == historycoverage.UncommittedTail {
			a, b := issue.Bounds()
			if a.Sequence() == from && b.Sequence() == through {
				return true
			}
		}
	}
	return false
}

func nha203NativeCoverage(t *testing.T, store *storage.Store, epochText string, sequence uint64) storage.RecorderCoverageSnapshot {
	t.Helper()
	epoch, err := storage.ParseRecorderEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	request, err := storage.NewRecorderCoverageRequest(epoch, 0, epoch, &sequence)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadRecorderCoverage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
