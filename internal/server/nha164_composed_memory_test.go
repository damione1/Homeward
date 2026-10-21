package server

// Authored against source05 and the source06 request-wide reservation fix.
// Qualification status belongs to the recorded runs, including fixture failures.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"github.com/damione1/Homeward/gen/home/history/v1/historyv1connect"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha164EncodedOwnerBarrier struct {
	entered chan int
	release chan struct{}
}

// The existing Connect Codec seam stops *after* the real proto encoder has
// allocated its result. Both the generated response graph and its byte buffer
// remain live until the test releases them. Request decoding stays unchanged.
type nha164ComposedCodec struct {
	barrier atomic.Pointer[nha164EncodedOwnerBarrier]
	fail    atomic.Bool
}

func (*nha164ComposedCodec) Name() string { return "proto" }
func (c *nha164ComposedCodec) Marshal(value any) ([]byte, error) {
	message, ok := value.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("unexpected fixture message %T", value)
	}
	encoded, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	if c.fail.Swap(false) {
		return nil, fmt.Errorf("synthetic encoding failure")
	}
	if _, ok := message.(*history.QueryObservationsResponse); ok {
		if barrier := c.barrier.Load(); barrier != nil {
			barrier.entered <- len(encoded)
			<-barrier.release
		}
	}
	runtime.KeepAlive(message)
	return encoded, nil
}
func (*nha164ComposedCodec) Unmarshal(encoded []byte, value any) error {
	message, ok := value.(proto.Message)
	if !ok {
		return fmt.Errorf("unexpected fixture request %T", value)
	}
	return proto.Unmarshal(encoded, message)
}

type nha164ComposedFixture struct {
	api                historyv1connect.HistoryServiceClient
	jsonAPI            historyv1connect.HistoryServiceClient
	request            *history.QueryObservationsRequest
	codec              *nha164ComposedCodec
	configCalls        atomic.Int64
	selectedPoints     int
	selectedInputBytes int
	pointCharge        int
	catalogueCharge    int
}

func nha164ComposedHistoryFixture(t *testing.T, points int, escaped ...bool) *nha164ComposedFixture {
	t.Helper()
	store := nha164RegressionStore(t)
	h, _, _, _ := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	account, _ := contracts.ParseAccountID("nha164-account")
	source, err := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha164-account\ngrants:\n  - scope: {family: history, operation: read}\n    objects:\n      entities: {all: {capabilities: {all: {}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, 2)); err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha164-composed")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	driver, err := fake.NewDriver(now)
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
	alias, _ := domain.ParseHAAlias("switch.nha164_composed")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	name, _ := entity.Value()

	// The integration candidate and registry projection admit at most 32
	// descriptions per entity. The authority's MaxSlots bounds a different list.
	// Exercise all 32 admitted descriptions, retaining the full 1 MiB catalogue
	// charge with short fan options and the scalar padding calculated below.
	const catalogueSlots = 32
	descriptions := make([]*registry.CapabilityDescription, catalogueSlots)
	for i := range descriptions {
		descriptions[i] = &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic", Endpoint: fmt.Sprintf("slot-%02d", i), Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
	}
	options := make([]string, 30000)
	for i := range options {
		options[i] = fmt.Sprintf("s%05d", i)
	}
	descriptions[0] = &registry.CapabilityDescription{SchemaId: contracts.FanSchema, Major: 1, DisplayName: "Synthetic fan", Endpoint: "fan", Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SetOn: true, SpeedOptions: options}}}
	base, err := sequencer.EstimateTriggerRegistryBytes(record)
	if err != nil {
		t.Fatal(err)
	}
	charge := base + 256 + len(name)
	for _, description := range descriptions {
		charge += 4*proto.Size(description) + 128
	}
	if charge >= 1<<20 {
		t.Fatalf("fixture catalogue already over admitted budget: %d", charge)
	}
	descriptions[0].DisplayName += strings.Repeat("c", ((1<<20)-charge-128)/4)
	candidate, err := integrations.NewCandidate(key, descriptions)
	if err != nil {
		t.Fatalf("catalogue fixture candidate: slots=%d initialCharge=%d: %v", len(descriptions), charge, err)
	}
	if _, err = session.Discover(context.Background(), []integrations.Candidate{candidate}); err != nil {
		t.Fatalf("catalogue fixture discovery: slots=%d initialCharge=%d: %v", len(descriptions), charge, err)
	}
	capture, err := fixture.Sequencer().CaptureEntityCatalogue(entity, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if capture.EstimatedBytes() < ((1 << 20) * 99 / 100) {
		t.Fatalf("catalogue stress charge is not near its unchanged limit: %d", capture.EstimatedBytes())
	}
	epochText, _, err := capture.Cursor().Components()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := storage.ParseRecorderEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := store.OpenRecorderEpoch(context.Background(), epoch, now); err != nil || outcome != storage.TransactionCommitted {
		t.Fatalf("epoch: %v %v", outcome, err)
	}
	initial, err := storage.NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &storage.RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(context.Background(), initial); err != nil || outcome != storage.TransactionCommitted {
		t.Fatalf("initial image: %v %v", outcome, err)
	}

	// Admission fixtures and SQL seeding precede allocation measurement. All
	// metadata are legitimate Recorder inputs; no direct SQL edits or digest
	// bypass. The page spans the full admitted seven-day request and size100.
	units := domain.NewUnitCatalog()
	unitSymbol := strings.Repeat("u", 300)
	if err = units.Register(unitSymbol, domain.DimensionPower, 0.01, 0); err != nil {
		t.Fatal(err)
	}
	unit, err := units.Parse(unitSymbol)
	if err != nil {
		t.Fatal(err)
	}
	// Register preserves the admitted symbol as its own canonical spelling.
	// Converting from W changes the numeric scale, not that historical identity.
	canonicalSymbol, err := unit.CanonicalSymbol()
	if err != nil {
		t.Fatal(err)
	}
	baseUnit, err := units.Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(0, baseUnit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err = measurement.Convert(unit)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceEstimated)
	if err != nil {
		t.Fatal(err)
	}
	at := now.Add(-time.Hour)
	makeRow := func(seq uint64, sourceLength, instanceLength int) storage.RecorderRecord {
		sourceText := strings.Repeat("s", sourceLength)
		if len(escaped) > 0 && escaped[0] {
			sourceText = strings.Repeat("\x01", sourceLength)
		}
		source, err := domain.NewSourceStamp(sourceText, nil)
		if err != nil {
			t.Fatal(err)
		}
		state, err := domain.NewObservedStateSnapshot(entity, quality, &value, domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, source, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		historicalInstance, err := domain.ParseIntegrationInstanceID(strings.Repeat("i", instanceLength))
		if err != nil {
			t.Fatal(err)
		}
		row, err := storage.NewRecorderRecord(seq, historicalInstance, state, domain.Causality{}, at, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	row := makeRow(1, 1, 1)
	sourceLength := historyquery.MaxPointBytes/points - (row.EncodedBytes() + len(name) + len(unitSymbol) + len(canonicalSymbol) + 512) + 1
	if sourceLength < 1 {
		t.Fatal("fixture cannot retain the real page limit")
	}
	row = makeRow(1, sourceLength, 1)
	for points*(row.EncodedBytes()+len(name)+len(unitSymbol)+len(canonicalSymbol)+512) > historyquery.MaxPointBytes {
		sourceLength--
		if sourceLength < 1 {
			t.Fatal("protobuf varint overhead exhausted fixture source")
		}
		row = makeRow(1, sourceLength, 1)
	}
	// The exact reader preflight charges payload, entity, both historical unit
	// spellings and512. A custom registered unit has a 300-byte canonical spelling
	// too; assuming the original W understates each point by299 bytes.
	pointCharge := points * (row.EncodedBytes() + len(name) + len(unitSymbol) + len(canonicalSymbol) + 512)
	if pointCharge > historyquery.MaxPointBytes || pointCharge < historyquery.MaxPointBytes*99/100 {
		t.Fatalf("point charge is not near the unchanged page bound: %d", pointCharge)
	}
	instanceLength := (4<<20)/points - row.EncodedBytes() - sourceLength - 8192/points
	if instanceLength < 1 {
		t.Fatal("fixture hidden metadata budget invalid")
	}
	for seq := uint64(1); seq <= uint64(points+1); seq++ {
		row = makeRow(seq, sourceLength, instanceLength)
		batch, err := storage.NewRecorderBatch(epoch, seq-1, seq, []storage.RecorderRecord{row}, nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := store.PersistRecorderBatch(context.Background(), batch); err != nil || outcome != storage.TransactionCommitted {
			t.Fatalf("seed point: %v %v", outcome, err)
		}
	}
	selectedInput := points * (row.EncodedBytes() + sourceLength + instanceLength)
	if selectedInput > 4<<20 || selectedInput < (4<<20)*99/100 {
		t.Fatalf("hidden SQL input is not near unchanged4MiB bound: %d", selectedInput)
	}
	// Exercise the complete admitted 512-gap evidence cardinality as well.
	from := uint64(points + 1)
	for n := 0; n < storage.RecorderCoverageMaxGaps; n += 32 {
		gaps := make([]storage.RecorderGap, 32)
		for j := range gaps {
			one := uint64(1)
			sequence := from + uint64(j) + 1
			gaps[j] = storage.RecorderGap{From: sequence, Through: sequence, KnownLost: &one, Reason: storage.RecorderGapOverflow}
		}
		batch, err := storage.NewRecorderBatch(epoch, from, from+32, nil, gaps, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := store.PersistRecorderBatch(context.Background(), batch); err != nil || outcome != storage.TransactionCommitted {
			t.Fatalf("seed coverage: %v %v", outcome, err)
		}
		from += 32
	}
	f := &nha164ComposedFixture{codec: &nha164ComposedCodec{}, selectedPoints: points, selectedInputBytes: selectedInput, pointCharge: pointCharge, catalogueCharge: capture.EstimatedBytes()}
	stamp, err := view.NewConfigStamp(1, [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	h.runtime.sequence = fixture.Sequencer()
	h.runtime.reader = store
	h.runtime.currentConfig = func(ctx context.Context) (view.ConfigStamp, error) {
		f.configCalls.Add(1)
		if err := ctx.Err(); err != nil {
			return view.ConfigStamp{}, err
		}
		return stamp, nil
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorization.NewScopeSet([]authorization.Permission{permission})
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := contracts.ParseMcpKeyID("nha164-composed-fixture")
	principal, err := authorization.ScopedPrincipal(account, credential, scopes)
	if err != nil {
		t.Fatal(err)
	}
	provider := nha164TLSCredential{principal: principal, authority: h.runtime.authority}
	plan, err := NewPublicPlan(provider, provider, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	if err = registerHistoryRoutes(plan, h.runtime); err != nil {
		t.Fatal(err)
	}
	// Test-only codec injection through the existing generated handler build.
	// PublicPlan's real routes, TLS admission, authority and Store stay in use.
	for i := range plan.connect {
		build := plan.connect[i].build
		plan.connect[i].build = func(options ...connect.HandlerOption) (string, http.Handler) {
			return build(append(options, connect.WithCodec(f.codec))...)
		}
	}
	public := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, public)
	if err != nil {
		t.Fatal(err)
	}
	tls := httptest.NewUnstartedServer(compiled.handler(public, newPublicAdmissions()))
	tls.EnableHTTP2 = true
	tls.StartTLS()
	t.Cleanup(tls.Close)
	f.api = historyv1connect.NewHistoryServiceClient(tls.Client(), tls.URL)
	f.jsonAPI = historyv1connect.NewHistoryServiceClient(tls.Client(), tls.URL, connect.WithProtoJSON())
	f.request = &history.QueryObservationsRequest{EntityId: name, Period: &history.Period{Start: timestamppb.New(now.Add(-6 * 24 * time.Hour)), End: timestamppb.New(now.Add(24 * time.Hour))}, Resolution: history.Resolution_RESOLUTION_RAW, PageSize: proto.Uint32(historyquery.MaxPoints)}
	// Warm TLS and pgx statement caches before the controlled interval.
	response, err := f.api.QueryObservations(context.Background(), nha164TLSRequest(proto.Clone(f.request).(*history.QueryObservationsRequest)))
	if err != nil {
		code := history.ErrorCode_ERROR_CODE_UNSPECIFIED
		if failure, ok := err.(*connect.Error); ok {
			for _, detail := range failure.Details() {
				value, detailErr := detail.Value()
				if typed, ok := value.(*history.HistoryError); detailErr == nil && ok {
					code = typed.Code
				}
			}
		}
		t.Fatalf("composed warm read: points=%d pointCharge=%d selectedSQLInput=%d catalogueCharge=%d historyCode=%v: %v", points, pointCharge, selectedInput, capture.EstimatedBytes(), code, err)
	}
	f.check(t, response.Msg)
	return f
}

func (f *nha164ComposedFixture) check(t *testing.T, response *history.QueryObservationsResponse) {
	t.Helper()
	if contracts.AdmitHistoryResponse(response) != nil {
		t.Fatal("composed wire response failed admission")
	}
	page := response.GetPage()
	if page == nil || len(page.Points) != f.selectedPoints || page.Continuation.GetNextPageToken() == "" || len(page.Coverage.Logical.Gaps) != storage.RecorderCoverageMaxGaps {
		t.Fatalf("stress charge/continuation/evidence was silently reduced: points=%d", len(page.GetPoints()))
	}
	for _, point := range page.Points {
		if point.State.GetMeasurement() == nil || point.State.GetMeasurement().Value != 0 || point.Descriptor_ == nil || len(point.Descriptor_.Unit) != 300 || point.Descriptor_.CanonicalUnit != point.Descriptor_.Unit || point.Descriptor_.Precision != -2 {
			t.Fatal("historical zero/descriptor was rewritten")
		}
	}
}

type nha164ComposedResult struct {
	response *connect.Response[history.QueryObservationsResponse]
	err      error
}

func (f *nha164ComposedFixture) start(ctx context.Context) <-chan nha164ComposedResult {
	done := make(chan nha164ComposedResult, 1)
	go func() {
		response, err := f.api.QueryObservations(ctx, nha164TLSRequest(proto.Clone(f.request).(*history.QueryObservationsRequest)))
		done <- nha164ComposedResult{response: response, err: err}
	}()
	return done
}
func nha164AwaitEncoded(t *testing.T, b *nha164EncodedOwnerBarrier) {
	t.Helper()
	select {
	case size := <-b.entered:
		if size > historyquery.MaxResponseBytes || size == 0 {
			t.Fatalf("encoded response outside admitted bound: %d", size)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("composed server did not reach encoded response barrier")
	}
}

func TestNHA164ComposedHistoryOwnerAllocations(t *testing.T) {
	// Two independent shapes cover maximal point cardinality and a single large
	// admitted point. Page size100,7days,4MiB input,512gaps and catalogue1MiB
	// keep their production values; the test changes none of those limits.
	for _, points := range []int{1, historyquery.MaxPoints} {
		t.Run(fmt.Sprintf("selected-points-%d", points), func(t *testing.T) {
			f := nha164ComposedHistoryFixture(t, points)
			for _, owners := range []int{1, historyquery.MaxReaderReservations} {
				t.Run(fmt.Sprintf("owners-%d", owners), func(t *testing.T) {
					barrier := &nha164EncodedOwnerBarrier{entered: make(chan int, owners), release: make(chan struct{})}
					f.codec.barrier.Store(barrier)
					released := false
					defer func() {
						if !released {
							close(barrier.release)
						}
						f.codec.barrier.Store(nil)
					}()
					runtime.GC()
					previousGC := debug.SetGCPercent(-1)
					defer debug.SetGCPercent(previousGC)
					var before, encoded, joined runtime.MemStats
					runtime.ReadMemStats(&before)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					done := make([]<-chan nha164ComposedResult, owners)
					for i := range done {
						done[i] = f.start(ctx)
					}
					for range owners {
						nha164AwaitEncoded(t, barrier)
					}
					runtime.ReadMemStats(&encoded)
					close(barrier.release)
					released = true
					responses := make([]*history.QueryObservationsResponse, owners)
					for i, result := range done {
						select {
						case result := <-result:
							if result.err != nil {
								t.Fatal(result.err)
							}
							responses[i] = result.response.Msg
							f.check(t, responses[i])
						case <-ctx.Done():
							t.Fatal("composed request did not join", ctx.Err())
						}
					}
					runtime.ReadMemStats(&joined)
					allocation := joined.TotalAlloc - before.TotalAlloc
					liveEncoded := encoded.HeapAlloc - before.HeapAlloc
					ceiling := uint64(owners * historyquery.ReaderReservationBytes)
					t.Logf("owners=%d pointCharge=%d selectedSQLInput=%d catalogueCharge=%d allocatedIncludingTLSClient=%d heapDeltaAtServerEncodedBarrier=%d ceiling=%d", owners, f.pointCharge, f.selectedInputBytes, f.catalogueCharge, allocation, liveEncoded, ceiling)
					if allocation > ceiling || liveEncoded > ceiling {
						t.Fatalf("composed allocation fixture exceeds owner allowance: allocation=%d live=%d ceiling=%d", allocation, liveEncoded, ceiling)
					}
					// Graphs and transport-decoded copies stay live through the measurement.
					runtime.KeepAlive(responses)
					runtime.KeepAlive(f)
				})
			}
		})
	}
}

func TestNHA164OwnerReservationIncludesCaptureAndConnectEncoding(t *testing.T) {
	// This is a discriminating regression: source05 releases its Store slot
	// before the Codec barrier, so a third request performs four more captures.
	// With one request-wide reservation, it must wait before currentConfig or
	// catalogue copying until an encoded owner has finished.
	f := nha164ComposedHistoryFixture(t, historyquery.MaxPoints)
	barrier := &nha164EncodedOwnerBarrier{entered: make(chan int, 3), release: make(chan struct{})}
	f.codec.barrier.Store(barrier)
	var release sync.Once
	unblock := func() { release.Do(func() { close(barrier.release) }) }
	defer func() { unblock(); f.codec.barrier.Store(nil) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, second := f.start(ctx), f.start(ctx)
	nha164AwaitEncoded(t, barrier)
	nha164AwaitEncoded(t, barrier)
	captures := f.configCalls.Load()
	third := f.start(ctx)
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	violation := ""
	select {
	case <-barrier.entered:
		violation = "third encoded owner bypassed two request-wide reservations"
	case <-timer.C:
	}
	if got := f.configCalls.Load(); got != captures {
		violation = fmt.Sprintf("third request allocated catalogue/config work before acquiring ownership: captures %d -> %d", captures, got)
	}
	// Release every blocked codec before joining any client, including a failed
	// regression on source05. The outer defer handles failure-path release.
	f.codec.barrier.Store(nil)
	unblock()
	for _, result := range []<-chan nha164ComposedResult{first, second, third} {
		select {
		case result := <-result:
			if result.err != nil {
				t.Fatal(result.err)
			}
			f.check(t, result.response.Msg)
		case <-ctx.Done():
			t.Fatal("requests failed to join", ctx.Err())
		}
	}
	if violation != "" {
		t.Fatal(violation)
	}
}

func TestNHA164JSONEncodingKeepsNominalAndBoundsEscapedResponses(t *testing.T) {
	f := nha164ComposedHistoryFixture(t, 1, true)
	nominal := proto.Clone(f.request).(*history.QueryObservationsRequest)
	end := time.Now().UTC().Add(-8 * 24 * time.Hour)
	nominal.Period = &history.Period{Start: timestamppb.New(end.Add(-24 * time.Hour)), End: timestamppb.New(end)}
	response, err := f.jsonAPI.QueryObservations(context.Background(), nha164TLSRequest(nominal))
	if err != nil || response.Msg.GetNotRetained() == nil || contracts.AdmitHistoryResponse(response.Msg) != nil {
		t.Fatalf("nominal JSON route changed: %v", err)
	}
	// Full Store/capture/conversion plus real protojson escaping. GC suppression
	// makes TotalAlloc a conservative bound including dead temporary JSON
	// buffers, TLS/HTTP2 work and client decoding, rather than RSS evidence.
	runtime.GC()
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	done := make(chan nha164ComposedResult, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range historyquery.MaxReaderReservations {
		go func() {
			response, err := f.jsonAPI.QueryObservations(ctx, nha164TLSRequest(proto.Clone(f.request).(*history.QueryObservationsRequest)))
			done <- nha164ComposedResult{response: response, err: err}
		}()
	}
	results := make([]*history.QueryObservationsResponse, 0, 2)
	for range historyquery.MaxReaderReservations {
		select {
		case result := <-done:
			if result.err != nil {
				t.Fatalf("JSON budget was misclassified by Connect: %v", result.err)
			}
			budget := result.response.Msg.GetBudget()
			if budget == nil || budget.Reason != history.BudgetReason_BUDGET_REASON_CAPACITY || contracts.AdmitHistoryResponse(result.response.Msg) != nil {
				t.Fatal("escaped oversized JSON was emitted or lost explicit Budget")
			}
			results = append(results, result.response.Msg)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	runtime.ReadMemStats(&after)
	allocation := after.TotalAlloc - before.TotalAlloc
	ceiling := uint64(historyquery.MaxReaderReservations * historyquery.ReaderReservationBytes)
	t.Logf("two composed JSON reads including escape temporary buffers: allocated=%d ceiling=%d", allocation, ceiling)
	if allocation > ceiling {
		t.Fatalf("JSON checking allocations exceeded two owners: %d", allocation)
	}
	runtime.KeepAlive(results)
	runtime.KeepAlive(f)
}

func TestNHA164HTTPReservationReturnsAfterEncodingErrorAndCancellation(t *testing.T) {
	f := nha164ComposedHistoryFixture(t, historyquery.MaxPoints)
	for _, mode := range []string{"encoding-error", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "encoding-error" {
				f.codec.fail.Store(true)
				if response, err := f.api.QueryObservations(context.Background(), nha164TLSRequest(proto.Clone(f.request).(*history.QueryObservationsRequest))); response != nil || err == nil {
					t.Fatal("synthetic encoder did not fail")
				}
			} else {
				held := &nha164EncodedOwnerBarrier{entered: make(chan int, 1), release: make(chan struct{})}
				f.codec.barrier.Store(held)
				var once sync.Once
				unblock := func() { once.Do(func() { close(held.release) }) }
				defer unblock()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := f.start(ctx)
				nha164AwaitEncoded(t, held)
				cancel()
				f.codec.barrier.Store(nil)
				unblock()
				select {
				case result := <-done:
					if result.err == nil {
						t.Fatal("cancelled request returned data")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("cancelled client failed to join")
				}
			}
			held := &nha164EncodedOwnerBarrier{entered: make(chan int, 2), release: make(chan struct{})}
			f.codec.barrier.Store(held)
			var once sync.Once
			unblock := func() { once.Do(func() { close(held.release) }) }
			defer func() { unblock(); f.codec.barrier.Store(nil) }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a, b := f.start(ctx), f.start(ctx)
			nha164AwaitEncoded(t, held)
			nha164AwaitEncoded(t, held)
			f.codec.barrier.Store(nil)
			unblock()
			for _, done := range []<-chan nha164ComposedResult{a, b} {
				select {
				case result := <-done:
					if result.err != nil {
						t.Fatal("reservation was not returned", result.err)
					}
					f.check(t, result.response.Msg)
				case <-ctx.Done():
					t.Fatal("healthy requests did not join", ctx.Err())
				}
			}
		})
	}
}
