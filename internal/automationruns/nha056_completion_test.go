package automationruns

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
)

func TestNHA056ActualAutomationPendingPressureCancelsOnce(t *testing.T) {
	automation := nha041Automation(t, "queued", 3)
	plan, err := NewPlan([]ir.Automation{automation})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan Run, 1)
	executor := nha041ExecuteFunc(func(ctx context.Context, _ ir.Automation, run Run) error {
		entered <- run
		<-ctx.Done()
		return ctx.Err()
	})
	preparer := nha041PrepareFunc(func(context.Context, ir.Automation, Trigger) (Preparation, error) {
		return Preparation{Outcome: PreparationReady}, nil
	})
	c, err := NewController(plan, &nha041Reader{done: make(chan struct{})}, preparer, executor)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AttachObservation(cells); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	spec := plan.items[automation.ID()]
	nha041Admit(t, c, spec, "first")
	nha041Receive(t, entered)
	for _, label := range []string{"pending-1", "pending-2", "rejected"} {
		nha041Admit(t, c, spec, label)
	}
	queues, metrics, err := c.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	queue := queues[0].GetInstalled()
	if queue == nil || queue.CapacityEntries.GetValue() != 2 || queue.QueuedEntries.GetValue() != 2 || queue.RejectedTotal.GetExact() != 1 || metrics.GetInstalled().Kinds[0].Active.GetValue() != 1 {
		t.Fatal("actual automation queue pressure not represented")
	}
	c.BeginClose()
	c.BeginClose()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		queues, metrics, err := c.ObservationMetrics()
		if err != nil || queues[0].GetInstalled().QueuedEntries.GetValue() != 0 {
			t.Fatal("automation queue did not drain", err)
		}
		found := false
		for _, kind := range metrics.GetInstalled().Kinds {
			if kind.Kind == obs.RunKind_RUN_KIND_AUTOMATION {
				found = true
				if kind.CancelledTotal.GetExact() != 3 || kind.Active.GetValue() != 0 {
					t.Fatal("automation terminal removal recounted or lost")
				}
			}
		}
		if !found {
			t.Fatal("automation metrics absent")
		}
	}
}
