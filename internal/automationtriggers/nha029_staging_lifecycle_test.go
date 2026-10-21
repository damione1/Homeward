package automationtriggers

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA029StagingChargeTransfersAndReturnsAtTerminalClose(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	old, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(old, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	firstPlan, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.RetainPlan(firstPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	preparedCharge := service.stagingCharge
	service.mu.Unlock()
	if preparedCharge < MaxTriggers*int(unsafe.Sizeof(rule{})) {
		t.Fatalf("staging did not own the rule array: %d", preparedCharge)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	chargeAfterRelease := service.stagingCharge
	service.mu.Unlock()
	if chargeAfterRelease != 0 {
		t.Fatalf("rejected candidate kept staging charge: %d", chargeAfterRelease)
	}
	secondPlan, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RetainPlan(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	if err := second.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	publishedCharge := service.stagingCharge
	service.mu.Unlock()
	if publishedCharge == 0 {
		t.Fatal("published rule array lost its budget while service still owns it")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	finalStaging, finalPlans, images := service.stagingCharge, service.planCharge, len(service.planImages)
	service.mu.Unlock()
	if finalStaging != 0 || finalPlans != 0 || images != 0 {
		t.Fatalf("terminal close retained image charges: staging=%d plans=%d images=%d", finalStaging, finalPlans, images)
	}
}

func TestNHA029TerminalCloseRejectsStagedCandidateAndReturnsItsCharge(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	old, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(old, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	newPlan, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := service.RetainPlan(newPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := reserved.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reserved.Publish(); err != ErrInvalidPlan {
		t.Fatalf("closed staged candidate published: %v", err)
	}
	if err := reserved.Release(); err != ErrInvalidPlan {
		t.Fatalf("closed staged candidate retained a second owner: %v", err)
	}
	service.mu.Lock()
	staging, plans, images := service.stagingCharge, service.planCharge, len(service.planImages)
	service.mu.Unlock()
	if staging != 0 || plans != 0 || images != 0 {
		t.Fatalf("close retained staged candidate: staging=%d plans=%d images=%d", staging, plans, images)
	}
}
