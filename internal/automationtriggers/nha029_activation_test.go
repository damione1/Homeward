package automationtriggers

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA029DiscardPublishedCandidateKeepsOnlyChargedLiveWaitStorage(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	script := nha042ScriptWait(t, "  - trigger: event\n    event_type: nha042_release")
	planA, err := NewPlan(PlanInput{Scripts: []ir.Script{script}, Zone: "UTC", Resolver: nha040Resolver{domain.EntityID{}}})
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
	t.Cleanup(func() { _ = service.Close() })
	a, err := service.RetainCurrentPlan()
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := a.AdmitWait(script.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	live, err := service.ObserveAdmittedWait(ctx, admitted)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { <-live.Close() })
	if err := a.Release(); err != nil {
		t.Fatal(err)
	} // the real wait now owns A
	planB, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.RetainPlan(planB)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(); err != nil {
		t.Fatal(err)
	}
	stageB := b.image.stagingCharge
	if stageB == 0 {
		t.Fatal("B array/maps have no physical charge")
	}
	if err := b.DiscardPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	if service.planCharge != planA.charge || service.stagingCharge != stageB || service.detachedStagingCharge != stageB || len(service.planImages) != 1 || service.planImages[planB.token] != nil || service.activePlanImage != nil || len(service.waits) != 1 || len(service.waitOwners) != 1 || len(service.plan.rules) != MaxTriggers {
		service.mu.Unlock()
		t.Fatal("discard retained B metadata or lost the charged live A wait storage")
	}
	service.mu.Unlock()
	select {
	case <-live.Done():
		t.Fatal("discard closed intact A wait")
	default:
	}
	planC, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	c, err := service.RetainPlan(planC)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(); err != nil {
		t.Fatal(err)
	}
	stageC := c.image.stagingCharge
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	if service.detachedStagingCharge != 0 || service.stagingCharge != stageC || service.planCharge != planA.charge+planC.charge {
		service.mu.Unlock()
		t.Fatal("C did not return B's transferred storage exactly")
	}
	service.mu.Unlock()
	<-live.Close()
	service.mu.Lock()
	if service.planCharge != planC.charge || len(service.planImages) != 1 {
		service.mu.Unlock()
		t.Fatal("A plan cost was not returned at its actual last wait holder")
	}
	service.mu.Unlock()
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	plans, stages, images := service.planCharge, service.stagingCharge, len(service.planImages)
	service.mu.Unlock()
	if plans != 0 || stages != 0 || images != 0 {
		t.Fatalf("terminal retained metadata: plans=%d staging=%d images=%d", plans, stages, images)
	}
}
