package sequencer_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

func nha018Instance(t *testing.T, name string) domain.IntegrationInstanceID {
	t.Helper()
	id, err := domain.ParseIntegrationInstanceID(name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nha018Cause(t *testing.T, number int, trace string) domain.Causality {
	t.Helper()
	cause, err := domain.ParseCausationID(fmt.Sprintf("observation-%d", number))
	if err != nil {
		t.Fatal(err)
	}
	correlation, err := domain.ParseCorrelationID(trace)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := domain.NewCausality(&cause, &correlation)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func nha018Observation(t *testing.T, number int, trace string) integrations.StateObservation {
	t.Helper()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(number%2 == 0)
	observed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(number) * time.Second)
	seq := uint64(number)
	observation, err := integrations.NewStateObservation("relay", quality, &value, &observed, observed, &seq)
	if err != nil {
		t.Fatal(err)
	}
	observation, err = observation.WithCausality(nha018Cause(t, number, trace))
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func TestNHA018TenThousandConcurrentMutationsAndAtomicCaptures(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	type publisher struct {
		name     string
		session  *integrations.Session
		binding  integrations.Binding
		entity   domain.EntityID
		baseline uint64
	}
	var publishers [2]publisher
	for i, name := range []string{"fake-a", "fake-b"} {
		session, err := host.OpenSession(nha018Instance(t, name))
		if err != nil {
			t.Fatal(err)
		}
		driver, err := fake.NewDriver(time.Date(2026, 10, 18, 12, i, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := driver.Start(ctx, session); err != nil {
			t.Fatal(err)
		}
		binding, err := driver.Binding()
		if err != nil {
			t.Fatal(err)
		}
		_, entity, err := binding.IDs()
		if err != nil {
			t.Fatal(err)
		}
		state, ok := fixture.State(entity)
		if !ok {
			t.Fatal("initial fake state missing")
		}
		_, _, _, _, revision, _ := state.Components()
		publishers[i] = publisher{name: name, session: session, binding: binding, entity: entity, baseline: revision}
	}
	start, err := fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	var captures []sequencer.Snapshot
	var captureMu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for driverIndex := range publishers {
		p := publishers[driverIndex]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 5000; n++ {
				observation := nha018Observation(t, driverIndex*5000+n, p.name)
				if err := p.session.Publish(ctx, p.binding, observation); err != nil {
					errCh <- err
					return
				}
				if n%100 == 0 {
					snapshot, err := fixture.Sequencer().Capture()
					if err != nil {
						errCh <- err
						return
					}
					captureMu.Lock()
					captures = append(captures, snapshot)
					captureMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	final, err := fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	entries, cursor, err := fixture.Sequencer().ReadAfter(start.Cursor, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10000 || cursor != final.Cursor {
		t.Fatalf("journal count/cursor: %d %+v %+v", len(entries), cursor, final.Cursor)
	}
	startEpoch, startSeq, _ := start.Cursor.Components()
	var counts [2]uint64
	for i, entry := range entries {
		epoch, seq, err := entry.Cursor().Components()
		if err != nil || epoch != startEpoch || seq != startSeq+uint64(i)+1 {
			t.Fatalf("non-monotone position %d: %s %d %v", i, epoch, seq, err)
		}
		entity, state, _, ok := entry.State()
		if !ok || entry.Kind() != sequencer.KindState {
			t.Fatalf("entry %d is not state", i)
		}
		instance, source, causal, present := entry.Origin()
		if !present {
			t.Fatal("state origin absent")
		}
		index := -1
		for j, p := range publishers {
			if p.entity == entity {
				index = j
				break
			}
		}
		if index < 0 {
			t.Fatalf("entry %d has unknown entity", i)
		}
		counts[index]++
		if instance != nha018Instance(t, publishers[index].name) {
			t.Fatalf("entry %d lost instance", i)
		}
		_, sourceSeq, err := source.Components()
		if err != nil || sourceSeq == nil {
			t.Fatalf("entry %d lost source sequence", i)
		}
		cause, correlation, err := causal.Components()
		if err != nil || cause == nil || correlation == nil {
			t.Fatalf("entry %d lost causes", i)
		}
		causeText, _ := cause.Value()
		trace, _ := correlation.Value()
		if causeText != fmt.Sprintf("observation-%d", *sourceSeq) || trace != publishers[index].name {
			t.Fatalf("entry %d mixed causes: %s %s %d", i, causeText, trace, *sourceSeq)
		}
		_, _, _, stateSource, revision, err := state.Components()
		if err != nil || revision != publishers[index].baseline+counts[index] || stateSource != source {
			t.Fatalf("entry %d mixed projection/revision", i)
		}
	}
	if counts != [2]uint64{5000, 5000} {
		t.Fatalf("two fake counts: %v", counts)
	}
	for _, snapshot := range captures {
		_, seq, _ := snapshot.Cursor.Components()
		var expected [2]uint64
		for _, entry := range entries[:seq-startSeq] {
			entity, _, _, _ := entry.State()
			if entity == publishers[0].entity {
				expected[0]++
			} else {
				expected[1]++
			}
		}
		for i, p := range publishers {
			state := snapshot.States[p.entity]
			_, _, _, _, revision, err := state.Components()
			if err != nil || revision != p.baseline+expected[i] {
				t.Fatalf("partial snapshot at %d: entity %d revision %d expected %d err %v", seq, i, revision, p.baseline+expected[i], err)
			}
		}
	}
}

type nha018Clock struct {
	mu    sync.Mutex
	times []time.Time
	next  int
}

func (c *nha018Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	at := c.times[c.next]
	c.next++
	return at
}

func TestNHA018ReconnectClockCausesAndCursorErrors(t *testing.T) {
	first := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := &nha018Clock{times: []time.Time{first, first.Add(-time.Hour), first.Add(-2 * time.Hour), first.Add(-3 * time.Hour), first.Add(-4 * time.Hour), first.Add(-5 * time.Hour)}}
	host, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance := nha018Instance(t, "reconnect")
	old, _ := host.OpenSession(instance)
	driver, _ := fake.NewDriver(first.Add(24 * time.Hour))
	if err := driver.Start(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	binding, _ := driver.Binding()
	_, entity, _ := binding.IDs()
	before, _ := fixture.Sequencer().Capture()
	newSession, _ := host.OpenSession(instance)
	observation := nha018Observation(t, 0, "reconnect")
	if err := old.Publish(context.Background(), binding, observation); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("stale session accepted: %v", err)
	}
	if err := newSession.Publish(context.Background(), binding, observation); !errors.Is(err, integrations.ErrForeignBinding) {
		t.Fatalf("stale binding accepted: %v", err)
	}
	newDriver, _ := fake.NewDriver(first.Add(-24 * time.Hour))
	if err := newDriver.Start(context.Background(), newSession); err != nil {
		t.Fatal(err)
	}
	after, _ := fixture.Sequencer().Capture()
	if _, oldSeq, _ := before.Cursor.Components(); oldSeq+5 != func() uint64 { _, n, _ := after.Cursor.Components(); return n }() {
		t.Fatal("reconnect omitted session-loss state/health or current driver's three reports")
	}
	state := after.States[entity]
	_, _, times, _, revision, err := state.Components()
	if err != nil || revision != 3 || !times.LastReported.Equal(first.Add(-5*time.Hour)) || times.ObservedAt == nil || !times.ObservedAt.Equal(first.Add(-24*time.Hour)) {
		t.Fatalf("admission/source clocks mixed: %+v rev=%d %v", times, revision, err)
	}
	if _, _, err := fixture.Sequencer().ReadAfter(sequencer.Cursor{}, 1); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("zero cursor admitted: %v", err)
	}
	other, _ := sequencer.New(sequencer.RealClock{}, 2)
	foreign, _ := other.Start()
	if _, _, err := fixture.Sequencer().ReadAfter(foreign, 1); !errors.Is(err, sequencer.ErrEpochMismatch) {
		t.Fatalf("foreign epoch admitted: %v", err)
	}
	if _, _, err := fixture.Sequencer().ReadAfter(before.Cursor, 0); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("zero read limit admitted: %v", err)
	}
	if _, err := domain.ParseCausationID(""); !errors.Is(err, domain.ErrInvalidCausality) {
		t.Fatalf("zero cause admitted: %v", err)
	}
	if _, err := domain.ParseCorrelationID(""); !errors.Is(err, domain.ErrInvalidCausality) {
		t.Fatalf("zero correlation admitted: %v", err)
	}
	if reflect.TypeFor[domain.CausationID]().ConvertibleTo(reflect.TypeFor[domain.CorrelationID]()) || reflect.TypeFor[domain.CausationID]().ConvertibleTo(reflect.TypeFor[domain.DeviceID]()) {
		t.Fatal("causal identities can be confused by Go conversion")
	}
	if reflect.TypeFor[domain.CorrelationID]().ConvertibleTo(reflect.TypeFor[domain.ActionID]()) || reflect.TypeFor[domain.CorrelationID]().AssignableTo(reflect.TypeFor[domain.CausationID]()) {
		t.Fatal("correlation identity can be confused with another ID")
	}
	initial, _, err := fixture.Sequencer().ReadAfter(func() sequencer.Cursor { c, _ := fixture.Sequencer().Start(); return c }(), 3)
	if err != nil || len(initial) != 3 {
		t.Fatalf("initial fake journal: %d %v", len(initial), err)
	}
	_, initialSource, _, present := initial[0].Origin()
	if !present {
		t.Fatal("state origin absent")
	}
	_, sourceSeq, _ := initialSource.Components()
	if sourceSeq != nil {
		t.Fatal("absent source sequence was confused with zero")
	}
	if !initial[0].ReceivedAt().Equal(first) {
		t.Fatal("initial receipt did not use injected clock")
	}
}

func TestNHA018EventHealthCausalityAndSourceZero(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha018Instance(t, "causal-fake")
	session, _ := host.OpenSession(instance)
	driver, _ := fake.NewDriver(time.Now())
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, _ := driver.Binding()
	before, _ := fixture.Sequencer().Capture()
	zero := uint64(0)
	event, _ := integrations.NewDeviceEvent("fake.edge", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), &zero)
	causal := nha018Cause(t, 42, "trace-edge")
	event, err = event.WithCausality(causal)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Emit(ctx, binding, event); err != nil {
		t.Fatal(err)
	}
	health, _ := integrations.NewHealthReport(integrations.HealthDegraded, "simulated")
	health, err = health.WithCausality(causal)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ReportHealth(ctx, health); err != nil {
		t.Fatal(err)
	}
	entries, _, err := fixture.Sequencer().ReadAfter(before.Cursor, 2)
	if err != nil || len(entries) != 2 {
		t.Fatalf("causal entries: %d %v", len(entries), err)
	}
	if entries[0].Kind() != sequencer.KindEvent || entries[1].Kind() != sequencer.KindHealth {
		t.Fatal("event/health kinds lost")
	}
	for _, entry := range entries {
		gotInstance, source, gotCausal, present := entry.Origin()
		if !present {
			t.Fatal("event/health origin absent")
		}
		if gotInstance != instance || gotCausal != causal || entry.ReceivedAt().IsZero() {
			t.Fatal("event/health origin or receipt lost")
		}
		if _, _, err := source.Components(); err != nil {
			t.Fatal(err)
		}
	}
	_, recorded, ok := entries[0].Event()
	if !ok {
		t.Fatal("event payload lost")
	}
	_, _, sourceSeq, _ := recorded.Components()
	if sourceSeq == nil || *sourceSeq != 0 {
		t.Fatal("observed zero source sequence became absent")
	}
	fixtureEvents, err := fixture.Events()
	if err != nil || len(fixtureEvents) != 2 {
		t.Fatalf("fixture event helper: %d %v", len(fixtureEvents), err)
	}
	last := fixtureEvents[1]
	if last.Instance != instance || last.Causality != causal || last.Event != recorded {
		t.Fatal("fixture event helper lost admitted metadata")
	}
	_, helperSeq, err := last.Source.Components()
	if err != nil || helperSeq == nil || *helperSeq != 0 {
		t.Fatal("fixture event helper lost zero source sequence")
	}
	if status, detail, _ := func() (integrations.HealthStatus, string, error) {
		report, _ := entries[1].Health()
		return report.Components()
	}(); status != integrations.HealthDegraded || detail != "simulated" {
		t.Fatal("health payload lost")
	}
}

func TestNHA018DirectSinkValidationAndBoundedJournal(t *testing.T) {
	ctx := context.Background()
	sequence, err := sequencer.New(sequencer.RealClock{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:direct")
	instance := nha018Instance(t, "direct")
	if err := sequence.AcceptState(ctx, entity, integrations.AcceptedState{}); err == nil {
		t.Fatal("zero accepted state admitted")
	}
	if err := sequence.AcceptEvent(ctx, entity, integrations.AcceptedEvent{}); err == nil {
		t.Fatal("zero accepted event admitted")
	}
	health, _ := integrations.NewHealthReport(integrations.HealthReady, "")
	if err := sequence.AcceptHealth(ctx, instance, health); err == nil {
		t.Fatal("health without Host session admitted")
	}
	start, _ := sequence.Start()
	snapshot, _ := sequence.Capture()
	if _, seq, _ := snapshot.Cursor.Components(); seq != 0 {
		t.Fatal("rejected input consumed sequence")
	}
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	host, err := integrations.NewHost(fixture, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := host.OpenSession(instance)
	driver, _ := fake.NewDriver(time.Now())
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sequence.ReadAfter(start, 2); !errors.Is(err, sequencer.ErrCursorExpired) {
		t.Fatalf("expired cursor silently read: %v", err)
	}
	snapshot, _ = sequence.Capture()
	if len(snapshot.States) != 1 || len(snapshot.Health) != 1 {
		t.Fatal("bounded journal lost current projections")
	}
	if _, err := sequencer.New(nil, 2); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("nil clock admitted: %v", err)
	}
	var nilClock *nha018Clock
	if _, err := sequencer.New(nilClock, 2); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("typed nil clock admitted: %v", err)
	}
}
