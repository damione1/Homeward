package automationtriggers

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA029SameWaitSourceKeepsAdmittedAAndBDefinitionsInOneService(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
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
	binding, _ := driver.Binding()
	_, entity, _ := binding.IDs()
	a := nha042ScriptWait(t, "  - trigger: state\n    entity_id: switch.fixture\n    to: 'on'")
	b := nha042ScriptWait(t, "  - trigger: state\n    entity_id: switch.fixture\n    to: 'off'")
	planA, err := NewPlan(PlanInput{Scripts: []ir.Script{a}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	planB, err := NewPlan(PlanInput{Scripts: []ir.Script{b}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(planA, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	old, err := service.RetainCurrentPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	oldWait, err := old.AdmitWait(a.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	newImage, err := service.RetainPlan(planB)
	if err != nil {
		t.Fatal(err)
	}
	defer newImage.Release()
	newWait, err := newImage.AdmitWait(b.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	if err := newImage.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	if err := newImage.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := newImage.Publish(); err != ErrInvalidPlan {
		t.Fatalf("one-use plan published twice: %v", err)
	}
	if err := newImage.SchedulePublished(); err != nil {
		t.Fatal(err)
	}
	if oldWait.definition.source.Object() != newWait.definition.source.Object() || oldWait.definition.source.Path() != newWait.definition.source.Path() {
		t.Fatal("test did not reuse the exact logical wait source")
	}
	foreign, err := NewService(planA, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := foreign.ObserveAdmittedWait(ctx, oldWait); h != nil || err != ErrInvalidPlan {
		t.Fatalf("foreign service admitted A token: %v", err)
	}
	hA, err := service.ObserveAdmittedWait(ctx, oldWait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { <-hA.Close() }()
	hB, err := service.ObserveAdmittedWait(ctx, newWait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { <-hB.Close() }()
	publish := func(on bool) {
		t.Helper()
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		at := clock.Now()
		value := domain.NewSwitchValue(on)
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	publish(true)
	select {
	case <-hA.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("A wait did not fire on A definition")
	}
	select {
	case <-hB.Ready():
		t.Fatal("B wait fired on A definition")
	default:
	}
	clock.Advance(time.Second, time.Second)
	publish(false)
	select {
	case <-hB.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("B wait did not fire on B definition")
	}
	<-hA.Close()
	<-hB.Close()
	if err := old.Release(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	charge, images := service.planCharge, len(service.planImages)
	service.mu.Unlock()
	if charge != planB.charge || images != 1 {
		t.Fatalf("old plan retained after final wait/reference: charge=%d want=%d images=%d", charge, planB.charge, images)
	}
}
