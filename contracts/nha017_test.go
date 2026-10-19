package contracts

import (
	"errors"
	"testing"
	"time"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testNHA017Wire(t *testing.T, previous *domain.ObservedState, quality domain.Quality, value *domain.StateValue, received time.Time, revision uint64) (*state.ObservedState, domain.ObservedState) {
	t.Helper()
	entity := testNHA014Entity(t)
	source, err := domain.NewSourceStamp("fake-driver-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := domain.NewStateReport(quality, value, &received, received, source, revision)
	if err != nil {
		t.Fatal(err)
	}
	observed, _, err := domain.ApplyStateReport(entity, previous, report)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := EncodeObservedState(observed)
	if err != nil {
		t.Fatal(err)
	}
	return wire, observed
}

func TestNHA017WireEqualReportAndUnknown(t *testing.T) {
	q, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	falseValue := domain.NewSwitchValue(false)
	w1, first := testNHA017Wire(t, nil, q, &falseValue, t0, 1)
	if _, ok := w1.Value.(*state.ObservedState_SwitchOn); !ok || w1.GetSwitchOn() {
		t.Fatal("explicit false switch lost on wire")
	}
	roundtrip, err := AdmitWireState(w1, nil, nil)
	if err != nil {
		t.Fatalf("first wire snapshot: %v", err)
	}
	w2, _ := testNHA017Wire(t, &first, q, &falseValue, t0.Add(time.Second), 2)
	second, err := AdmitWireState(w2, &roundtrip, nil)
	if err != nil {
		t.Fatalf("equal wire snapshot: %v", err)
	}
	_, _, times, _, _, _ := second.Components()
	if !times.LastChanged.Equal(t0) || !times.LastUpdated.Equal(t0) || !times.LastReported.Equal(t0.Add(time.Second)) {
		t.Fatalf("wire timestamps conflated: %+v", times)
	}
	estimatedStale, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceEstimated)
	w3, _ := testNHA017Wire(t, &second, estimatedStale, &falseValue, t0.Add(2*time.Second), 3)
	third, err := AdmitWireState(w3, &second, nil)
	if err != nil || w3.Quality.Freshness != state.Freshness_FRESHNESS_STALE || w3.Quality.Provenance != state.Provenance_PROVENANCE_ESTIMATED {
		t.Fatalf("estimated stale wire quality: %v", err)
	}
	if _, ok := third.CriticalValue(); ok {
		t.Fatal("estimated stale value available to critical decision")
	}
	unknown, _ := domain.NewQuality(domain.AvailabilityUnknown, domain.FreshnessStale, domain.ProvenanceObserved)
	w4, _ := testNHA017Wire(t, &third, unknown, nil, t0.Add(3*time.Second), 4)
	if w4.Value != nil {
		t.Fatal("unknown encoded as switch or numeric sentinel")
	}
	fourth, err := AdmitWireState(w4, &third, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := fourth.CurrentValue(); present {
		t.Fatal("unknown admitted with current value")
	}
	if _, present := fourth.DisplayValue(); !present {
		t.Fatal("display value lost after unknown report")
	}
}

func TestNHA017WireZeroMeasurementAdmission(t *testing.T) {
	catalog, entity := testNHA014Catalog(t)
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	m, err := domain.NewMeasurement(0, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(m)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	w, _ := testNHA017Wire(t, nil, q, &value, t0, 1)
	if w.GetMeasurement() == nil || w.GetMeasurement().Value != 0 {
		t.Fatal("zero measurement lost")
	}
	profile := &StateMeasurementAdmission{Catalog: catalog, Endpoint: "power"}
	admitted, err := AdmitWireState(w, nil, profile)
	if err != nil {
		t.Fatal(err)
	}
	gotEntity, _, _, _, _, _ := admitted.Components()
	if gotEntity != entity {
		t.Fatal("measurement entity changed")
	}
	if _, err := AdmitWireState(w, nil, nil); !errors.Is(err, ErrInvalidWireState) {
		t.Fatalf("measurement without capability metadata admitted: %v", err)
	}
	w.GetMeasurement().Unit = "kW"
	if _, err := AdmitWireState(w, nil, profile); err == nil {
		t.Fatal("unadvertised measurement unit admitted")
	}
}

func TestNHA017HistoricalAndCoalescedSnapshots(t *testing.T) {
	q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	falseValue := domain.NewSwitchValue(false)
	initial, _ := testNHA017Wire(t, nil, q, &falseValue, t0, 10)
	initial.LastChanged = timestamppb.New(t0.Add(-time.Hour))
	initial.LastUpdated = timestamppb.New(t0.Add(-time.Minute))
	parsed, err := AdmitWireState(initial, nil, nil)
	if err != nil {
		t.Fatalf("valid historical initial snapshot rejected: %v", err)
	}
	reencoded, err := EncodeObservedState(parsed)
	if err != nil || !proto.Equal(initial, reencoded) {
		t.Fatalf("historical clocks changed on roundtrip: %v", err)
	}
	trueValue := domain.NewSwitchValue(true)
	later, _ := testNHA017Wire(t, nil, q, &trueValue, t0.Add(10*time.Minute), 15)
	later.LastChanged = timestamppb.New(t0.Add(5 * time.Minute))
	later.LastUpdated = timestamppb.New(t0.Add(6 * time.Minute))
	coalesced, err := AdmitWireState(later, &parsed, nil)
	if err != nil {
		t.Fatalf("coalesced snapshot rejected: %v", err)
	}
	_, _, times, _, revision, err := coalesced.Components()
	if err != nil || revision != 15 || !times.LastChanged.Equal(t0.Add(5*time.Minute)) || !times.LastUpdated.Equal(t0.Add(6*time.Minute)) {
		t.Fatalf("coalesced clocks altered: %+v %v", times, err)
	}
	unknown, _ := domain.NewQuality(domain.AvailabilityUnknown, domain.FreshnessStale, domain.ProvenanceObserved)
	withoutValue, _ := testNHA017Wire(t, nil, unknown, nil, t0.Add(11*time.Minute), 16)
	last, err := AdmitWireState(withoutValue, &coalesced, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := last.CurrentValue(); present {
		t.Fatal("unknown snapshot has current value")
	}
	if display, present := last.DisplayValue(); !present {
		t.Fatal("last known value lost across snapshots")
	} else if on, switchValue := display.Switch(); !switchValue || !on {
		t.Fatal("last known value changed")
	}
}

func TestNHA017EncodeRejectsOutOfRangeTime(t *testing.T) {
	q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	outside := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	value := domain.NewSwitchValue(true)
	_, observed := testNHA017Wire(t, nil, q, &value, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), 1)
	entity, _, times, source, revision, err := observed.Components()
	if err != nil {
		t.Fatal(err)
	}
	times.ObservedAt = &outside
	outOfRange, err := domain.NewObservedStateSnapshot(entity, q, &value, times, source, revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeObservedState(outOfRange); !errors.Is(err, ErrInvalidWireState) {
		t.Fatalf("out-of-Protobuf-range time encoded: %v", err)
	}
}

func TestNHA017WireRejectMalformedAndFuture(t *testing.T) {
	q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	value := domain.NewSwitchValue(true)
	good, _ := testNHA017Wire(t, nil, q, &value, t0, 1)
	for name, mutate := range map[string]func(*state.ObservedState){
		"missing quality":     func(w *state.ObservedState) { w.Quality = nil },
		"future availability": func(w *state.ObservedState) { w.Quality.Availability = state.Availability(99) },
		"future freshness":    func(w *state.ObservedState) { w.Quality.Freshness = state.Freshness(99) },
		"future provenance":   func(w *state.ObservedState) { w.Quality.Provenance = state.Provenance(99) },
		"missing entity":      func(w *state.ObservedState) { w.Entity = nil },
		"alias entity": func(w *state.ObservedState) {
			w.Entity = &registry.EntityRef{Reference: &registry.EntityRef_HaAlias{HaAlias: "switch.test"}}
		},
		"missing value":     func(w *state.ObservedState) { w.Value = nil },
		"missing receipt":   func(w *state.ObservedState) { w.ReceivedAt = nil },
		"invalid timestamp": func(w *state.ObservedState) { w.ReceivedAt = &timestamppb.Timestamp{Nanos: 1000000000} },
		"missing changed":   func(w *state.ObservedState) { w.LastChanged = nil },
		"zero revision":     func(w *state.ObservedState) { w.Revision = 0 },
		"missing session":   func(w *state.ObservedState) { w.SourceSession = "" },
		"future value field": func(w *state.ObservedState) {
			w.Value = nil
			w.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := proto.Clone(good).(*state.ObservedState)
			mutate(candidate)
			if _, err := AdmitWireState(candidate, nil, nil); err == nil {
				t.Fatal("malformed or future state admitted")
			}
		})
	}
	if _, err := AdmitWireState(nil, nil, nil); !errors.Is(err, ErrInvalidWireState) {
		t.Fatalf("nil state admitted: %v", err)
	}
	if _, err := EncodeObservedState(domain.ObservedState{}); !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("zero domain state encoded: %v", err)
	}
}

func TestNHA017WireEnumCoverage(t *testing.T) {
	for _, item := range []struct {
		name       protoreflect.FullName
		descriptor protoreflect.EnumDescriptor
		expected   int
	}{
		{"home.state.v1.Availability", state.Availability(0).Descriptor(), 4},
		{"home.state.v1.Freshness", state.Freshness(0).Descriptor(), 3},
		{"home.state.v1.Provenance", state.Provenance(0).Descriptor(), 4},
	} {
		if item.descriptor.FullName() != item.name || item.descriptor.Values().Len() != item.expected {
			t.Fatalf("%s changed; review exhaustive state conversion", item.name)
		}
	}
	fields := (&state.ObservedState{}).ProtoReflect().Descriptor().Oneofs().ByName("value").Fields()
	if fields.Len() != 6 || fields.ByName("switch_on") == nil || fields.ByName("measurement") == nil ||
		fields.ByName("light") == nil || fields.ByName("fan") == nil || fields.ByName("climate") == nil || fields.ByName("climate").Number() != 15 ||
		fields.ByName("helper") == nil || fields.ByName("helper").Number() != 16 {
		t.Fatal("state value oneof changed; review admission and encoding")
	}
}
