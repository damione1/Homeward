package observability

import (
	"math"
	"testing"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

func TestNHA056CatalogueIsExactly124ClosedMeasures(t *testing.T) {
	catalogue := MetricCatalogue()
	if len(catalogue) != 124 || len(catalogue) > 128 {
		t.Fatalf("catalogue = %d", len(catalogue))
	}
	queues := make(map[obs.QueueClass]int)
	for _, slot := range catalogue {
		if class, present := slot.QueueClass(); present {
			queues[class]++
		}
		if slot.Descriptor() == nil {
			t.Fatal("slot without schema authority")
		}
	}
	if len(queues) != 9 {
		t.Fatalf("queue classes = %d", len(queues))
	}
	for class, count := range queues {
		want := 9
		if class <= obs.QueueClass_QUEUE_CLASS_VIEW {
			want++
		}
		if class == obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS {
			want += 2
		}
		if class == obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS {
			want++
		}
		if count != want {
			t.Fatalf("class %v: %d, want %d", class, count, want)
		}
	}
	if (&obs.InstalledQueueMetrics{}).ProtoReflect().Descriptor().Fields().Len() != 11 {
		t.Fatal("queue schema catalogue drift")
	}
	if (&obs.InstalledRecorderMetrics{}).ProtoReflect().Descriptor().Fields().Len() != 13 {
		t.Fatal("Recorder schema catalogue drift")
	}
	if obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS != 9 || len(obs.QueueClass_name) != 10 {
		t.Fatal("queue enum drift")
	}
}

func TestNHA056CountersSaturateAndRejectInvalidGauges(t *testing.T) {
	_, cells, _ := nha056Fixture(t)
	cells.progress.value = math.MaxUint64 - 1
	if err := cells.AddActionProgressIndeterminate(); err != nil {
		t.Fatal(err)
	}
	if cells.ActionCounters().GetProgressIndeterminateTotal().GetExact() != math.MaxUint64 {
		t.Fatal("exact maximum lost")
	}
	if err := cells.AddActionProgressIndeterminate(); err != nil {
		t.Fatal(err)
	}
	if cells.ActionCounters().GetProgressIndeterminateTotal().GetSaturated().GetValue() != math.MaxUint64 {
		t.Fatal("counter wrapped or was reset")
	}
	if err := cells.AddQueueRejected(0, 1); err == nil {
		t.Fatal("invalid queue accepted")
	}
	if _, err := NewQueueSnapshot(1, 3, 2, 0, nil, nil); err == nil {
		t.Fatal("depth beyond capacity accepted")
	}
	if _, err := NewQueueSnapshot(1, 0, 2, 2, nil, nil); err == nil {
		t.Fatal("degraded beyond active accepted")
	}
	for range 10 {
		_ = cells.ActionCounters()
	}
	if cells.ActionCounters().GetProgressIndeterminateTotal().GetSaturated() == nil {
		t.Fatal("read reset total")
	}
}

func TestNHA056Queue124SlotsSeparateInflightAndPending(t *testing.T) {
	_, cells, _ := nha056Fixture(t)
	class := obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS
	if err := cells.ChangeRuntimeQueue(class, 1, 2, 32, 1, 2); err != nil {
		t.Fatal(err)
	}
	q, err := cells.RuntimeQueue(class)
	if err != nil {
		t.Fatal(err)
	}
	if q.GetInstalled().InFlightEntries.GetValue() != 1 || q.GetInstalled().PendingEntries.GetValue() != 2 || q.GetInstalled().QueuedEntries.GetValue() != 2 {
		t.Fatal("queue gauges were derived from each other")
	}
	if contracts.AdmitObservabilityValue(q) != nil {
		t.Fatal("valid worker snapshot refused")
	}
	q.GetInstalled().PendingEntries = nil
	if contracts.AdmitObservabilityValue(q) == nil {
		t.Fatal("new required presence lost")
	}
	cloud := obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS
	if err := cells.ChangeRuntimeQueue(cloud, 1, 0, 1, 4, 0); err != nil {
		t.Fatal(err)
	}
	q, err = cells.RuntimeQueue(cloud)
	if err != nil || contracts.AdmitObservabilityValue(q) != nil {
		t.Fatal("cloud inflight incorrectly constrained by waiting capacity", err)
	}
	if q.GetInstalled().PendingEntries.GetNotMeasured() == nil {
		t.Fatal("cloud pending was invented")
	}
	q.GetInstalled().PendingEntries = &obs.GaugeMeasurement{Result: &obs.GaugeMeasurement_Value{Value: 0}}
	if contracts.AdmitObservabilityValue(q) == nil {
		t.Fatal("cloud admitted an inapplicable pending measurement")
	}
}
