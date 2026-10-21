package automationtriggers

import (
	"context"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"testing"
	"time"
)

func TestNHA101RemoteDeadlineTerminalAndNoWallExtension(t *testing.T) {
	wall := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	d, err := NewRemoteCalendarDeadline(wall, wall.Add(time.Hour), time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !d.live(wall.Add(30*time.Minute), 90*time.Minute) {
		t.Fatal("valid admission refused")
	}
	if d.live(wall.Add(30*time.Minute), 2*time.Hour) {
		t.Fatal("civil rollback extended the original monotone deadline")
	}
	if d.live(wall, time.Hour) {
		t.Fatal("expired admission resurrected")
	}
	if _, err := NewRemoteCalendarDeadline(wall, wall, 0, time.Hour); err == nil {
		t.Fatal("empty civil lifetime admitted")
	}
	if _, err := NewRemoteCalendarDeadline(wall, wall.Add(time.Hour), time.Hour, time.Hour); err == nil {
		t.Fatal("empty monotone lifetime admitted")
	}
}

func TestNHA101RemoteExpiryAtFireAndEmptyStaleWindow(t *testing.T) {
	clock := &nha040Clock{wall: time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: calendar\n  entity_id: calendar.fixture\n  event: start")}, Zone: "UTC", Resolver: nha040Resolver{entity}})
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
	now := clock.Now()
	item, err := NewCalendarOccurrence(source, "uid", "instance", now.Add(time.Minute), now.Add(2*time.Minute), "UTC", false, domain.FreshnessFresh)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := NewRemoteCalendarDeadline(now, now.Add(30*time.Second), 0, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	window, err := NewCalendarWindow(source, 1, now, now.Add(MaxCalendarHorizon), nil, []CalendarOccurrence{item})
	if err != nil {
		t.Fatal(err)
	}
	window.remote = &remoteCalendarWindow{deadline: deadline}
	if err := service.ReplaceCalendarWindow(context.Background(), window); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute, time.Minute)
	nha040Wait(t, func() bool { service.mu.Lock(); defer service.mu.Unlock(); return service.calendarDegraded[source] })
	service.mu.Lock()
	seen := len(service.seen)
	service.mu.Unlock()
	if seen != 0 {
		t.Fatal("expired fire consumed seen before freshness check")
	}
	empty, err := NewCalendarWindow(source, 2, clock.Now(), clock.Now().Add(MaxCalendarHorizon), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty.remote = &remoteCalendarWindow{stale: true}
	if err := service.ReplaceCalendarWindow(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	stored := service.windows[source]
	degraded := service.calendarDegraded[source]
	service.mu.Unlock()
	if !degraded || stored.remote == nil || !stored.remote.stale || len(stored.occurrences) != 0 {
		t.Fatal("empty stale window silently became fresh")
	}
}
