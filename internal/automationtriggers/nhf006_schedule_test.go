package automationtriggers

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func nhf006ScheduleFixture(t *testing.T) (*Service, *nha040Clock) {
	t.Helper()
	clock := &nha040Clock{wall: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("Host:unused")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(PlanInput{
		Automations: []ir.Automation{nha040Automation(t, "- trigger: time_pattern\n  seconds: '*'")},
		Zone:        "America/Toronto", Resolver: nha040Resolver{entity},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, clock
}

func TestNHF006PlanningTimeDoesNotBecomeACivilClockJump(t *testing.T) {
	service, clock := nhf006ScheduleFixture(t)
	plannedAt := service.sampleScheduleTime()
	instant := plannedAt.wall.Add(time.Second)
	// Planning consumes real elapsed time; neither clock jumps. Binding the
	// original civil sample to a later monotonic sample would move the deadline.
	clock.Advance(250*time.Millisecond, 250*time.Millisecond)
	service.setScheduleLocked(0, instant, plannedAt)
	clock.Advance(750*time.Millisecond, 750*time.Millisecond)
	service.runDueLocked()
	if len(service.results) != 1 || service.results[0].Kind() != ResultFired {
		t.Fatalf("elapsed planning was mistaken for a clock jump: %+v", service.results)
	}
}

func TestNHF006WallJumpDuringPlanningDoesNotRearmTheDeadline(t *testing.T) {
	service, clock := nhf006ScheduleFixture(t)
	plannedAt := service.sampleScheduleTime()
	instant := plannedAt.wall.Add(time.Hour)
	clock.Advance(time.Hour-time.Second, 250*time.Millisecond)
	service.setScheduleLocked(0, instant, plannedAt)
	clock.Advance(time.Second, time.Second)
	service.runDueLocked()
	if len(service.results) != 1 || service.results[0].Kind() != ResultMissed {
		t.Fatalf("planning hid a real wall jump: %+v", service.results)
	}
}
