package server

import (
	"context"
	"fmt"
	"reflect"
	goruntime "runtime"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
)

// Costs describe this complete real Run snapshot, on the executing machine.
// CPU comes from the process rusage counters; elapsed wall time is separate.
func TestNHA056RealSnapshotCatalogueAfterTenThousandIDsAndCosts(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	nha056ActivateLocalGeneration(t, cfg)
	_, runtime, _ := nha056ObservedDefaultRun(t, cfg)
	catalogue := observability.MetricCatalogue()
	before, err := runtime.metricsSnapshot()
	if err != nil || contracts.AdmitObservabilityResponse(before) != nil {
		t.Fatal("initial real snapshot invalid", err)
	}
	// Exercise the installed sequencer's real native ingress, rather than only
	// looking up unknown IDs. Its bounded journal admits these source identities
	// without installing 10,000 integrations or creating a registry projection.
	startCursor, err := runtime.sequence.RecorderCursor()
	if err != nil {
		t.Fatal(err)
	}
	startEpoch, startSequence, err := startCursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	targets := make(map[domain.IntegrationInstanceID]uint64, 10000)
	for i := range 10000 {
		instance, err := domain.ParseIntegrationInstanceID(fmt.Sprintf("nha056-new-diagnostic-id-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		sourceSequence := uint64(i + 1)
		source, err := domain.NewSourceStamp(fmt.Sprintf("nha056-new-source-%d", i), &sourceSequence)
		if err != nil {
			t.Fatal(err)
		}
		edge, err := integrations.NewNativeMQTTEdge(fmt.Sprintf("nha056/new-source/%d", i), []byte("synthetic"), false, sourceSequence)
		if err != nil {
			t.Fatal(err)
		}
		targets[instance] = sourceSequence
		if err := runtime.sequence.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
			t.Fatal("new source identity did not cross the real ingress", err)
		}
	}
	endCursor, err := runtime.sequence.RecorderCursor()
	if err != nil {
		t.Fatal(err)
	}
	endEpoch, endSequence, err := endCursor.Components()
	if err != nil || endEpoch != startEpoch || endSequence < startSequence+10000 {
		t.Fatal("new identities did not advance the actual admission stream", err)
	}
	// Other real Run producers share this cursor. Count the test's exact
	// identities in the real bounded journal, rather than treating the global
	// delta as a private counter or assuming the next row belongs to this test.
	seen := make(map[domain.IntegrationInstanceID]bool, len(targets))
	for cursor := startCursor; ; {
		entries, next, err := runtime.sequence.ReadAfter(cursor, 256)
		if err != nil || len(entries) == 0 {
			t.Fatal("targeted native admissions were not retained", err)
		}
		for _, entry := range entries {
			_, position, err := entry.Cursor().Components()
			if err != nil {
				t.Fatal("native replay position was not admitted", err)
			}
			if position > endSequence {
				break
			}
			instance, _, _, present := entry.Origin()
			expectedSequence, targeted := targets[instance]
			if !present || !targeted {
				continue
			}
			edge, native := entry.NativeMQTTEdge()
			if !native || seen[instance] {
				t.Fatal("targeted identity was not admitted exactly once as a native publication")
			}
			_, _, sourceSequence, err := edge.Components()
			if err != nil || sourceSequence != expectedSequence {
				t.Fatal("native ingress lost its actual source sequence", err)
			}
			seen[instance] = true
		}
		epoch, position, err := next.Components()
		if err != nil || epoch != startEpoch {
			t.Fatal("native admission replay changed epoch", err)
		}
		if position >= endSequence {
			break
		}
		cursor = next
	}
	if len(targets) != 10000 || len(seen) != 10000 {
		t.Fatalf("native admission witness saw %d of %d targeted identities", len(seen), len(targets))
	}
	t.Logf("targeted_native_admissions=%d global_admission_delta=%d", len(seen), endSequence-startSequence)
	after, err := runtime.metricsSnapshot()
	if err != nil || contracts.AdmitObservabilityResponse(after) != nil {
		t.Fatal("final real snapshot invalid", err)
	}
	if !reflect.DeepEqual(catalogue, observability.MetricCatalogue()) || len(catalogue) != 124 || len(after.Metrics.Queues) != 9 || len(after.Metrics.QueueLags) != 4 || proto.Size(after) > 32<<10 {
		t.Fatal("new IDs changed the closed catalogue or snapshot bound")
	}
	for i := range before.Metrics.Queues {
		if before.Metrics.Queues[i].QueueClass != after.Metrics.Queues[i].QueueClass {
			t.Fatal("targeted IDs changed a queue identity")
		}
	}
	for i := range before.Metrics.QueueLags {
		if before.Metrics.QueueLags[i].QueueClass != after.Metrics.QueueLags[i].QueueClass {
			t.Fatal("new source IDs changed a lag identity")
		}
	}
	var snapshotErr error
	allocations := testing.AllocsPerRun(100, func() { _, snapshotErr = runtime.metricsSnapshot() })
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	var memoryBefore, memoryAfter goruntime.MemStats
	var startCPU, stopCPU syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &startCPU); err != nil {
		t.Fatal(err)
	}
	goruntime.ReadMemStats(&memoryBefore)
	start := time.Now()
	const samples = 1000
	for range samples {
		response, err := runtime.metricsSnapshot()
		if err != nil || proto.Size(response) > 32<<10 {
			t.Fatal("cost sample violated snapshot contract", err)
		}
	}
	wall := time.Since(start)
	goruntime.ReadMemStats(&memoryAfter)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &stopCPU); err != nil {
		t.Fatal(err)
	}
	timeval := func(value syscall.Timeval) time.Duration {
		return time.Duration(value.Sec)*time.Second + time.Duration(value.Usec)*time.Microsecond
	}
	cpu := timeval(stopCPU.Utime) + timeval(stopCPU.Stime) - timeval(startCPU.Utime) - timeval(startCPU.Stime)
	if cpu < 0 {
		t.Fatal("CPU observation regressed")
	}
	t.Logf("actual Run snapshot: catalogue=%d protobuf_bytes=%d allocations_per_snapshot=%.1f sampled_allocated_bytes_per_snapshot=%d samples=%d cpu_total=%s wall_total=%s", len(catalogue), proto.Size(after), allocations, (memoryAfter.TotalAlloc-memoryBefore.TotalAlloc)/samples, samples, cpu, wall)
}
