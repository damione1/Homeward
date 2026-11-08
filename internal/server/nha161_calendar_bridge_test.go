package server

import (
	"context"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
	"testing"
	"time"
)

type nha161BridgeClock struct{ wall time.Time }

func (c *nha161BridgeClock) Now() time.Time         { return c.wall }
func (c *nha161BridgeClock) MonoNow() time.Duration { return 0 }
func (c *nha161BridgeClock) NewTimer(d time.Duration) automationtriggers.Timer {
	return automationtriggers.RealClock{}.NewTimer(d)
}
func TestNHA161BridgeSQLLocalCalculatedAndRestartMissed(t *testing.T) {
	r := nhf015NewCalendarRig(t, 120*time.Second)
	local := r.create(t)
	r.add(t, local, "synthetic-series", nha202CalendarDayEvent())
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	records, err := r.lists.registry.List(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := sourceRuntimeResolverFromRecords(records, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.calendars) < 2 {
		t.Fatal("local and calculated sources missing from resolver")
	}
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	makeBridge := func(at time.Time) *calendarBridge {
		clock := &nha161BridgeClock{at}
		_, fixture, err := fake.NewFixtureWithClock(clock)
		if err != nil {
			t.Fatal(err)
		}
		alias := nhf015CalendarAliases(t)[calendars.Local]
		calendar, _ := ir.NewEntity(alias, ir.ReferencePresent)
		target, _ := ir.NewEntity("switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
		catalogue, _ := ir.NewCatalogue([]ir.Entity{calendar, target}, nil, nil)
		body := "id: synthetic_calendar\ntriggers:\n- trigger: calendar\n  entity_id: " + alias + "\n  event: start\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n"
		source, _ := parser.NewSourceFile("synthetic-calendar.yaml", []byte(body))
		compiled, err := ir.CompileSources("automation.synthetic_calendar", "synthetic-calendar.yaml", []parser.SourceFile{source}, catalogue)
		if err != nil || len(compiled.Accepted()) != 1 {
			t.Fatal("calendar fixture", err)
		}
		plan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: compiled.Accepted(), Zone: "America/Toronto", Resolver: resolver})
		if err != nil {
			t.Fatal(err)
		}
		engine, err := automationtriggers.NewService(plan, fixture.Sequencer(), clock)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.AttachCalendarDeliveryJournal(r.store); err != nil {
			t.Fatal(err)
		}
		if err = engine.Start(r.ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Close() })
		return &calendarBridge{handler: r.handler.calendarResourceHandler, engine: engine, store: r.store, clock: clock, coverage: map[automationtriggers.CalendarSourceID]time.Time{}}
	}
	first := makeBridge(now)
	var localDemand automationtriggers.CalendarDemand
	for _, origin := range []calendars.Origin{calendars.Local, calendars.Calculated} {
		entity := r.ids[origin]
		raw, _ := entity.Value()
		source, _ := automationtriggers.ParseCalendarSourceID("calendar/" + raw)
		demand := automationtriggers.CalendarDemand{Source: source, Zone: "America/Toronto", MinimumOffset: -time.Hour, MaximumOffset: time.Hour}
		if err = first.refreshSource(r.ctx, demand); err != nil {
			t.Fatalf("origin %v: %v", origin, err)
		}
		coverage, err := r.store.ReadCalendarCoverage(r.ctx, "calendar/"+raw)
		if err != nil || !coverage.Equal(now) {
			t.Fatal("coverage missing", coverage, err)
		}
		if origin == calendars.Local {
			localDemand = demand
		}
	}
	// Deterministically deliver an invalidation after SQL's final revision
	// check but before publication. The old capture must not rearm the source.
	first.beforePublish = func() { first.invalidate(r.ctx, localDemand.Source) }
	if err = first.refreshSource(r.ctx, localDemand); err == nil {
		t.Fatal("watch invalidation overwritten by stale publication")
	}
	if first.armed[localDemand.Source] {
		t.Fatal("invalidated source rearmed")
	}
	first.beforePublish = nil
	_ = first.engine.Close()
	restarted := makeBridge(now.Add(29 * time.Hour))
	if err = restarted.refreshSource(r.ctx, localDemand); err != nil {
		t.Fatal(err)
	}
	// The batch checkpoint has committed while every Missed result is still
	// undrained. A restart cannot resurrect one of these original identities.
	_, keys := restarted.engine.CalendarCheckpoint(localDemand.Source, now.Add(29*time.Hour))
	if len(keys) == 0 {
		t.Fatal("no consumed historical identity")
	}
	for _, key := range keys {
		if claimed, err := r.store.ClaimCalendarDelivery(r.ctx, key, now.Add(30*time.Hour)); err != nil || claimed {
			t.Fatal("checkpoint advanced before missed claim", claimed, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := restarted.engine.NextResult(ctx)
	if err != nil || result.Kind() != automationtriggers.ResultMissed || result.Automation() != "synthetic_calendar" {
		t.Fatalf("restart replayed or hid gap: %v %v", result.Kind(), err)
	}
}
