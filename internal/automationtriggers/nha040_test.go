package automationtriggers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha040Resolver struct{ entity domain.EntityID }

func (r nha040Resolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) { return r.entity, nil }
func (r nha040Resolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalidPlan
}
func (r nha040Resolver) ResolveCalendar(domain.HAAlias) (CalendarSourceID, error) {
	return ParseCalendarSourceID("fixture-calendar")
}

type nha040PairResolver struct {
	entities map[domain.HAAlias]domain.EntityID
}

func (r nha040PairResolver) ResolveEntity(alias domain.HAAlias) (domain.EntityID, error) {
	entity, ok := r.entities[alias]
	if !ok {
		return domain.EntityID{}, ErrInvalidPlan
	}
	return entity, nil
}
func (nha040PairResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, ErrInvalidPlan
}
func (nha040PairResolver) ResolveCalendar(domain.HAAlias) (CalendarSourceID, error) {
	return CalendarSourceID{}, ErrInvalidPlan
}

func nha040Automation(t *testing.T, triggerYAML string) ir.Automation {
	t.Helper()
	switchEntity, err := ir.NewEntity("switch.fixture", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	calendarEntity, err := ir.NewEntity("calendar.fixture", ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{switchEntity, calendarEntity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := "id: fixture\ntriggers:\n" + triggerYAML + "\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n"
	file, err := parser.NewSourceFile("fixture.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileSources("automation.fixture", "fixture.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(result.Accepted()) != 1 {
		t.Fatalf("compile: %v %+v", err, result.Rejected())
	}
	return result.Accepted()[0]
}

type nha040Clock struct {
	mu     sync.Mutex
	wall   time.Time
	mono   time.Duration
	timers []*nha040Timer
}
type nha040Timer struct {
	owner    *nha040Clock
	deadline time.Duration
	ch       chan time.Time
	stopped  bool
}

func (c *nha040Clock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha040Clock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono }
func (c *nha040Clock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &nha040Timer{owner: c, deadline: c.mono + d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	if d <= 0 {
		t.ch <- c.wall
	}
	return t
}
func (t *nha040Timer) C() <-chan time.Time { return t.ch }
func (t *nha040Timer) Stop() bool {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}
func (c *nha040Clock) Advance(wall, elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(wall)
	c.mono += elapsed
	for _, timer := range c.timers {
		if !timer.stopped && timer.deadline <= c.mono {
			select {
			case timer.ch <- c.wall:
			default:
			}
		}
	}
}

func nha040Wait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not reached")
}

func TestNHA040ForInterruptedAt29Seconds(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	host, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("fixture")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
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
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: state\n  entity_id: switch.fixture\n  to: 'on'\n  for: '00:00:30'")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	publish := func(on bool) {
		t.Helper()
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		value := domain.NewSwitchValue(on)
		at := clock.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(time.Second, time.Second)
	publish(true)
	nha040Wait(t, func() bool { service.mu.Lock(); defer service.mu.Unlock(); return len(service.pending) == 1 })
	clock.Advance(29*time.Second, 29*time.Second)
	publish(false)
	nha040Wait(t, func() bool { service.mu.Lock(); defer service.mu.Unlock(); return len(service.pending) == 0 })
	clock.Advance(time.Second, time.Second)
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if result, err := service.NextResult(checkCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("interrupted for fired: result=%+v error=%v", result, err)
	}
}

func nha040SwitchState(t *testing.T, clock *nha040Clock, entity domain.EntityID, on bool, revision uint64) domain.ObservedState {
	t.Helper()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stamp, _ := domain.NewSourceStamp("fixture", nil)
	value := domain.NewSwitchValue(on)
	at := clock.Now()
	state, err := domain.NewObservedStateSnapshot(entity, quality, &value,
		domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, stamp, revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestNHA040StateFromAndToPredicates(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:relay")
	for _, test := range []struct {
		name, trigger                                string
		firstOld, firstNew, matchingOld, matchingNew bool
	}{
		{"from-only", "from: 'on'", false, true, true, false},
		{"from-to", "from: 'off'\n  to: 'on'", true, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t,
				"- trigger: state\n  entity_id: switch.fixture\n  "+test.trigger)},
				Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(plan, fixture.Sequencer(), clock)
			if err != nil {
				t.Fatal(err)
			}
			service.processStateLocked(0, nha040SwitchState(t, clock, entity, test.firstOld, 1),
				nha040SwitchState(t, clock, entity, test.firstNew, 2), sequencer.Entry{})
			if len(service.results) != 0 {
				t.Fatal("nonmatching from/to transition fired")
			}
			service.processStateLocked(0, nha040SwitchState(t, clock, entity, test.matchingOld, 3),
				nha040SwitchState(t, clock, entity, test.matchingNew, 4), sequencer.Entry{})
			if len(service.results) != 1 || service.results[0].Kind() != ResultFired {
				t.Fatalf("matching from/to transition did not fire once: %+v", service.results)
			}
		})
	}
}

func TestNHA040ForRecaptureDropsOldDeadline(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:relay")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t,
		"- trigger: state\n  entity_id: switch.fixture\n  to: 'on'\n  for: '00:00:30'")},
		Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	service.ctx, service.cancel = context.WithCancel(context.Background())
	defer service.Close()
	snapshot, err := fixture.Sequencer().CaptureTriggerView(MaxEngineCharge / 2)
	if err != nil {
		t.Fatal(err)
	}
	service.installSnapshotLocked(snapshot)
	oldSub, _, _, _, err := fixture.Sequencer().SubscribeTriggerViewAfter(snapshot.Cursor, maxReplayEntries, MaxEngineCharge/2)
	if err != nil {
		t.Fatal(err)
	}
	service.sub = oldSub
	service.processStateLocked(0, nha040SwitchState(t, clock, entity, false, 1),
		nha040SwitchState(t, clock, entity, true, 2), sequencer.Entry{})
	if len(service.pending) != 1 {
		t.Fatal("for did not arm before recapture")
	}
	firstGeneration := service.generation
	if err := service.resynchronize(oldSub); err != nil {
		t.Fatal(err)
	}
	if service.generation != firstGeneration+1 || len(service.pending) != 0 {
		t.Fatalf("recapture retained the old for: generation=%d pending=%d", service.generation, len(service.pending))
	}
	clock.Advance(31*time.Second, 31*time.Second)
	service.runDueLocked()
	for _, result := range service.results {
		if result.Kind() == ResultFired {
			t.Fatal("recapture replayed a previous for as Fire")
		}
	}
}

func TestNHA040ForDoesNotSurviveServiceRestart(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	host, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("fixture")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
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
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t,
		"- trigger: state\n  entity_id: switch.fixture\n  to: 'on'\n  for: '00:00:30'")},
		Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	value := domain.NewSwitchValue(true)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := clock.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	nha040Wait(t, func() bool { first.mu.Lock(); defer first.mu.Unlock(); return len(first.pending) == 1 })
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	clock.Advance(31*time.Second, 31*time.Second)
	second.mu.Lock()
	pending, fired := len(second.pending), len(second.results)
	second.mu.Unlock()
	if pending != 0 || fired != 0 {
		t.Fatalf("restart rearmed or replayed an old for: pending=%d results=%d", pending, fired)
	}
}

func TestNHA040CapacitySuspendsWithoutReplay(t *testing.T) {
	for _, limit := range []string{"fired-count", "output-bytes", "engine-charge"} {
		t.Run(limit, func(t *testing.T) {
			clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
			_, fixture, err := fake.NewFixtureWithClock(clock)
			if err != nil {
				t.Fatal(err)
			}
			entity, _ := domain.ParseEntityID("Host:unused")
			plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t,
				"- trigger: time_pattern\n  seconds: '*'")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(plan, fixture.Sequencer(), clock)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			service.scheduleAllLocked()
			switch limit {
			case "fired-count":
				for i := 0; i <= MaxFiredQueue; i++ {
					service.fireLocked(0, sequencer.Cursor{}, domain.EntityID{}, nil)
				}
				if service.firedQueued != MaxFiredQueue {
					t.Fatalf("fired queue held %d instead of %d", service.firedQueued, MaxFiredQueue)
				}
			case "output-bytes":
				payload := make([]byte, 64<<10)
				for i := 0; i < MaxFiredQueue && !service.suspended; i++ {
					service.fireLocked(0, sequencer.Cursor{}, domain.EntityID{}, payload)
				}
				if service.firedQueued == 0 || service.firedQueued >= MaxFiredQueue || service.resultBytes > MaxOutputBytes {
					t.Fatalf("output-byte limit was bypassed: fired=%d bytes=%d", service.firedQueued, service.resultBytes)
				}
			case "engine-charge":
				// A precharged trigger projection can leave less than one result's
				// admitted size. The engine must reject before copying its payload.
				service.projectionBytes = MaxEngineCharge - sequencer.MaxViewQueueBytes - runtimeFixedCharge - plan.charge - 256 - 80
				service.fireLocked(0, sequencer.Cursor{}, domain.EntityID{}, []byte("no-copy"))
				if service.firedQueued != 0 {
					t.Fatal("engine-charge overflow retained a fired result")
				}
			}
			if !service.suspended || service.suspendReason != DegradedCapacity || len(service.results) != service.firedQueued+1 ||
				service.results[len(service.results)-1].Kind() != ResultDegraded || len(service.scheduled) != 0 {
				t.Fatalf("capacity crossing did not suspend and cancel scheduling: suspended=%v queued=%d results=%d timers=%d",
					service.suspended, service.firedQueued, len(service.results), len(service.scheduled))
			}
			before := len(service.results)
			clock.Advance(time.Minute, time.Minute)
			service.runDueLocked()
			service.scheduleAllLocked()
			if len(service.results) != before || len(service.scheduled) != 0 {
				t.Fatal("capacity suspension replayed an old wakeup")
			}
		})
	}
}

func TestNHA040DSTGapFoldAndWallRollback(t *testing.T) {
	location, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	gap := nha040Automation(t, "- trigger: time\n  at: '02:30:00'").Triggers()[0]
	now := time.Date(2026, 3, 8, 0, 0, 0, 0, location)
	next, ok := nextCivilTime(now, location, gap, "")
	if !ok || next.In(location).Day() != 9 {
		t.Fatalf("spring gap became an occurrence: %v %t", next, ok)
	}
	fold := nha040Automation(t, "- trigger: time\n  at: '01:30:00'").Triggers()[0]
	now = time.Date(2026, 11, 1, 0, 0, 0, 0, location)
	first, ok := nextCivilTime(now, location, fold, "")
	if !ok {
		t.Fatal("fold first occurrence missing")
	}
	key := civilKey(first, location)
	rolledBack := first.Add(-time.Hour)
	next, ok = nextCivilTime(rolledBack, location, fold, key)
	if !ok || next.In(location).Day() != 2 {
		t.Fatalf("fold or rollback repeated civil occurrence: %v %t", next, ok)
	}
}

func TestNHA040TimerLoopDoesNotRefireFoldAfterWallRollback(t *testing.T) {
	location, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, location)
	clock := &nha040Clock{wall: start}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t,
		"- trigger: time\n  at: '01:30:00'")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	nha040Wait(t, func() bool { clock.mu.Lock(); defer clock.mu.Unlock(); return len(clock.timers) != 0 })
	first, ok := nextCivilTime(start, location, plan.rules[0].trigger, "")
	if !ok {
		t.Fatal("first fold wakeup absent")
	}
	delta := first.Sub(start)
	clock.Advance(delta, delta)
	nha040Wait(t, func() bool { service.mu.Lock(); defer service.mu.Unlock(); return len(service.results) == 1 })
	service.mu.Lock()
	if service.results[0].Kind() != ResultFired {
		service.mu.Unlock()
		t.Fatal("first fold wakeup was not fired")
	}
	service.mu.Unlock()
	clock.Advance(-time.Hour, time.Second)
	clock.Advance(2*time.Hour, 2*time.Hour)
	time.Sleep(30 * time.Millisecond)
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.results) != 1 || service.civil[0] != civilKey(first, location) {
		t.Fatalf("timer loop repeated a civil fold wakeup after rollback: results=%d highWater=%q", len(service.results), service.civil[0])
	}
}

func TestNHA040TimePatternOneSecondBeyondSixtyEightMinutes(t *testing.T) {
	location, _ := time.LoadLocation("America/Toronto")
	trigger := nha040Automation(t, "- trigger: time_pattern\n  seconds: '*'").Triggers()[0]
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, location)
	highWater := ""
	for i := 0; i < 4100; i++ {
		next, ok := nextCivilTime(now, location, trigger, highWater)
		if !ok || !next.Equal(now.Add(time.Second)) {
			t.Fatalf("second %d: next=%v ok=%t", i, next, ok)
		}
		highWater = civilKey(next, location)
		now = next
	}
	if len(highWater) != 14 {
		t.Fatalf("high-water is not constant size: %d", len(highWater))
	}
}

func TestNHA040NumericCrossingRearm(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("Host:temperature")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: numeric_state\n  entity_id: switch.fixture\n  above: 0\n  below: 10")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := domain.NewSourceStamp("fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := func(value float64, revision uint64) domain.ObservedState {
		t.Helper()
		measurement, err := domain.NewMeasurement(value, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			t.Fatal(err)
		}
		native, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			t.Fatal(err)
		}
		at := clock.Now()
		times := domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}
		observed, err := domain.NewObservedStateSnapshot(entity, quality, &native, times, stamp, revision, nil)
		if err != nil {
			t.Fatal(err)
		}
		return observed
	}
	unknownQuality, err := domain.NewQuality(domain.AvailabilityUnknown, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := clock.Now()
	unknown, err := domain.NewObservedStateSnapshot(entity, unknownQuality, nil,
		domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, stamp, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.processNumericLocked(0, unknown, sequencer.Entry{})
	service.processNumericLocked(0, state(1, 2), sequencer.Entry{})
	if len(service.results) != 0 || service.armed[0] {
		t.Fatal("unknown followed by first in-range value armed or fired")
	}
	for i, value := range []float64{0, 1, 5, 10, 0, 1} {
		service.processNumericLocked(0, state(value, uint64(i+3)), sequencer.Entry{})
	}
	if len(service.results) != 2 || service.results[0].Kind() != ResultFired || service.results[1].Kind() != ResultFired {
		t.Fatalf("strict crossing/rearm fired %d times", len(service.results))
	}
}

type nha040Sun struct{ target time.Time }

func (s nha040Sun) NextSunset(_ context.Context, after time.Time, _ string, _ SolarPosition) (SunResult, error) {
	if !s.target.After(after) {
		return NoSunEvent(), nil
	}
	return SunEventAt(s.target)
}

func TestNHA040SunPortPresentAndAbsent(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	position, err := NewSolarPosition(43.65, -79.38)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: sun\n  event: sunset")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}, SunSource: nha040Sun{target: clock.Now().Add(time.Hour)}, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	clock.Advance(time.Hour, time.Hour)
	nha040Wait(t, func() bool { service.mu.Lock(); defer service.mu.Unlock(); return len(service.results) == 1 })
	result, err := service.NextResult(context.Background())
	if err != nil || result.Kind() != ResultFired {
		t.Fatalf("sun source fire: %+v %v", result, err)
	}
	service.mu.Lock()
	remaining := len(service.scheduled)
	service.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("absent next sun event scheduled %d timers", remaining)
	}
}

func TestNHA040CalendarOccurrenceWindowDedupeAndMissed(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: calendar\n  entity_id: calendar.fixture\n  event: start")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	source, _ := ParseCalendarSourceID("fixture-calendar")
	start := clock.Now().Add(time.Hour)
	item, err := NewCalendarOccurrence(source, "uid-1", "instance-1", start, start.Add(time.Hour), "America/Toronto", false, domain.FreshnessFresh)
	if err != nil {
		t.Fatal(err)
	}
	window := func(revision uint64, items ...CalendarOccurrence) CalendarWindow {
		t.Helper()
		value, err := NewCalendarWindow(source, revision, clock.Now().Add(-time.Minute), clock.Now().Add(3*time.Hour), nil, items)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if err := service.ReplaceCalendarWindow(context.Background(), window(1, item)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour, time.Hour)
	nha040Wait(t, func() bool { service.mu.Lock(); defer service.mu.Unlock(); return len(service.results) == 1 })
	first, err := service.NextResult(context.Background())
	if err != nil || first.Kind() != ResultFired {
		t.Fatalf("first occurrence: %+v %v", first, err)
	}
	moved, err := NewCalendarOccurrence(source, "uid-1", "instance-1", start.Add(30*time.Minute), start.Add(90*time.Minute), "America/Toronto", false, domain.FreshnessFresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReplaceCalendarWindow(context.Background(), window(2, moved)); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	remaining := len(service.scheduled)
	service.mu.Unlock()
	if remaining != 0 {
		t.Fatal("revision replayed an already seen edge")
	}
	past, err := NewCalendarOccurrence(source, "uid-2", "instance-1", clock.Now().Add(-30*time.Minute), clock.Now().Add(time.Hour), "America/Toronto", false, domain.FreshnessFresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReplaceCalendarWindow(context.Background(), window(3, moved, past)); err != nil {
		t.Fatal(err)
	}
	missed, err := service.NextResult(context.Background())
	if err != nil || missed.Kind() != ResultMissed {
		t.Fatalf("past edge was replayed or hidden: %+v %v", missed, err)
	}
	zone, _ := time.LoadLocation("America/Toronto")
	localStart := time.Date(2026, 11, 1, 0, 0, 0, 0, zone)
	localEnd := localStart.AddDate(0, 0, 1)
	if _, err := NewCalendarOccurrence(source, "all-day", "instance-2", localStart.UTC(), localEnd.UTC(), "America/Toronto", true, domain.FreshnessFresh); err != nil {
		t.Fatalf("DST all-day occurrence refused: %v", err)
	}
}

func TestNHA040StaleCalendarWindowDegradesThenMissesElapsedBorder(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: calendar\n  entity_id: calendar.fixture\n  event: start")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	source, _ := ParseCalendarSourceID("fixture-calendar")
	border := clock.Now().Add(20 * time.Minute)
	occurrence := func(freshness domain.Freshness) CalendarOccurrence {
		t.Helper()
		item, err := NewCalendarOccurrence(source, "uid-stale", "instance-1", border, border.Add(time.Hour), "America/Toronto", false, freshness)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	replace := func(revision uint64, item CalendarOccurrence) {
		t.Helper()
		window, err := NewCalendarWindow(source, revision, clock.Now().Add(-time.Hour), clock.Now().Add(2*time.Hour), nil, []CalendarOccurrence{item})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ReplaceCalendarWindow(context.Background(), window); err != nil {
			t.Fatal(err)
		}
	}
	replace(1, occurrence(domain.FreshnessFresh))
	service.mu.Lock()
	initiallyScheduled := len(service.scheduled)
	service.mu.Unlock()
	if initiallyScheduled != 1 {
		t.Fatalf("fresh border has %d timers, want one", initiallyScheduled)
	}
	replace(2, occurrence(domain.FreshnessStale))
	degraded, err := service.NextResult(context.Background())
	if err != nil || degraded.Kind() != ResultDegraded || degraded.Reason() != DegradedSource {
		t.Fatalf("stale window did not degrade its source: %+v %v", degraded, err)
	}
	service.mu.Lock()
	scheduled, seen := len(service.scheduled), len(service.seen)
	service.mu.Unlock()
	if scheduled != 0 || seen != 0 {
		t.Fatalf("stale window retained %d timers or %d consumed keys", scheduled, seen)
	}
	clock.Advance(30*time.Minute, 30*time.Minute)
	checkCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if result, err := service.NextResult(checkCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale border produced an output: %+v %v", result, err)
	}
	replace(3, occurrence(domain.FreshnessFresh))
	missed, err := service.NextResult(context.Background())
	if err != nil || missed.Kind() != ResultMissed {
		t.Fatalf("fresh revision replayed or hid elapsed border: %+v %v", missed, err)
	}
}

func TestNHA040CalendarWindowAdmissionBoundaries(t *testing.T) {
	source, err := ParseCalendarSourceID("fixture-calendar")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	to := from.Add(MaxCalendarHorizon)
	items := make([]CalendarOccurrence, MaxCalendarOccurrences+1)
	for i := range items {
		items[i], err = NewCalendarOccurrence(source, fmt.Sprintf("uid-%d", i), "instance-1",
			from.Add(time.Hour), from.Add(2*time.Hour), "America/Toronto", false, domain.FreshnessFresh)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewCalendarWindow(source, 1, from, to, nil, items[:MaxCalendarOccurrences]); err != nil {
		t.Fatalf("512 occurrences within 48 hours refused: %v", err)
	}
	if _, err := NewCalendarWindow(source, 1, from, to, nil, items); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("513 occurrences admitted: %v", err)
	}
	if _, err := NewCalendarWindow(source, 1, from, to.Add(time.Nanosecond), nil, items[:1]); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("window beyond 48 hours admitted: %v", err)
	}
}

type nha040BlockingSun struct {
	entered chan struct{}
	once    sync.Once
}

type nha040NilContext struct{ context.Context }

func TestNHA040TypedNilContextBoundaries(t *testing.T) {
	var typedNil *nha040NilContext
	service := &Service{}
	if err := service.Start(typedNil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Start admitted typed-nil context: %v", err)
	}
	if _, err := service.NextResult(typedNil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("NextResult admitted typed-nil context: %v", err)
	}
	if err := service.ReplaceCalendarWindow(typedNil, CalendarWindow{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("ReplaceCalendarWindow admitted typed-nil context: %v", err)
	}
}

func TestNHA040BlockedResultReaderWakesOnClose(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: time\n  at: '23:59:00'")},
		Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	readerEntered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(readerEntered)
		_, err := service.NextResult(context.Background())
		result <- err
	}()
	<-readerEntered
	select {
	case err := <-result:
		t.Fatalf("reader returned before shutdown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	service.BeginClose()
	select {
	case err := <-result:
		if !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("blocked reader returned %v after shutdown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked reader did not wake after shutdown")
	}
	select {
	case <-service.Done():
	case <-time.After(time.Second):
		t.Fatal("service did not join after shutdown")
	}
}

func (s *nha040BlockingSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ SolarPosition) (SunResult, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return SunResult{}, ctx.Err()
}

func TestNHA040CloseCancelsBlockedSunPort(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	position, _ := NewSolarPosition(43.65, -79.38)
	entity, _ := domain.ParseEntityID("Host:unused")
	source := &nha040BlockingSun{entered: make(chan struct{})}
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: sun\n  event: sunset")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}, SunSource: source, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- service.Start(context.Background()) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("sun source was not entered")
	}
	secondStart := make(chan error, 1)
	go func() { secondStart <- service.Start(context.Background()) }()
	select {
	case err := <-secondStart:
		if !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("concurrent Start while planning: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Start blocked behind the first source callback")
	}
	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for its own blocked source callback")
	}
	select {
	case err := <-started:
		if !errors.Is(err, ErrServiceClosed) && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start admitted a closed solar generation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start remained blocked after Close")
	}
	select {
	case <-service.Done():
	default:
		t.Fatal("Close left the not-yet-launched reader channel open")
	}
}

type nha040ExitGateSun struct {
	entered, cancelled, release, exited chan struct{}
}

func (s *nha040ExitGateSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ SolarPosition) (SunResult, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.cancelled)
	<-s.release
	close(s.exited)
	return SunResult{}, ctx.Err()
}

func TestNHA040CloseJoinsInflightStartCallback(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	position, _ := NewSolarPosition(43.65, -79.38)
	entity, _ := domain.ParseEntityID("Host:unused")
	source := &nha040ExitGateSun{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: sun\n  event: sunset")},
		Zone: "America/Toronto", Resolver: nha040Resolver{entity}, SunSource: source, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- service.Start(context.Background()) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("sun source was not entered")
	}
	firstClose := make(chan error, 1)
	go func() { firstClose <- service.Close() }()
	select {
	case <-source.cancelled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the sun source")
	}
	secondClose := make(chan error, 1)
	go func() { secondClose <- service.Close() }()
	select {
	case err := <-firstClose:
		t.Fatalf("Close returned before source callback exited: %v", err)
	case err := <-secondClose:
		t.Fatalf("second Close returned before source callback exited: %v", err)
	case <-service.Done():
		t.Fatal("Done closed before source callback exited")
	case <-time.After(30 * time.Millisecond):
	}
	close(source.release)
	for _, closed := range []<-chan error{firstClose, secondClose} {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close after callback exit: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not join the exited callback")
		}
	}
	select {
	case <-source.exited:
	default:
		t.Fatal("Close completed before callback exit")
	}
	select {
	case <-service.Done():
	default:
		t.Fatal("Done did not join the source callback")
	}
	select {
	case err := <-started:
		if !errors.Is(err, ErrServiceClosed) && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start admitted a closed generation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start remained blocked after callback exit")
	}
}

func TestNHA040CloseAdmissionsDuringBlockedSunPlanning(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	position, _ := NewSolarPosition(43.65, -79.38)
	entity, _ := domain.ParseEntityID("Host:unused")
	source := &nha040BlockingSun{entered: make(chan struct{})}
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: sun\n  event: sunset")},
		Zone: "America/Toronto", Resolver: nha040Resolver{entity}, SunSource: source, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- service.Start(context.Background()) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("sun source was not entered")
	}
	closedAdmissions := make(chan struct{})
	go func() { service.CloseAdmissions(); close(closedAdmissions) }()
	select {
	case <-closedAdmissions:
	case <-time.After(time.Second):
		t.Fatal("CloseAdmissions blocked on the solar callback")
	}
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start after closing admissions: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not discard the cancelled solar answer")
	}
	service.mu.Lock()
	if len(service.scheduled) != 0 || len(service.results) != 0 {
		service.mu.Unlock()
		t.Fatal("solar result survived closed admissions")
	}
	service.mu.Unlock()
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

type nha040ResyncSun struct {
	mu            sync.Mutex
	target        time.Time
	entered       chan struct{}
	calls, active int
	peak          int
}

func (s *nha040ResyncSun) NextSunset(ctx context.Context, _ time.Time, _ string, _ SolarPosition) (SunResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.active++
	if s.active > s.peak {
		s.peak = s.active
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	switch call {
	case 1:
		return SunEventAt(s.target)
	case 2:
		close(s.entered)
		<-ctx.Done()
		return SunResult{}, ctx.Err()
	default:
		return NoSunEvent(), nil
	}
}

func TestNHA040ResyncCancelsStaleSunCallWithoutParallelCalls(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	position, _ := NewSolarPosition(43.65, -79.38)
	entity, _ := domain.ParseEntityID("Host:unused")
	source := &nha040ResyncSun{target: clock.Now().Add(time.Hour), entered: make(chan struct{})}
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: sun\n  event: sunset")},
		Zone: "America/Toronto", Resolver: nha040Resolver{entity}, SunSource: source, SolarPosition: position})
	if err != nil {
		t.Fatal(err)
	}
	seq := fixture.Sequencer()
	service, err := NewService(plan, seq, clock)
	if err != nil {
		t.Fatal(err)
	}
	service.ctx, service.cancel = context.WithCancel(context.Background())
	service.mu.Lock()
	service.scheduleRuleLocked(0, service.sampleScheduleTime())
	service.mu.Unlock()
	start, err := seq.Start()
	if err != nil {
		t.Fatal(err)
	}
	old, _, _, _, err := seq.SubscribeTriggerViewAfter(start, 256, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	service.sub = old
	planning := make(chan struct{})
	go func() {
		service.mu.Lock()
		clock.Advance(time.Hour, time.Hour)
		service.scheduleRuleLocked(0, service.sampleScheduleTime())
		service.mu.Unlock()
		close(planning)
	}()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("second solar call was not entered")
	}
	resynced := make(chan error, 1)
	go func() { resynced <- service.resynchronize(old) }()
	select {
	case err := <-resynced:
		if err != nil {
			t.Fatalf("recapture: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("recapture waited for a cancelled old source call")
	}
	select {
	case <-planning:
	case <-time.After(time.Second):
		t.Fatal("old solar planner did not finish")
	}
	source.mu.Lock()
	peak := source.peak
	source.mu.Unlock()
	service.mu.Lock()
	generation, scheduled := service.generation, len(service.scheduled)
	service.mu.Unlock()
	if peak != 1 || generation != 2 || scheduled != 0 {
		t.Fatalf("stale or parallel solar planning: peak=%d generation=%d scheduled=%d", peak, generation, scheduled)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNHA040StateAttributeOnlyAndForPredicate(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:light")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: state\n  entity_id: switch.fixture\n  for: '00:00:30'")}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stamp, _ := domain.NewSourceStamp("fixture", nil)
	lightState := func(brightness float64, revision uint64) domain.ObservedState {
		t.Helper()
		light, err := domain.NewLightValue(true, &brightness, nil)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewLightStateValue(light)
		if err != nil {
			t.Fatal(err)
		}
		at := clock.Now()
		times := domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}
		state, err := domain.NewObservedStateSnapshot(entity, quality, &value, times, stamp, revision, nil)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	old := lightState(10, 1)
	next := lightState(20, 2)
	service.processStateLocked(0, old, next, sequencer.Entry{})
	if len(service.pending) != 1 {
		t.Fatal("attribute-only state_changed did not arm for")
	}
	clock.Advance(10*time.Second, 10*time.Second)
	changed := lightState(30, 3)
	service.states[entity] = changed
	service.processStateLocked(0, next, changed, sequencer.Entry{})
	if service.pending[0].deadline != 30*time.Second {
		t.Fatal("matching attribute-only update reset an existing for interval")
	}
	clock.Advance(19*time.Second, 19*time.Second)
	service.runDueLocked()
	if len(service.results) != 0 {
		t.Fatal("for fired before 30 seconds")
	}
	clock.Advance(time.Second, time.Second)
	service.runDueLocked()
	if len(service.results) != 1 || service.results[0].Kind() != ResultFired {
		t.Fatalf("for did not fire once at initial deadline: %d", len(service.results))
	}
	clock.Advance(10*time.Second, 10*time.Second)
	service.runDueLocked()
	if len(service.results) != 1 || len(service.pending) != 0 {
		t.Fatal("attribute-only update created a second fire at 40 seconds")
	}
}

func TestNHA040PureRegistryRemovalCancelsOnlyAffectedForAtItsCursor(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	sequence := fixture.Sequencer()
	instance, _ := domain.ParseIntegrationInstanceID("nha040-registry")
	a, _ := domain.ParseEntityID("Host:registry-a")
	b, _ := domain.ParseEntityID("Host:registry-b")
	deviceA, _ := domain.ParseDeviceID("registry-device-a")
	deviceB, _ := domain.ParseDeviceID("registry-device-b")
	aliasA, _ := domain.ParseHAAlias("switch.a")
	aliasB, _ := domain.ParseHAAlias("switch.b")
	keyA, _ := integrations.NewSourceKey("device-a", "relay")
	keyB, _ := integrations.NewSourceKey("device-b", "relay")
	recordA, err := integrations.NewRegistryRecord(deviceA, a, instance, keyA, aliasA, aliasA, []domain.HAAlias{aliasA}, false)
	if err != nil {
		t.Fatal(err)
	}
	recordB, err := integrations.NewRegistryRecord(deviceB, b, instance, keyB, aliasB, aliasB, []domain.HAAlias{aliasB}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(ctx, []integrations.RegistryRecord{recordA, recordB}); err != nil {
		t.Fatal(err)
	}
	entityA, _ := ir.NewEntity("switch.a", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	entityB, _ := ir.NewEntity("switch.b", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	catalogue, err := ir.NewCatalogue([]ir.Entity{entityA, entityB}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.NewSourceFile("registry.yaml", []byte("id: registry_waits\ntriggers:\n- trigger: state\n  id: a\n  entity_id: switch.a\n  to: 'on'\n  for: '00:00:30'\n- trigger: state\n  id: b\n  entity_id: switch.b\n  to: 'on'\n  for: '00:00:30'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.b}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.registry_waits", "registry.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("pair compile: %v, rejected=%+v", err, compiled.Rejected())
	}
	plan, err := NewPlan(PlanInput{Automations: compiled.Accepted(), Zone: "America/Toronto", Resolver: nha040PairResolver{map[domain.HAAlias]domain.EntityID{aliasA: a, aliasB: b}}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, sequence, clock)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := sequence.CaptureTriggerView(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	service.installSnapshotLocked(snapshot)
	service.cursor = snapshot.Cursor
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stamp, _ := domain.NewSourceStamp("fixture", nil)
	value := domain.NewSwitchValue(true)
	at := clock.Now()
	times := domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}
	stateB, err := domain.NewObservedStateSnapshot(b, quality, &value, times, stamp, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	chargeB, err := sequencer.EstimateTriggerStateBytes(b, stateB)
	if err != nil {
		t.Fatal(err)
	}
	service.states[b], service.stateCharges[b] = stateB, chargeB
	service.projectionBytes += chargeB
	for i, rule := range plan.rules {
		if rule.entity != a && rule.entity != b {
			t.Fatalf("unexpected state rule: %+v", rule)
		}
		service.pending[i] = pendingFor{deadline: 30 * time.Second, generation: service.generation, newText: "on"}
	}
	clock.Advance(29*time.Second, 29*time.Second)
	if err := sequence.PublishRegistry(ctx, []integrations.RegistryRecord{recordB}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := sequence.ReadAfter(snapshot.Cursor, 2)
	if err != nil || len(entries) != 1 {
		t.Fatalf("pure removal cursor: %d %v", len(entries), err)
	}
	removed, ok := entries[0].RegistryRemoved()
	if !ok || len(removed) != 1 || removed[0] != a {
		t.Fatalf("A removal absent from same cursor: %+v %v", removed, ok)
	}
	service.processEntryLocked(entries[0])
	if service.suspended || service.cursor != entries[0].Cursor() || len(service.registry) != 1 || len(service.pending) != 1 {
		t.Fatalf("removal changed unrelated waiter or cursor: suspended=%v registry=%d pending=%d", service.suspended, len(service.registry), len(service.pending))
	}
	for i := range service.pending {
		if plan.rules[i].entity != b {
			t.Fatal("A waiter remained armed after its registry removal")
		}
	}
	clock.Advance(time.Second, time.Second)
	service.runDueLocked()
	if len(service.results) != 1 || service.results[0].Kind() != ResultFired || service.results[0].Entity() != b || service.results[0].Cursor() != entries[0].Cursor() {
		t.Fatalf("B was lost or A fired after pure removal: %+v", service.results)
	}
}
