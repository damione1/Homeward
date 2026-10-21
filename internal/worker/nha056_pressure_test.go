package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA056WorkerRealPressureAndTimeoutReleaseMetrics(t *testing.T) {
	journal, _ := nha056WorkerJournal(t)
	attached, err := journal.Cells()
	if err != nil {
		t.Fatal(err)
	}
	rig := startWorkerRigWithGate(t, "many_hold", true, "", "", nil, func(host *integrations.Manager) {
		if err := host.AttachObservation(attached, journal); err != nil {
			t.Fatal(err)
		}
	})
	cells, err := rig.host.ObservationCells()
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha056-pressure-actor")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, maxPending)
	for i := range maxPending {
		request := commandRequest(t, rig.fixture, rig.entities[i], fmt.Sprintf("nha056-held-%d", i))
		go func() {
			response, err := rig.service.Dispatch(ctx, actor, request)
			if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
				done <- fmt.Errorf("held outcome %v: %w", response, err)
				return
			}
			done <- nil
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		queue, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
		if err != nil {
			t.Fatal(err)
		}
		installed := queue.GetInstalled()
		if installed != nil && installed.PendingEntries.GetValue() == maxPending {
			if installed.ActiveQueues.GetValue() != 1 || installed.CapacityEntries.GetValue() != maxPending || installed.InFlightEntries.GetValue() > maxPending || installed.QueuedEntries.GetValue() > maxPending {
				t.Fatal("worker populations lost their independent bounds")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual worker pending population never filled")
		}
		<-poll.C
	}
	response, err := rig.service.Dispatch(context.Background(), actor, commandRequest(t, rig.fixture, rig.entities[maxPending], "nha056-rejected"))
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatal("pressure changed command certainty", err)
	}
	queue, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
	if err != nil || queue.GetInstalled().RejectedTotal.GetExact() != 1 {
		t.Fatal("real worker rejection not counted", err)
	}
	cancel()
	for range maxPending {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled dispatch not joined")
		}
	}
	queue, err = cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
	if err != nil || queue.GetInstalled().PendingEntries.GetValue() != 0 || queue.GetInstalled().InFlightEntries.GetValue() != 0 {
		t.Fatal("cancelled worker retained pending population", err)
	}
	// Cancellation releases pending ownership before the stream necessarily
	// removes every stale delivery. Observe that removal before admitting the
	// timeout witness so capacity rejection cannot satisfy its error oracle.
	deadline = time.Now().Add(5 * time.Second)
	for queue.GetInstalled().QueuedEntries.GetValue() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("cancelled worker deliveries were not drained")
		}
		<-poll.C
		queue, err = cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
		if err != nil || queue.GetInstalled() == nil {
			t.Fatal("worker queue disappeared before timeout witness", err)
		}
	}
	request := commandRequest(t, rig.fixture, rig.entities[0], "nha056-timeout")
	expires := time.Now().Add(3 * time.Second)
	request.ExpiresAt = timestamppb.New(expires)
	timeoutDone := make(chan error, 1)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), actor, request)
		if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			timeoutDone <- fmt.Errorf("worker timeout outcome %v: %w", response, err)
			return
		}
		timeoutDone <- nil
	}()
	for {
		queue, err = cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
		if err != nil || queue.GetInstalled() == nil {
			t.Fatal("worker queue disappeared during timeout witness", err)
		}
		if !time.Now().Before(expires) {
			t.Fatal("worker timeout was not observed in flight before expiration")
		}
		installed := queue.GetInstalled()
		if installed.PendingEntries.GetValue() == 1 && installed.InFlightEntries.GetValue() == 1 {
			break
		}
		select {
		case err := <-timeoutDone:
			t.Fatal("timeout dispatch completed before its in-flight witness", err)
		case <-poll.C:
		}
	}
	select {
	case err := <-timeoutDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired dispatch not joined")
	}
	if time.Now().Before(expires) {
		t.Fatal("worker dispatch completed before its expiration")
	}
	queue, err = cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
	if err != nil || queue.GetInstalled().PendingEntries.GetValue() != 0 || queue.GetInstalled().InFlightEntries.GetValue() != 0 || queue.GetInstalled().RejectedTotal.GetExact() != 1 {
		t.Fatal("timeout retained worker population", err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 7*time.Second)
	defer stop()
	if err := rig.supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	queue, err = cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS)
	if err != nil || queue.GetNotInstalled() == nil {
		t.Fatal("joined worker queue remained installed", err)
	}
	select {
	case <-rig.supervisor.Done():
	default:
		t.Fatal("worker Stop did not join its owner")
	}
}

func TestNHA056ActualWorkerHeartbeatExpiryDegradesOnlyItsCapacity(t *testing.T) {
	journal, _ := nha056WorkerJournal(t)
	cells, err := journal.Cells()
	if err != nil {
		t.Fatal(err)
	}
	rig := startWorkerRigWithGate(t, "silent", true, "", "", nil, func(host *integrations.Manager) {
		if err := host.AttachObservation(cells, journal); err != nil {
			t.Fatal(err)
		}
	})
	// Add one synchronous native fixture to the same Host and route/ledger.
	// Preserve the worker's existing registry records while admitting its peer.
	local, err := domain.ParseIntegrationInstanceID("nha056-native-beside-worker")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := rig.fixture.Resolve(context.Background(), local, []integrations.SourceKey{key})
	if err != nil || len(ids) != 1 {
		t.Fatal("local fixture identity unavailable", err)
	}
	alias, err := domain.ParseHAAlias("switch.nha056_native_peer")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, local, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	records := []integrations.RegistryRecord{record}
	for _, existing := range snapshot.Registry {
		records = append(records, existing)
	}
	if err := rig.fixture.PublishRegistry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	session, err := rig.host.OpenSession(local)
	if err != nil {
		t.Fatal(err)
	}
	defer rig.host.Revoke(session)
	driver, err := fake.NewActionDriver(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := rig.host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	worker, err := domain.ParseIntegrationInstanceID("nha094-synthetic-worker")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		supervision, err := rig.supervisor.ObservationIntegration(worker)
		if err != nil {
			t.Fatal(err)
		}
		if status := supervision.GetSupervised(); status != nil && status.Phase == obs.PilotPhase_PILOT_PHASE_DEGRADED && status.Tasks == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("actual heartbeat expiry did not degrade and join its worker tasks")
		case <-poll.C:
		}
	}
	report, err := rig.fixture.Sequencer().IntegrationHealth(worker)
	if err != nil || report.GetPresent().GetStatus() != wire.HealthStatus_HEALTH_STATUS_DEGRADED {
		t.Fatal("expired worker health projection was not degraded", err)
	}
	phases, err := rig.supervisor.ObservationPhases()
	if err != nil {
		t.Fatal(err)
	}
	degraded := uint64(0)
	for _, phase := range phases {
		if phase.Phase == obs.PilotPhase_PILOT_PHASE_DEGRADED {
			degraded = phase.Instances
		}
	}
	if degraded != 1 || !rig.host.AdmissionsOpen() {
		t.Fatal("worker heartbeat failure changed core admission or its phase projection")
	}
	actor, err := domain.ParseActorID("nha056-native-worker-peer-actor")
	if err != nil {
		t.Fatal(err)
	}
	response, err := rig.service.Dispatch(context.Background(), actor, commandRequest(t, rig.fixture, ids[0].Entity, "nha056-native-after-worker-expiry"))
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || len(driver.Commands()) != 1 {
		t.Fatal("expired worker blocked or repeated its peer's local fake command", err)
	}
	report, err = rig.fixture.Sequencer().IntegrationHealth(local)
	if err != nil || report.GetPresent().GetStatus() != wire.HealthStatus_HEALTH_STATUS_READY {
		t.Fatal("expired worker degraded its local peer", err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 7*time.Second)
	defer stop()
	if err := rig.supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rig.supervisor.Done():
	default:
		t.Fatal("worker heartbeat owner was not joined")
	}
}
