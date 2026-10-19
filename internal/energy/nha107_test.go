package energy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/testpg"
)

// A configured counter with no source report still owns an observable energy
// entity. It must report missing source without inventing an initial 0 kWh.
func TestNHA107EnergyPublishesMissingSourceBeforeFirstPowerReport(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	rig := nha108NewRig(t, "W")
	outputSession, _, outputID := nha108Output(t, rig)
	rig.output = outputID
	if _, exists := rig.fixture.State(outputID); exists {
		t.Fatal("the output already had an observation before its pilot started")
	}

	profile := nha108Profile(t, rig, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	key, err := integrations.NewSourceKey("computed-energy", "counter")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewOutputBinding(profile, key)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewPilotFactory(rig.sequence, repository, []OutputBinding{binding}, nil, RealClock{Sequence: rig.sequence})
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory().(*Pilot)
	ctx, cancel := context.WithCancel(context.Background())
	tasks := &nha108PilotTasks{ctx: ctx, errs: make(chan error, 2)}
	if err := pilot.Start(ctx, outputSession, tasks); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := pilot.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		done := make(chan struct{})
		go func() { tasks.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("energy pilot tasks did not stop")
		}
		select {
		case err := <-tasks.errs:
			t.Errorf("energy pilot task: %v", err)
		default:
		}
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if state, exists := rig.fixture.State(outputID); exists {
			_, quality, _, _, _, err := state.Components()
			if err != nil {
				t.Fatal(err)
			}
			availability, freshness, provenance, err := quality.Components()
			if err != nil || availability == domain.AvailabilityAvailable || freshness != domain.FreshnessStale || provenance != domain.ProvenanceEstimated {
				t.Fatalf("missing source quality: %v/%v/%v, %v", availability, freshness, provenance, err)
			}
			if _, present := state.CurrentValue(); present {
				t.Fatal("missing source fabricated a current energy value")
			}
			if _, present := state.DisplayValue(); present {
				t.Fatal("new counter fabricated a historical energy total")
			}
			return
		}
		select {
		case err := <-tasks.errs:
			t.Fatalf("energy pilot stopped before publishing missing source: %v", err)
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("energy entity has no quality report before its first power observation")
		}
	}
}

func TestNHA107MissingSourcePreservesDurableTotalAndStartsNewInterval(t *testing.T) {
	rig := nha108NewRig(t, "W")
	profile := nha108Profile(t, rig, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	base := rig.clock.Now()
	state, err := MissingSource(newCheckpoint(profile), base)
	if err != nil {
		t.Fatal(err)
	}
	if state.pending.Kind() != PendingMissingSource || state.hasEnergy || state.Coverage() != CoverageContinuous {
		t.Fatal("initial absence gained a numeric total")
	}
	state = nha108Advance(t, profile, state, rig, rig.publish(t, 1000, &base, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 1))
	if !state.valid() || state.pending.Kind() != PendingMissingSource || state.pending.Revision() != state.Revision() {
		t.Fatal("first source sample did not renew explicit energy absence")
	}
	rig.clock.set(base.Add(time.Hour), time.Hour)
	oneHour := base.Add(time.Hour)
	state = nha108Advance(t, profile, state, rig, rig.publish(t, 1000, &oneHour, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 2))
	if state.pending.Kind() != PendingValue {
		t.Fatal("real integrated total has no numeric pending variant")
	}
	if state.Coverage() != CoverageContinuous || state.Diagnostic() != DiagnosticNone {
		t.Fatal("initial absence incorrectly degraded coverage after the first real interval")
	}
	beforeNum, beforeDen, err := state.TotalFraction()
	if err != nil || beforeNum != "1" || beforeDen != "1" {
		t.Fatalf("one kWh: %s/%s %v", beforeNum, beforeDen, err)
	}
	rig.clock.set(base.Add(65*time.Minute), 65*time.Minute)
	gap := rig.publish(t, 0, nil, domain.AvailabilityUnavailable, domain.ProvenanceObserved, 3)
	state = nha108Advance(t, profile, state, rig, gap)
	if state.pending.Kind() != PendingMissingSource || state.hasPrevious == true {
		t.Fatal("gap retained an integration bound")
	}
	observation, err := state.pending.Observation()
	if err != nil {
		t.Fatal(err)
	}
	_, _, current, _, _, _, _, err := observation.Components()
	if err != nil || current != nil {
		t.Fatal("missing source published a numeric old total")
	}
	encoded, err := encode(state)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	retainedNum, retainedDen, err := restored.TotalFraction()
	if err != nil || retainedNum != beforeNum || retainedDen != beforeDen {
		t.Fatal("restart lost the old accumulated total")
	}
	rig.clock.set(base.Add(80*time.Minute), 80*time.Minute)
	first := base.Add(80 * time.Minute)
	state = nha108Advance(t, profile, restored, rig, rig.publish(t, 1000, &first, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 4))
	if !state.valid() || state.pending.Kind() != PendingMissingSource || state.pending.Revision() != state.Revision() {
		t.Fatal("first recovered sample exposed old energy as current")
	}
	retainedNum, retainedDen, _ = state.TotalFraction()
	if retainedNum != beforeNum || retainedDen != beforeDen {
		t.Fatal("first recovered sample bridged the gap")
	}
	rig.clock.set(base.Add(100*time.Minute), 100*time.Minute)
	second := base.Add(100 * time.Minute)
	state = nha108Advance(t, profile, state, rig, rig.publish(t, 1000, &second, domain.AvailabilityAvailable, domain.ProvenanceEstimated, 5))
	num, den, err := state.TotalFraction()
	if err != nil {
		t.Fatal(err)
	}
	total, err := rational(num, den)
	if err != nil || total.Cmp(big.NewRat(4, 3)) != 0 {
		t.Fatalf("recovered interval: %v %v", total, err)
	}
}

func TestNHA107PendingWireDistinguishesLegacyAbsenceFromExplicitInvalidKind(t *testing.T) {
	rig := nha108NewRig(t, "W")
	profile := nha108Profile(t, rig, MethodTrapezoidal, TimeBasisReceipt, time.Hour, 0, 3)
	original := newCheckpoint(profile)
	fence, cursor, err := rig.sequence.SubscribeRecorder()
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Close()
	// A reset is a legitimate value-bearing zero and gives the old wire a
	// pending projection with no variant field.
	base, err := MissingSource(original, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	value, err := Reset(base, cursor, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	wire := checkpointToWire(value)
	wire.Pending.Kind = nil
	wire.CurrentKind = nil
	legacy, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decode(legacy)
	if err != nil || restored.pending.kind != PendingValue || restored.pending.quantized != 0 {
		t.Fatalf("legacy pending value: %v, %v", restored.pending.kind, err)
	}
	zero := PendingUnspecified
	wire.Pending.Kind = &zero
	invalid, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(legacy, invalid) || !errors.Is(func() error { _, e := decode(invalid); return e }(), ErrInvalidCheckpoint) {
		t.Fatal("explicit zero kind was confused with omitted legacy variant")
	}
	future := PendingKind(99)
	wire.Pending.Kind = &future
	invalid, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(invalid); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatal("unknown future kind admitted")
	}
	valueKind := PendingValue
	wire.Pending.Kind = &valueKind
	wire.CurrentKind = &zero
	invalid, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(invalid); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatal("explicit zero current kind was confused with omitted legacy field")
	}
	wire.CurrentKind = &future
	invalid, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(invalid); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatal("unknown future current kind admitted")
	}
}

// Exercise the ordinary Pilot path: every publication is acknowledged and
// reloaded from PostgreSQL before the next report. The durable current-kind
// must survive clearing the delivery slot.
func TestNHA107AcknowledgedMissingSourceKeepsCurrentEnergyAbsentUntilNewInterval(t *testing.T) {
	cluster := testpg.Start(t)
	store := nha108Store(t, cluster.Port)
	defer store.Close()
	rig := nha108NewRig(t, "W")
	outputSession, outputBinding, outputID := nha108Output(t, rig)
	rig.output = outputID
	profile := nha108Profile(t, rig, MethodTrapezoidal, TimeBasisCaptureRequired, time.Hour, 0, 3)
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pilot := &Pilot{repository: repository, epoch: rig.epoch}
	item := &counter{binding: outputBinding}
	item.state, err = repository.Load(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	base := rig.clock.Now()
	advance := func(after time.Duration, watts float64, available domain.Availability, sequence uint64) Checkpoint {
		t.Helper()
		rig.clock.set(base.Add(after), after)
		at := base.Add(after)
		var captured *time.Time
		if available == domain.AvailabilityAvailable {
			captured = &at
		}
		entry := rig.publish(t, watts, captured, available, domain.ProvenanceObserved, sequence)
		if err := pilot.report(ctx, outputSession, item, entry); err != nil {
			t.Fatalf("Pilot report %d: %v", sequence, err)
		}
		loaded, err := repository.Load(ctx, profile)
		if err != nil || loaded.hasPending || !loaded.valid() || loaded.revision != item.state.revision || loaded.currentKind != item.state.currentKind {
			t.Fatalf("report %d was not acknowledged durably: %+v, %v", sequence, loaded, err)
		}
		item.state = loaded
		return loaded
	}
	assertOutput := func(wantCurrent bool, wantDisplay float64) {
		t.Helper()
		state, exists := rig.fixture.State(outputID)
		if !exists {
			t.Fatal("Pilot did not publish the energy output")
		}
		_, quality, _, _, _, err := state.Components()
		if err != nil {
			t.Fatal(err)
		}
		availability, freshness, provenance, err := quality.Components()
		if err != nil || provenance != domain.ProvenanceEstimated {
			t.Fatalf("energy quality: %v, %v", quality, err)
		}
		if _, present := state.CurrentValue(); present != wantCurrent {
			t.Fatalf("current value presence = %t, want %t", present, wantCurrent)
		}
		if display, present := state.DisplayValue(); !present {
			t.Fatal("durable historical display total was lost")
		} else {
			measurement, ok := display.Measurement()
			if !ok {
				t.Fatal("display is not an energy measurement")
			}
			parts, err := measurement.Components()
			if err != nil || parts.Value != wantDisplay {
				t.Fatalf("display total changed: %+v, want %v, %v", parts, wantDisplay, err)
			}
		}
		if wantCurrent && (availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh) ||
			!wantCurrent && (availability != domain.AvailabilityUnavailable || freshness != domain.FreshnessStale) {
			t.Fatalf("current quality: %v/%v", availability, freshness)
		}
	}
	first := advance(0, 1000, domain.AvailabilityAvailable, 1)
	if first.hasEnergy || first.currentKind != PendingUnspecified {
		t.Fatal("first power bound fabricated energy")
	}
	one := advance(time.Hour, 1000, domain.AvailabilityAvailable, 2)
	if one.currentKind != PendingValue || one.totalNum != "1" || one.totalDen != "1" {
		t.Fatal("first actual interval did not produce one durable kWh")
	}
	assertOutput(true, 1)
	missing := advance(65*time.Minute, 0, domain.AvailabilityUnavailable, 3)
	if missing.currentKind != PendingMissingSource || missing.totalNum != "1" || missing.totalDen != "1" || missing.hasPrevious {
		t.Fatal("acknowledged source gap lost absence or changed total")
	}
	assertOutput(false, 1)
	recovered := advance(80*time.Minute, 1000, domain.AvailabilityAvailable, 4)
	if recovered.currentKind != PendingMissingSource || recovered.totalNum != "1" || recovered.totalDen != "1" || !recovered.hasPrevious {
		t.Fatal("first recovered point exposed old total or integrated the hole")
	}
	assertOutput(false, 1)
	resumed := advance(100*time.Minute, 1000, domain.AvailabilityAvailable, 5)
	if resumed.currentKind != PendingValue {
		t.Fatal("new real interval did not restore current energy")
	}
	total, err := rational(resumed.totalNum, resumed.totalDen)
	if err != nil || total.Cmp(big.NewRat(4, 3)) != 0 || resumed.generation != one.generation {
		t.Fatalf("resumed total/generation: %v, %v", total, err)
	}
	assertOutput(true, 1.333)
}
