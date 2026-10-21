package automationtriggers

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

// Even an empty B needs a second 256-slot rule image while A is live. The
// existing service's accounted projection is used to put that real staging
// requirement at the admission boundary without a wall-clock or heap probe.
func TestNHA029EmptyCandidateRefusesUnreservedStagingCharge(t *testing.T) {
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
	candidate, err := NewEmptyActivationPlan("UTC")
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
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	// Only the fixed rule array is counted here; the two staging maps need
	// additional space, so this is a conservative lower bound.
	stagingLowerBound := MaxTriggers * int(unsafe.Sizeof(rule{}))
	if stagingLowerBound <= 256 {
		t.Fatal("fixture has no discriminating staging lower bound")
	}
	service.mu.Lock()
	current := service.estimatedChargeLocked(service.occurrenceCountLocked(), service.seenBytes)
	target := MaxEngineCharge - 256 - candidate.charge - stagingLowerBound/2
	if target <= current {
		service.mu.Unlock()
		t.Fatalf("fixture cannot approach budget: current=%d target=%d", current, target)
	}
	pressure := target - current
	service.projectionBytes += pressure
	service.mu.Unlock()
	defer func() {
		service.mu.Lock()
		service.projectionBytes -= pressure
		service.mu.Unlock()
	}()

	reservation, err := service.RetainPlan(candidate)
	if err == ErrCapacity {
		return // A corrected admission may account for staging here.
	}
	if err != nil {
		t.Fatal(err)
	}
	prepareErr := reservation.PreparePublication()
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if prepareErr != ErrCapacity {
		t.Errorf("B staging exceeded available engine charge but was admitted: %v", prepareErr)
	}
	service.mu.Lock()
	charge, images := service.planCharge, len(service.planImages)
	service.mu.Unlock()
	if charge != old.charge || images != 1 {
		t.Fatalf("candidate charge leaked after refusal: charge=%d images=%d", charge, images)
	}
}

func TestNHA029OldWaitImageFreedOnlyAfterItsLastHandle(t *testing.T) {
	ctx := context.Background()
	clock := &nha040Clock{wall: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	script := nha042ScriptWait(t, "  - trigger: event\n    event_type: nha042_release")
	old, err := NewPlan(PlanInput{Scripts: []ir.Script{script}, Zone: "UTC", Resolver: nha040Resolver{}})
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
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	oldLease, err := service.RetainCurrentPlan()
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := oldLease.AdmitWait(script.Waits()[0].Source())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := service.ObserveAdmittedWait(ctx, admitted)
	if err != nil {
		t.Fatal(err)
	}
	newPlan, err := NewEmptyActivationPlan("UTC")
	if err != nil {
		t.Fatal(err)
	}
	newLease, err := service.RetainPlan(newPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := newLease.PreparePublication(); err != nil {
		t.Fatal(err)
	}
	if err := newLease.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := oldLease.Release(); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	charge, images := service.planCharge, len(service.planImages)
	service.mu.Unlock()
	if charge != old.charge+newPlan.charge || images != 2 {
		t.Fatalf("live A wait lost its image after lease release: charge=%d images=%d", charge, images)
	}
	<-handle.Close()
	service.mu.Lock()
	charge, images = service.planCharge, len(service.planImages)
	service.mu.Unlock()
	if charge != newPlan.charge || images != 1 {
		t.Fatalf("A image survived its final handle: charge=%d images=%d", charge, images)
	}
	if err := newLease.Release(); err != nil {
		t.Fatal(err)
	}
}
