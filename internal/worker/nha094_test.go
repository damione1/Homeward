package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fixedGeneration struct{}

func (fixedGeneration) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

func pythonConfig(t *testing.T, instance, mode, marker string, extraArgs ...string) Config {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
	if err != nil {
		t.Fatalf("run scripts/ensure-worker-python.sh before tests: %v", err)
	}
	args := []string{filepath.Join(root, "testworker/fake_worker.py"), "--mode=" + mode, "--marker", marker}
	args = append(args, extraArgs...)
	config, err := NewConfig(instance, strings.TrimSpace(string(path)), args, nil)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func waitMarker(t *testing.T, marker, text string, timeout time.Duration, onWaiting ...func()) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var reportWaiting func()
	if len(onWaiting) != 0 {
		reportWaiting = onWaiting[0]
	}
	for {
		bytes, err := os.ReadFile(marker)
		if err == nil && strings.Contains(string(bytes), text) {
			return string(bytes)
		}
		if err == nil && strings.Contains(string(bytes), "command_stream_error:") {
			t.Fatalf("worker marker %q absent after command stream admission failed: %s", text, bytes)
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker marker %q absent: %s (%v)", text, bytes, err)
		}
		if reportWaiting != nil {
			reportWaiting()
			reportWaiting = nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type workerRig struct {
	host       *integrations.Manager
	fixture    *fake.Fixture
	supervisor *integrations.Supervisor
	service    *integrations.VolatileCommandService
	entities   []domain.EntityID
	pilot      *pilot
	admitted   <-chan integrations.AdmittedCommand
	marker     string
}

type capturePilot struct {
	*pilot
	admitted chan integrations.AdmittedCommand
}

func (p *capturePilot) CommandDriver() integrations.CommandDriver { return p }
func (p *capturePilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	select {
	case p.admitted <- command:
	default:
	}
	return p.pilot.Dispatch(ctx, command)
}

func startWorkerRig(t *testing.T, mode string) workerRig {
	return startWorkerRigWithCapture(t, mode, false)
}

func startWorkerRigWithCapture(t *testing.T, mode string, capture bool) workerRig {
	return startWorkerRigWithGate(t, mode, capture, "", "", nil)
}

func startWorkerRigWithGate(t *testing.T, mode string, capture bool, commandGate, marker string, onWaiting func()) workerRig {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha094-synthetic-worker")
	if err != nil {
		t.Fatal(err)
	}
	var keys []integrations.SourceKey
	count := 2
	if mode == "many_hold" {
		count = 33
	}
	for number := 1; number <= count; number++ {
		key, err := integrations.NewSourceKey(fmt.Sprintf("synthetic-device-%d", number), fmt.Sprintf("relay-%d", number))
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	ids, err := fixture.Resolve(ctx, instance, keys)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]integrations.RegistryRecord, count)
	for i := range ids {
		alias, err := domain.ParseHAAlias(fmt.Sprintf("switch.nha094_relay_%d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		records[i], err = integrations.NewRegistryRecord(ids[i].Device, ids[i].Entity, instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewVolatileCommandService(host, fixedGeneration{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	if marker == "" {
		marker = filepath.Join(t.TempDir(), "worker.log")
	}
	var extraArgs []string
	if commandGate != "" {
		extraArgs = []string{"--command-gate", commandGate}
	}
	config := pythonConfig(t, "nha094-synthetic-worker", mode, marker, extraArgs...)
	var spec integrations.PilotSpec
	var launched *pilot
	var admitted chan integrations.AdmittedCommand
	if capture {
		launched = newPilot(config)
		admitted = make(chan integrations.AdmittedCommand, 64)
		wrapped := &capturePilot{pilot: launched, admitted: admitted}
		spec, err = integrations.NewPilotSpec(instance, func() integrations.Pilot { return wrapped })
	} else {
		spec, err = NewPilotSpec(config)
	}
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewRetryPolicy(0, 20*time.Millisecond, time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, policy, integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err := supervisor.Stop(stopCtx); err != nil {
			t.Errorf("supervisor stop: %v", err)
		}
	})
	if !strings.HasPrefix(mode, "incompatible") {
		waitMarker(t, marker, fmt.Sprintf("initial:%d", count), 6*time.Second)
	}
	switch mode {
	case "normal", "new", "new_hold", "crash", "fail", "hold", "many_hold", "stubborn", "orphan_child",
		"small_frame", "large_frame", "secret":
		// The shared client waits for this first RPC's admission headers, then
		// independently requires ALREADY_EXISTS from the second-stream probe.
		// A failure marker preserves its phase/status before this bounded wait.
		waitMarker(t, marker, "second_stream_rejected", 5*time.Second, onWaiting)
	case "incompatible", "incompatible_host_major", "incompatible_host_minor", "incompatible_driver_major",
		"incompatible_digest", "incompatible_version_pair", "incompatible_new_digest",
		"silent", "no_command":
		// These workers intentionally never open a command stream.
	default:
		t.Fatalf("unexpected synthetic worker mode %q", mode)
	}
	entities := make([]domain.EntityID, count)
	for i := range ids {
		entities[i] = ids[i].Entity
	}
	return workerRig{host: host, fixture: fixture, supervisor: supervisor, service: service,
		entities: entities, pilot: launched, admitted: admitted, marker: marker}
}

func commandRequest(t *testing.T, fixture *fake.Fixture, entity domain.EntityID, id string) *actions.CommandRequest {
	t.Helper()
	epoch, err := fixture.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	entityText, err := entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	return &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}},
		CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
}

func TestNHA094PythonWorkerTwoBindingsRouterAndCorrelatedState(t *testing.T) {
	rig := startWorkerRig(t, "normal")
	marker := waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	for _, proof := range []string{"handshake_ok", "rediscovered", "secret_denied", "event_emitted", "initial:1", "initial:2"} {
		if !strings.Contains(marker, proof) {
			t.Fatalf("missing %s: %s", proof, marker)
		}
	}
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	for index, entity := range rig.entities {
		id := fmt.Sprintf("nha094-command-%d", index+1)
		response, err := rig.service.Dispatch(context.Background(), actor, commandRequest(t, rig.fixture, entity, id))
		if err != nil {
			t.Fatal(err)
		}
		if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("router response %d: %v", index+1, response)
		}
		waitMarker(t, rig.marker, fmt.Sprintf("state:%d:%s", index+1, id), 5*time.Second)
		state, present := rig.fixture.State(entity)
		if !present {
			t.Fatalf("missing state for binding %d", index+1)
		}
		value, fresh := state.CriticalValue()
		if !fresh {
			t.Fatalf("uncertain state for binding %d", index+1)
		}
		on, ok := value.Switch()
		if !ok || !on {
			t.Fatalf("wrong binding %d state %v", index+1, value)
		}
	}
	events, err := rig.fixture.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Entity != rig.entities[0] {
		t.Fatalf("worker event was not bound to first entity: %+v", events)
	}
}

func TestNHA094PythonWorkerExactVersionAndFrameLimits(t *testing.T) {
	bad := startWorkerRig(t, "incompatible")
	waitMarker(t, bad.marker, "handshake_rejected:FAILED_PRECONDITION", 6*time.Second)
	small := startWorkerRig(t, "small_frame")
	waitMarker(t, small.marker, "frame_rejected", 5*time.Second)
	large := startWorkerRig(t, "large_frame")
	waitMarker(t, large.marker, "frame_limit:1048576", 5*time.Second)
}

func TestNHA094PythonWorkerCrashAndHeartbeatLease(t *testing.T) {
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	crashed := startWorkerRig(t, "crash")
	response, err := crashed.service.Dispatch(context.Background(), actor,
		commandRequest(t, crashed.fixture, crashed.entities[0], "nha094-crash-command"))
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
		t.Fatal(err)
	}
	if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("crashed driver must remain indeterminate: %v", response)
	}
	waitMarker(t, crashed.marker, "crash_on_command", 5*time.Second)
	instance, err := domain.ParseIntegrationInstanceID("nha094-synthetic-worker")
	if err != nil {
		t.Fatal(err)
	}
	waitPhase := func(supervisor *integrations.Supervisor, timeout time.Duration) {
		deadline := time.Now().Add(timeout)
		for {
			status, ok := supervisor.Status(instance)
			if ok && status.Phase == integrations.PilotDegraded {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("lease remained active: %+v %v", status, ok)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitPhase(crashed.supervisor, 6*time.Second)
	silent := startWorkerRig(t, "silent")
	waitPhase(silent.supervisor, 10*time.Second)
	state, present := silent.fixture.State(silent.entities[0])
	if !present {
		t.Fatal("missing retired state")
	}
	if _, fresh := state.CriticalValue(); fresh {
		t.Fatal("expired heartbeat retained a critical value")
	}
}

func TestNHA094OrphanChildGroupJoinedWhenParentExitsOnTERM(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "orphan_child", true)
	marker := waitMarker(t, rig.marker, "child:", 5*time.Second)
	var childPID int
	for _, line := range strings.Split(marker, "\n") {
		if strings.HasPrefix(line, "child:") {
			var err error
			childPID, err = strconv.Atoi(strings.TrimPrefix(line, "child:"))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if childPID <= 0 {
		t.Fatal("no descendant PID")
	}
	parentPID := rig.pilot.process.Process.Pid
	if group, err := syscall.Getpgid(childPID); err != nil || group != parentPID {
		t.Fatalf("descendant escaped worker process group: group=%d err=%v parent=%d", group, err, parentPID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err := rig.supervisor.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rig.pilot.groupDone:
	default:
		t.Fatal("worker process group was not joined")
	}
	if processGroupAlive(parentPID) {
		t.Fatalf("live worker group %d survived Stop", parentPID)
	}
}

func TestNHA094StubbornWorkerTreeKilledWithinStopBudget(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "stubborn", true)
	waitMarker(t, rig.marker, "child:", 5*time.Second)
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err := rig.supervisor.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > stopTimeout {
		t.Fatalf("stubborn worker exceeded total Stop budget: %v", elapsed)
	}
	select {
	case <-rig.pilot.groupDone:
	default:
		t.Fatal("stubborn process tree did not join")
	}
}

func TestNHA094FullSessionQueueKeepsRouterOutcomeIndeterminate(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "many_hold", true)
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type heldOutcome struct {
		id       string
		response *actions.CommandResponse
		err      error
	}
	done := make(chan heldOutcome, maxPending)
	for index := 0; index < maxPending; index++ {
		id := fmt.Sprintf("nha094-held-%d", index)
		request := commandRequest(t, rig.fixture, rig.entities[index], id)
		go func() {
			response, err := rig.service.Dispatch(ctx, actor, request)
			done <- heldOutcome{id: id, response: response, err: err}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rig.pilot.mu.Lock()
		pending := len(rig.pilot.pending)
		rig.pilot.mu.Unlock()
		if pending == maxPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending=%d, want %d", pending, maxPending)
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := rig.service.Dispatch(context.Background(), actor,
		commandRequest(t, rig.fixture, rig.entities[maxPending], "nha094-over-capacity"))
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil ||
		response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("full worker capacity must retain uncertain route/ledger outcome: %v %v", response, err)
	}
	cancel()
	for range maxPending {
		select {
		case outcome := <-done:
			if !errors.Is(outcome.err, integrations.ErrCommandOutcomeIndeterminate) || outcome.response == nil ||
				outcome.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
				t.Fatalf("held dispatch %s outcome: %v %v", outcome.id, outcome.response, outcome.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("held router dispatch did not join after cancel")
		}
	}
}

func TestNHA094ExactHeartbeatDeadlineRejectsLateAdmission(t *testing.T) {
	p := newPilot(Config{})
	p.active, p.handshaken, p.token, p.frameLimit = true, true, "local-token", 1024
	p.capabilities = map[wire.Capability]bool{wire.Capability_CAPABILITY_PUBLISH: true}
	p.lastHeartbeat = time.Now().Add(-leaseTimeout - time.Millisecond)
	before := p.lastHeartbeat
	request := connect.NewRequest(&wire.HeartbeatRequest{})
	request.Header().Set("Authorization", "Bearer local-token")
	if _, err := p.Heartbeat(context.Background(), request); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("late heartbeat renewed expired lease: %v", err)
	}
	if p.lastHeartbeat != before {
		t.Fatal("late heartbeat changed deadline")
	}
	if err := p.admitted("Bearer local-token", wire.Capability_CAPABILITY_PUBLISH); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("late Publish admission: %v", err)
	}
}

func TestNHA094QueuedCancellationAndExpiryAreNotEmitted(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "normal", true)
	waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	const id = "nha094-queue-witness"
	response, err := rig.service.Dispatch(context.Background(), actor,
		commandRequest(t, rig.fixture, rig.entities[0], id))
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("live command did not reach worker: %v %v", response, err)
	}
	waitMarker(t, rig.marker, "state:1:"+id, 5*time.Second)
	var admitted integrations.AdmittedCommand
	select {
	case admitted = <-rig.admitted:
	case <-time.After(time.Second):
		t.Fatal("route did not provide admitted command")
	}
	if !admitted.ReadyForTransport() {
		t.Fatal("captured command is not a live Host lease")
	}
	wireRequest, err := admitted.Request()
	if err != nil {
		t.Fatal(err)
	}
	cause, err := admitted.Causality()
	if err != nil {
		t.Fatal(err)
	}
	_, causeID, err := cause.Components()
	if err != nil || causeID == nil {
		t.Fatalf("command correlation: %v", err)
	}
	correlationText, err := causeID.Value()
	if err != nil {
		t.Fatal(err)
	}
	entityText, err := rig.entities[0].Value()
	if err != nil {
		t.Fatal(err)
	}
	rig.pilot.mu.Lock()
	handle := append([]byte(nil), rig.pilot.entityHandles[entityText]...)
	rig.pilot.mu.Unlock()
	if len(handle) != 32 {
		t.Fatal("witness binding handle absent")
	}
	witness := &delivery{command: admitted, ctx: context.Background(), expires: time.Now().Add(time.Minute)}
	if !rig.pilot.deliveryEligible(witness) {
		t.Fatal("uncancelled witness was not eligible")
	}
	for _, scenario := range []string{"cancelled", "expired"} {
		ctx, cancel := context.WithCancel(context.Background())
		expires := time.Now().Add(time.Minute)
		if scenario == "cancelled" {
			cancel()
		} else {
			expires = time.Now().Add(-time.Second)
		}
		item := &delivery{frame: &wire.CommandsResponse{Request: wireRequest, Correlation: correlationText,
			BindingHandle: handle}, result: make(chan error, 1), ctx: ctx, expires: expires, command: admitted}
		if rig.pilot.deliveryEligible(item) {
			t.Fatalf("%s valid command remained eligible", scenario)
		}
		rig.pilot.mu.Lock()
		rig.pilot.pending[id] = item
		rig.pilot.correlations[correlationText] = correlation{entity: entityText, expires: expires}
		rig.pilot.mu.Unlock()
		rig.pilot.queue <- item
		select {
		case err := <-item.result:
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%s queued result: %v", scenario, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s queued delivery was not dropped", scenario)
		}
		rig.pilot.mu.Lock()
		_, pending := rig.pilot.pending[id]
		_, retained := rig.pilot.correlations[correlationText]
		rig.pilot.mu.Unlock()
		if pending || retained || item.delivered {
			t.Fatalf("%s reached emission boundary", scenario)
		}
		cancel()
	}
	marker, err := os.ReadFile(rig.marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(marker), "command:1:"+id) != 1 {
		t.Fatal("cancelled or expired queued copy reached Python")
	}
}

func TestNHA094ForeignInstanceBindingRejectedAtRPCBoundary(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-device-1", "relay-1")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := integrations.NewSourceKey("synthetic-device-2", "relay-2")
	if err != nil {
		t.Fatal(err)
	}
	var pilots [2]*pilot
	var specs []integrations.PilotSpec
	var entities [2]domain.EntityID
	var markers [2]string
	var allRecords []integrations.RegistryRecord
	for i := range pilots {
		instanceText := fmt.Sprintf("nha094-foreign-worker-%d", i+1)
		instance, err := domain.ParseIntegrationInstanceID(instanceText)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key, otherKey})
		if err != nil {
			t.Fatal(err)
		}
		records := make([]integrations.RegistryRecord, 2)
		for j, id := range ids {
			alias, err := domain.ParseHAAlias(fmt.Sprintf("switch.nha094_foreign_%d_%d", i+1, j+1))
			if err != nil {
				t.Fatal(err)
			}
			source := key
			if j == 1 {
				source = otherKey
			}
			records[j], err = integrations.NewRegistryRecord(id.Device, id.Entity, instance, source, alias, alias, []domain.HAAlias{alias}, false)
			if err != nil {
				t.Fatal(err)
			}
		}
		allRecords = append(allRecords, records...)
		entities[i] = ids[0].Entity
		markers[i] = filepath.Join(t.TempDir(), "worker.log")
		pilots[i] = newPilot(pythonConfig(t, instanceText, "normal", markers[i]))
		attached := pilots[i]
		spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return attached })
		if err != nil {
			t.Fatal(err)
		}
		specs = append(specs, spec)
	}
	if err := fixture.PublishRegistry(ctx, allRecords); err != nil {
		t.Fatal(err)
	}
	supervisor, err := integrations.NewSupervisor(host, specs, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err := supervisor.Stop(stopCtx); err != nil {
			t.Errorf("supervisor stop: %v", err)
		}
	})
	for _, marker := range markers {
		waitMarker(t, marker, "second_stream_rejected", 6*time.Second)
	}
	pilots[0].mu.Lock()
	var foreignHandle []byte
	for handle := range pilots[0].bindings {
		foreignHandle = []byte(handle)
		break
	}
	pilots[0].mu.Unlock()
	if len(foreignHandle) != 32 {
		t.Fatal("first session has no handle")
	}
	pilots[1].mu.Lock()
	otherToken := pilots[1].token
	pilots[1].mu.Unlock()
	request := connect.NewRequest(&wire.PublishRequest{Observation: &wire.Observation{
		BindingHandle: foreignHandle, Endpoint: "relay",
		Quality: &statev1.Quality{Availability: statev1.Availability_AVAILABILITY_AVAILABLE,
			Freshness: statev1.Freshness_FRESHNESS_FRESH, Provenance: statev1.Provenance_PROVENANCE_OBSERVED},
		Value: &wire.Observation_SwitchOn{SwitchOn: true}}})
	request.Header().Set("Authorization", "Bearer "+otherToken)
	if _, err := pilots[1].Publish(ctx, request); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-instance handle admitted: %v", err)
	}
	state, present := fixture.State(entities[1])
	if !present {
		t.Fatal("second instance state absent")
	}
	value, fresh := state.CriticalValue()
	if !fresh {
		t.Fatal("second instance state became uncertain")
	}
	if on, ok := value.Switch(); !ok || on {
		t.Fatalf("foreign handle mutated second instance: %v", value)
	}
}

func TestNHA094WorkerFailureAndRevokedLateResponse(t *testing.T) {
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	failed := startWorkerRig(t, "fail")
	response, err := failed.service.Dispatch(context.Background(), actor,
		commandRequest(t, failed.fixture, failed.entities[0], "nha094-failed-command"))
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil ||
		response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("worker failure misclassified: %v %v", response, err)
	}
	waitMarker(t, failed.marker, "failed:nha094-failed-command", 5*time.Second)
	marker, err := os.ReadFile(failed.marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(marker), "command:1:nha094-failed-command\n") != 1 ||
		strings.Count(string(marker), "failed:nha094-failed-command\n") != 1 {
		t.Fatalf("failed command was not received and completed exactly once: %s", marker)
	}
	state, present := failed.fixture.State(failed.entities[0])
	if !present {
		t.Fatal("pre-command state missing")
	}
	value, fresh := state.CriticalValue()
	if !fresh {
		t.Fatal("worker failure unexpectedly changed state quality")
	}
	if on, ok := value.Switch(); !ok || on {
		t.Fatalf("worker failure invented physical confirmation: %v", value)
	}

	held := startWorkerRigWithCapture(t, "hold", true)
	held.pilot.mu.Lock()
	token := held.pilot.token
	var binding []byte
	for handle := range held.pilot.bindings {
		binding = []byte(handle)
		break
	}
	held.pilot.mu.Unlock()
	if len(binding) != 32 {
		t.Fatal("missing old session binding")
	}
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		response, err := held.service.Dispatch(context.Background(), actor,
			commandRequest(t, held.fixture, held.entities[0], "nha094-late-command"))
		result <- outcome{response, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		held.pilot.mu.Lock()
		item := held.pilot.pending["nha094-late-command"]
		delivered := item != nil && item.delivered
		held.pilot.mu.Unlock()
		if delivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held command did not enter transport")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	if err := held.supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case got := <-result:
		if !errors.Is(got.err, integrations.ErrCommandOutcomeIndeterminate) || got.response == nil ||
			got.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			t.Fatalf("revoked command outcome: %v %v", got.response, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked command dispatch did not join")
	}
	late := connect.NewRequest(&wire.CompleteCommandRequest{CommandId: "nha094-late-command", Kind: wire.CommandResultKind_COMMAND_RESULT_KIND_ACCEPTED})
	late.Header().Set("Authorization", "Bearer "+token)
	if _, err := held.pilot.CompleteCommand(context.Background(), late); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("old lease accepted completion: %v", err)
	}
	observation := connect.NewRequest(&wire.PublishRequest{Observation: &wire.Observation{BindingHandle: binding}})
	observation.Header().Set("Authorization", "Bearer "+token)
	if _, err := held.pilot.Publish(context.Background(), observation); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("old lease accepted state: %v", err)
	}
}

type heldDiscoverHost struct {
	integrations.SessionHost
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldDiscoverHost) Discover(context.Context, []integrations.Candidate) ([]integrations.Binding, error) {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return nil, integrations.ErrInactiveSession
}

func TestNHA094StopJoinsInflightAndWaitingDiscover(t *testing.T) {
	base, _, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha094-held-discover")
	if err != nil {
		t.Fatal(err)
	}
	session, err := base.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	blocked := &heldDiscoverHost{SessionHost: session, entered: make(chan struct{}), release: make(chan struct{})}
	p := newPilot(Config{})
	p.host, p.ctx, p.active, p.handshaken = blocked, context.Background(), true, true
	p.token, p.frameLimit, p.lastHeartbeat = "local-token", 65536, time.Now()
	p.capabilities = map[wire.Capability]bool{wire.Capability_CAPABILITY_DISCOVER: true}
	request := connect.NewRequest(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{
		Source: &wire.SourceKey{Device: "synthetic-device", Entity: "relay"},
		Descriptions: []*wireregistry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1,
			Endpoint: "relay", DisplayName: "Synthetic", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}}},
	}}})
	request.Header().Set("Authorization", "Bearer local-token")
	finished := make(chan error, 2)
	go func() { _, err := p.Discover(context.Background(), request); finished <- err }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("first Discover did not reach Host")
	}
	go func() { _, err := p.Discover(context.Background(), request); finished <- err }()
	deadline := time.Now().Add(time.Second)
	for len(p.rpcSlots) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("waiting Discover did not reserve global slot: %d", len(p.rpcSlots))
		}
		time.Sleep(time.Millisecond)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- p.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before in-flight Discover joined: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(blocked.release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop failed to join Discover calls")
	}
	for range 2 {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("Discover handler did not return")
		}
	}
}

func TestNHA094UnansweredWorkerCommandExpiresThroughLedger(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "hold", true)
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	request := commandRequest(t, rig.fixture, rig.entities[0], "nha094-unanswered")
	request.ExpiresAt = timestamppb.New(time.Now().Add(300 * time.Millisecond))
	start := time.Now()
	response, err := rig.service.Dispatch(context.Background(), actor, request)
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil ||
		response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("unanswered worker command became certain: %v %v", response, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("command deadline was not enforced: %v", elapsed)
	}
	rig.pilot.mu.Lock()
	pending := len(rig.pilot.pending)
	rig.pilot.mu.Unlock()
	if pending != 0 {
		t.Fatalf("expired command retained %d pending slots", pending)
	}
}
