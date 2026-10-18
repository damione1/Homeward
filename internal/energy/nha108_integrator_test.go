package energy

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha108Clock struct {
	mu   sync.Mutex
	wall time.Time
	tick time.Duration
}

func (c *nha108Clock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha108Clock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *nha108Clock) set(wall time.Time, tick time.Duration) {
	c.mu.Lock()
	c.wall = wall
	c.tick = tick
	c.mu.Unlock()
}

type nha108Rig struct {
	clock             *nha108Clock
	host              *integrations.Manager
	fixture           *fake.Fixture
	sequence          *sequencer.Sequencer
	session           *integrations.Session
	binding           integrations.Binding
	source, output    domain.EntityID
	sub               *sequencer.Subscription
	epoch, sourceUnit string
}

func nha108NewRig(t *testing.T, unitText string) *nha108Rig {
	t.Helper()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := &nha108Clock{wall: base}
	host, fixture, e := fake.NewFixtureWithClock(clock)
	if e != nil {
		t.Fatal(e)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha108-source")
	session, e := host.OpenSession(instance)
	if e != nil {
		t.Fatal(e)
	}
	key, _ := integrations.NewSourceKey("fake-device", "power")
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic power", Endpoint: "power", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: unitText, Precision: 0, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	candidate, e := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if e != nil {
		t.Fatal(e)
	}
	bindings, e := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if e != nil {
		t.Fatal(e)
	}
	_, source, e := bindings[0].IDs()
	if e != nil {
		t.Fatal(e)
	}
	output, _ := domain.ParseEntityID("nha108-derived-output")
	sub, cursor, e := fixture.Sequencer().SubscribeRecorder()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = sub.Close() })
	epoch, _, e := cursor.Components()
	if e != nil {
		t.Fatal(e)
	}
	return &nha108Rig{clock, host, fixture, fixture.Sequencer(), session, bindings[0], source, output, sub, epoch, unitText}
}
func (r *nha108Rig) publish(t *testing.T, value float64, observed *time.Time, availability domain.Availability, provenance domain.Provenance, seq uint64) sequencer.Entry {
	return r.publishWithSequence(t, value, observed, availability, provenance, &seq)
}
func (r *nha108Rig) publishNoSequence(t *testing.T, value float64, observed *time.Time, availability domain.Availability, provenance domain.Provenance) sequencer.Entry {
	return r.publishWithSequence(t, value, observed, availability, provenance, nil)
}
func (r *nha108Rig) publishWithSequence(t *testing.T, value float64, observed *time.Time, availability domain.Availability, provenance domain.Provenance, seq *uint64) sequencer.Entry {
	t.Helper()
	freshness := domain.FreshnessFresh
	if availability != domain.AvailabilityAvailable {
		freshness = domain.FreshnessStale
	}
	quality, e := domain.NewQuality(availability, freshness, provenance)
	if e != nil {
		t.Fatal(e)
	}
	var state *domain.StateValue
	if availability == domain.AvailabilityAvailable {
		unit, _ := domain.NewUnitCatalog().Parse(r.sourceUnit)
		measurement, e := domain.NewMeasurement(value, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
		if e != nil {
			t.Fatal(e)
		}
		v, e := domain.NewMeasurementValue(measurement)
		if e != nil {
			t.Fatal(e)
		}
		state = &v
	}
	observation, e := integrations.NewStateObservation("power", quality, state, observed, r.clock.Now(), seq)
	if e != nil {
		t.Fatal(e)
	}
	if e := r.session.Publish(context.Background(), r.binding, observation); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		delivery, e := r.sub.Next(ctx)
		if e != nil {
			t.Fatal(e)
		}
		entry, ok := delivery.Entry()
		if !ok {
			t.Fatal("missing admitted entry")
		}
		entity, observedState, _, isState := entry.State()
		if !isState || entity != r.source {
			continue
		}
		_, _, _, stamp, _, e := observedState.Components()
		if e != nil {
			t.Fatal(e)
		}
		_, number, e := stamp.Components()
		if e == nil && ((seq == nil && number == nil) || (seq != nil && number != nil && *number == *seq)) {
			return entry
		}
	}
}
func nha108Profile(t *testing.T, r *nha108Rig, method Method, basis TimeBasis, maxGap, tick time.Duration, precision int) Profile {
	t.Helper()
	unit, _ := domain.NewUnitCatalog().Parse("kWh")
	p, e := NewProfile("nha108-fixture-v1", 1, r.source, r.output, method, basis, unit, precision, domain.SemanticTotal, maxGap, tick)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func nha108Advance(t *testing.T, p Profile, c Checkpoint, r *nha108Rig, entry sequencer.Entry) Checkpoint {
	t.Helper()
	next, changed, e := AdvanceReport(p, c, entry, r.epoch)
	if e != nil || !changed {
		t.Fatalf("advance: changed=%t err=%v", changed, e)
	}
	return next
}

func TestNHA108TrapezoidExactGapDuplicateAndReset(t *testing.T) {
	r := nha108NewRig(t, "W")
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	at := r.clock.Now()
	first := r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1)
	c := nha108Advance(t, p, newCheckpoint(p), r, first)
	r.clock.set(at.Add(time.Hour), time.Hour)
	later := at.Add(time.Hour)
	second := r.publish(t, 1000, &later, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 2)
	c = nha108Advance(t, p, c, r, second)
	total, e := rational(c.totalNum, c.totalDen)
	if e != nil || total.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("1 kW-hour: %v %v", total, e)
	}
	if !c.hasPending || c.pending.quantized != 1000 || c.pending.freshness != domain.FreshnessFresh {
		t.Fatalf("projection: %+v", c.pending)
	}
	if _, changed, e := AdvanceReport(p, c, second, r.epoch); e != nil || changed {
		t.Fatalf("duplicate advanced: %t %v", changed, e)
	}
	r.clock.set(at.Add(65*time.Minute), 65*time.Minute)
	unavailable := r.publish(t, 0, nil, domain.AvailabilityUnavailable, domain.ProvenanceObserved, 3)
	c = nha108Advance(t, p, c, r, unavailable)
	if c.hasPrevious || c.coverage != CoverageDegraded || c.diagnostic != DiagnosticUnavailable {
		t.Fatalf("gap: %+v", c)
	}
	r.clock.set(at.Add(80*time.Minute), 80*time.Minute)
	afterGap := at.Add(80 * time.Minute)
	afterGapEntry := r.publish(t, 1000, &afterGap, domain.AvailabilityAvailable, domain.ProvenanceObserved, 4)
	c = nha108Advance(t, p, c, r, afterGapEntry)
	total, _ = rational(c.totalNum, c.totalDen)
	if total.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("gap invented energy: %v", total)
	}
	fence, _ := r.sequence.Capture()
	reset, e := Reset(c, fence.Cursor, at.Add(81*time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	if reset.generation != c.generation+1 || reset.pending.quantized != 0 {
		t.Fatalf("reset: %+v", reset)
	}
	if _, changed, e := AdvanceReport(p, reset, afterGapEntry, r.epoch); e != nil || changed {
		t.Fatalf("pre-reset replay advanced: %t %v", changed, e)
	}
}

func TestNHA108ReceiptUsesAdmissionTickAcrossWallJumpAndLeftLease(t *testing.T) {
	r := nha108NewRig(t, "W")
	base := r.clock.Now()
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisReceipt, time.Hour, 0, 6)
	c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1))
	r.clock.set(base.Add(10*time.Minute), time.Second)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2))
	total, _ := rational(c.totalNum, c.totalDen)
	if total.Cmp(big.NewRat(1, 3600)) != 0 {
		t.Fatalf("wall leap was integrated: %v", total)
	}
	r.clock.set(base.Add(-10*time.Minute), 2*time.Second)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 4))
	total, _ = rational(c.totalNum, c.totalDen)
	if total.Cmp(big.NewRat(1, 1800)) != 0 {
		t.Fatalf("backward wall leap changed monotone duration: %v", total)
	}
	left, e := HiloLeftV1(r.source, r.output, 10*time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	initial := newCheckpoint(left)
	r.clock.set(base.Add(20*time.Minute), 20*time.Minute)
	first := r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 5)
	initial = nha108Advance(t, left, initial, r, first)
	lease, e := NewLease(initial.lastReport, first.AdmissionTick(), first.AdmissionTick()+5*time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if _, changed, e := AdvanceTick(left, initial, lease, first.AdmissionTick()+time.Nanosecond); e != nil || changed {
		t.Fatalf("early timer advanced: %t %v", changed, e)
	}
	shortLease, e := NewLease(initial.lastReport, first.AdmissionTick(), first.AdmissionTick()+time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	earlyExpiry, changed, e := AdvanceTick(left, initial, shortLease, first.AdmissionTick()+time.Minute+time.Nanosecond)
	if e != nil || !changed || earlyExpiry.hasPrevious || earlyExpiry.diagnostic != DiagnosticLeaseExpired {
		t.Fatalf("lease shorter than tick remained fresh: %+v %t %v", earlyExpiry, changed, e)
	}
	advanced, changed, e := AdvanceTick(left, initial, lease, first.AdmissionTick()+120*time.Second)
	if e != nil || !changed {
		t.Fatalf("tick: %t %v", changed, e)
	}
	total, _ = rational(advanced.totalNum, advanced.totalDen)
	if total.Cmp(big.NewRat(1, 30)) != 0 {
		t.Fatalf("left 120s: %v", total)
	}
	if advanced.lastReport != initial.lastReport || advanced.pending.freshness != domain.FreshnessFresh {
		t.Fatal("tick changed source admission or freshness")
	}
	expired, changed, e := AdvanceTick(left, advanced, lease, first.AdmissionTick()+6*time.Minute)
	if e != nil || !changed || expired.hasPrevious || expired.diagnostic != DiagnosticLeaseExpired {
		t.Fatalf("expired lease integrated: %+v %t %v", expired, changed, e)
	}
}

func TestNHA108ReceiptRejectsNonAdvancingAdmissionTick(t *testing.T) {
	r := nha108NewRig(t, "W")
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisReceipt, time.Hour, 0, 3)
	base := r.clock.Now()
	c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1))
	r.clock.set(base.Add(time.Minute), 0)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2))
	if c.hasPrevious || c.hasEnergy || c.diagnostic != DiagnosticInvalidTime {
		t.Fatalf("same admission tick became a bound: %+v", c)
	}
	r.clock.set(base.Add(2*time.Minute), time.Second)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 3))
	if !c.hasPrevious || c.hasEnergy || c.coverage != CoverageDegraded {
		t.Fatalf("first valid post-gap report integrated: %+v", c)
	}
}

func TestNHA108RampWAndKWAndNoPerIntervalRounding(t *testing.T) {
	for _, input := range []struct {
		unit string
		top  float64
	}{{"W", 2000}, {"kW", 2}} {
		r := nha108NewRig(t, input.unit)
		p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
		at := r.clock.Now()
		c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 0, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1))
		r.clock.set(at.Add(time.Hour), time.Hour)
		later := at.Add(time.Hour)
		c = nha108Advance(t, p, c, r, r.publish(t, input.top, &later, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2))
		total, _ := rational(c.totalNum, c.totalDen)
		if total.Cmp(big.NewRat(1, 1)) != 0 {
			t.Fatalf("%s ramp: %v", input.unit, total)
		}
	}
	r := nha108NewRig(t, "W")
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Minute, 0, 3)
	base := r.clock.Now()
	c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 1, &base, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1))
	for i := 1; i <= 100; i++ {
		at := base.Add(time.Duration(i) * 36 * time.Second)
		r.clock.set(at, time.Duration(i)*36*time.Second)
		c = nha108Advance(t, p, c, r, r.publish(t, 1, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, uint64(i+1)))
	}
	total, _ := rational(c.totalNum, c.totalDen)
	if total.Cmp(big.NewRat(1, 1000)) != 0 || c.pending.quantized != 1 {
		t.Fatalf("small intervals rounded away: %v / %d", total, c.pending.quantized)
	}
}

func TestNHA108MissingCaptureNegativePowerAndSessionChangeDegrade(t *testing.T) {
	r := nha108NewRig(t, "W")
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	base := r.clock.Now()
	c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 1000, &base, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 1))
	r.clock.set(base.Add(10*time.Minute), 10*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2))
	if c.hasPrevious || c.diagnostic != DiagnosticInvalidTime || c.hasEnergy {
		t.Fatalf("non-advancing inherited capture became a bound: %+v", c)
	}
	r.clock.set(base.Add(11*time.Minute), 11*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 999, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 3))
	if c.hasPrevious || c.diagnostic != DiagnosticMissingCapture || c.hasEnergy {
		t.Fatalf("absent capture became a bound: %+v", c)
	}
	at := base.Add(20 * time.Minute)
	r.clock.set(at, 20*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 4))
	at = base.Add(30 * time.Minute)
	r.clock.set(at, 30*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, -10, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 5))
	if c.hasPrevious || c.diagnostic != DiagnosticNegativePower || c.hasEnergy {
		t.Fatalf("negative was clamped or integrated: %+v", c)
	}
	at = base.Add(40 * time.Minute)
	r.clock.set(at, 40*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 6))
	instance, _ := domain.ParseIntegrationInstanceID("nha108-source")
	newSession, err := r.host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("fake-device", "power")
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic power", Endpoint: "power", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: "W", Precision: 0, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := newSession.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	r.session = newSession
	r.binding = bindings[0]
	at = base.Add(50 * time.Minute)
	r.clock.set(at, 50*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 100))
	if c.diagnostic != DiagnosticSourceChanged || c.hasEnergy {
		t.Fatalf("new session bridged old interval: %+v", c)
	}
}

func TestNHA108InheritedCaptureCannotReanchorAcrossGapOrReload(t *testing.T) {
	r := nha108NewRig(t, "W")
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	base := r.clock.Now()
	c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 1000, &base, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1))
	r.clock.set(base.Add(10*time.Minute), 10*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2))
	if c.hasPrevious || c.diagnostic != DiagnosticInvalidTime || !c.captureWatermark.Equal(base) {
		t.Fatalf("first inherited capture: %+v", c)
	}
	encoded, err := encode(c)
	if err != nil {
		t.Fatal(err)
	}
	c, err = decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	r.clock.set(base.Add(20*time.Minute), 20*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, nil, domain.AvailabilityAvailable, domain.ProvenanceObserved, 3))
	if c.hasPrevious || c.diagnostic != DiagnosticInvalidTime || c.hasEnergy {
		t.Fatalf("reloaded inherited capture bridged gap: %+v", c)
	}
	at := base.Add(30 * time.Minute)
	r.clock.set(at, 30*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 4))
	if !c.hasPrevious || c.hasEnergy || !c.previous.at.Equal(at) || c.coverage != CoverageDegraded {
		t.Fatalf("new capture integrated gap: %+v", c)
	}
	at = base.Add(40 * time.Minute)
	r.clock.set(at, 40*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, &at, domain.AvailabilityAvailable, domain.ProvenanceObserved, 5))
	total, err := rational(c.totalNum, c.totalDen)
	if err != nil || total.Cmp(big.NewRat(1, 6)) != 0 {
		t.Fatalf("only post-gap ten minutes may contribute: %v %v", total, err)
	}
}

func TestNHA108ProfileIdentityRoundTripBoundary(t *testing.T) {
	source, _ := domain.ParseEntityID("source-open-id")
	output, _ := domain.ParseEntityID("output-open-id")
	unit, _ := domain.NewUnitCatalog().Parse("kWh")
	for _, name := range []string{"", string([]byte{0xff}), "has space"} {
		if _, err := NewProfile(name, 1, source, output, MethodTrapezoidal, TimeBasisReceipt, unit, 3, domain.SemanticTotal, time.Hour, 0); err == nil {
			t.Fatalf("invalid profile name %q admitted", name)
		}
	}
	if _, err := HiloLeftV1(source, output, 0); err == nil {
		t.Fatal("Hilo max gap silently defaulted")
	}
}

func TestNHA108SilenceBeyondExplicitMaxGapDoesNotBridge(t *testing.T) {
	r := nha108NewRig(t, "W")
	p := nha108Profile(t, r, MethodTrapezoidal, TimeBasisCaptureRequired, 5*time.Minute, 0, 3)
	base := r.clock.Now()
	c := nha108Advance(t, p, newCheckpoint(p), r, r.publish(t, 1000, &base, domain.AvailabilityAvailable, domain.ProvenanceObserved, 1))
	later := base.Add(15 * time.Minute)
	r.clock.set(later, 15*time.Minute)
	c = nha108Advance(t, p, c, r, r.publish(t, 1000, &later, domain.AvailabilityAvailable, domain.ProvenanceObserved, 2))
	if c.diagnostic != DiagnosticExcessiveGap || c.hasEnergy || !c.hasPrevious || c.coverage != CoverageDegraded {
		t.Fatalf("15-minute silence bridged: %+v", c)
	}
}

func TestNHA108HalfEvenProjectionAndBoundedRationals(t *testing.T) {
	source, _ := domain.ParseEntityID("source-open-id")
	output, _ := domain.ParseEntityID("output-open-id")
	unit, _ := domain.NewUnitCatalog().Parse("kWh")
	profile, err := NewProfile("nha108-round-v1", 1, source, output, MethodTrapezoidal, TimeBasisCaptureRequired, unit, 3, domain.SemanticTotal, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range []struct {
		total *big.Rat
		want  int64
	}{{big.NewRat(1, 2000), 0}, {big.NewRat(3, 2000), 2}, {big.NewRat(5, 2000), 2}} {
		got, err := quantize(profile, example.total)
		if err != nil || got != example.want {
			t.Fatalf("half-even %s: %d %v", example.total.RatString(), got, err)
		}
	}
	if _, err := quantize(profile, big.NewRat(1_000_000_000_000_001, 1)); err == nil {
		t.Fatal("unbounded total admitted")
	}
}
