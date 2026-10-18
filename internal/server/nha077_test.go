package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha077BridgeInfo struct {
	Settings   string `json:"settings"`
	CA         string `json:"ca"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
	Control    string `json:"control"`
}

type nha077Function struct {
	SourceDevice string `json:"source_device"`
	SourceEntity string `json:"source_entity"`
	ZoneID       string `json:"zone_id"`
}

type nha077ControlReply struct {
	Requests      [][]json.RawMessage `json:"requests"`
	Connected     bool                `json:"connected"`
	Entered       map[string]bool     `json:"entered"`
	Subscriptions []string            `json:"subscriptions"`
}

func nha077Await(t *testing.T, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func nha077Control(t *testing.T, socket string, command any) nha077ControlReply {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(command); err != nil {
		t.Fatal(err)
	}
	var reply nha077ControlReply
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func nha077RequestCount(reply nha077ControlReply, operation, url string) int {
	count := 0
	for _, frame := range reply.Requests {
		if len(frame) < 2 {
			continue
		}
		var gotOperation, gotURL string
		_ = json.Unmarshal(frame[0], &gotOperation)
		_ = json.Unmarshal(frame[1], &gotURL)
		if gotOperation == operation && (url == "" || gotURL == url) {
			count++
		}
	}
	return count
}

func nha077DispatchWithBridgeBarrier(t *testing.T, ctx context.Context,
	service *integrations.VolatileCommandService, actor domain.ActorID,
	control, operation, commandURL string, request *actions.CommandRequest) *actions.CommandResponse {
	t.Helper()
	before := nha077RequestCount(nha077Control(t, control, map[string]any{"op": "snapshot"}), operation, commandURL)
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := service.Dispatch(ctx, actor, request)
		done <- result{response, err}
	}()
	nha077Await(t, "command reaches fake bridge before LEAP ACK", func() bool {
		return nha077RequestCount(nha077Control(t, control, map[string]any{"op": "snapshot"}), operation, commandURL) > before
	})
	nha077Control(t, control, map[string]any{"op": "release_command_response", "command_url": commandURL})
	select {
	case got := <-done:
		if got.err != nil || got.response == nil || got.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("dispatch after real bridge delivery: %v %v", got.response, got.err)
		}
		return got.response
	case <-time.After(8 * time.Second):
		t.Fatal("dispatch did not finish after LEAP ACK release")
		return nil
	}
}

func TestNHA077ConfiguredCasetaWorkerTLSHostPostgres(t *testing.T) {
	cluster := testpg.Start(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pythonBytes, err := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
	if err != nil {
		t.Fatalf("run scripts/ensure-worker-python.sh: %v", err)
	}
	python := strings.TrimSpace(string(pythonBytes))
	fixtureDir, err := os.MkdirTemp("/tmp", "nha077-fake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fixtureDir) })
	fixtureCtx, fixtureCancel := context.WithCancel(context.Background())
	fixture := exec.CommandContext(fixtureCtx, python, filepath.Join(root, "testworker/caseta_test_server.py"),
		"--root", fixtureDir, "--reconcile-seconds", "1")
	fixture.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var fixtureLog bytes.Buffer
	fixture.Stdout, fixture.Stderr = &fixtureLog, &fixtureLog
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fixtureCancel(); _ = fixture.Wait() })
	readyPath := filepath.Join(fixtureDir, "ready.json")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("fake bridge TLS startup: %v: %s", err, fixtureLog.String())
	}
	readyData, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	var bridge nha077BridgeInfo
	if err := json.Unmarshal(readyData, &bridge); err != nil {
		t.Fatal(err)
	}
	settingsData, err := os.ReadFile(bridge.Settings)
	if err != nil {
		t.Fatal(err)
	}
	var mapping struct {
		Functions map[string]nha077Function `json:"functions"`
	}
	if err := json.Unmarshal(settingsData, &mapping); err != nil {
		t.Fatal(err)
	}
	if len(mapping.Functions) != 13 {
		t.Fatalf("function map: %d", len(mapping.Functions))
	}
	aliases := make([]string, 0, len(mapping.Functions))
	for alias := range mapping.Functions {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	dataDir := filepath.Join(t.TempDir(), "data")
	const instanceText = "nha077-caseta"
	var yaml strings.Builder
	fmt.Fprintf(&yaml, "listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nexternal_workers:\n  - instance: %q\n    executable: %q\n    args: [%q, %q, %q]\n    secret_names: [fake-ca, fake-cert, fake-key]\n    bindings:\n",
		dataDir, cluster.Port, instanceText, python, filepath.Join(root, "drivers/caseta/worker.py"), "--config", bridge.Settings)
	for _, alias := range aliases {
		f := mapping.Functions[alias]
		fmt.Fprintf(&yaml, "      - {device: %q, entity: %q, alias: %q}\n", f.SourceDevice, f.SourceEntity, alias)
	}
	c, err := config.ParseWithEnv([]byte(yaml.String()), func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	pgSettings, err := c.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, pgSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"fake-ca": bridge.CA, "fake-cert": bridge.ClientCert, "fake-key": bridge.ClientKey} {
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := storage.NewSecretValue(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := secrets.RotateCredential(ctx, nha051ServerHandle(t, instanceText, name), secret); err != nil {
			t.Fatal(err)
		}
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := defaultConfiguredRuntimePlan(mqtt.Settings{}, false, c.ExternalWorkers(), store, sequence)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(plan.Identity, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(secrets); err != nil {
		t.Fatal(err)
	}
	if err := plan.Configure(ctx, host, store); err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("trusted-caseta-test")
	if err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewVolatileCommandService(host, nha094Generation{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
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
			t.Errorf("stop worker: %v", err)
		}
	})
	instance, err := domain.ParseIntegrationInstanceID(instanceText)
	if err != nil {
		t.Fatal(err)
	}
	entities := make(map[string]domain.EntityID, 13)
	for _, alias := range aliases {
		f := mapping.Functions[alias]
		key, err := integrations.NewSourceKey(f.SourceDevice, f.SourceEntity)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := plan.Identity.Resolve(ctx, instance, []integrations.SourceKey{key})
		if err != nil || len(resolved) != 1 {
			t.Fatalf("resolve %s: %v", alias, err)
		}
		entities[alias] = resolved[0].Entity
	}
	nha077Await(t, "13 fresh states and ready after subscriptions", func() bool {
		snapshot, err := sequence.Capture()
		if err != nil || len(snapshot.States) != 13 {
			return false
		}
		report, ok := snapshot.Health[instance]
		if !ok {
			return false
		}
		status, _, err := report.Components()
		if err != nil || status != integrations.HealthReady {
			return false
		}
		for _, entity := range entities {
			state, present := snapshot.States[entity]
			if !present {
				return false
			}
			if _, fresh := state.CriticalValue(); !fresh {
				return false
			}
		}
		return true
	})
	initial := nha077Control(t, bridge.Control, map[string]any{"op": "snapshot"})
	if !initial.Connected || len(initial.Subscriptions) != 2 || nha077RequestCount(initial, "CreateRequest", "") != 0 || nha077RequestCount(initial, "UpdateRequest", "") != 0 {
		t.Fatalf("startup mutated fake bridge or missed subscriptions: %+v", initial)
	}
	for _, alias := range aliases {
		value, _ := func() (domain.StateValue, bool) {
			state, present, err := sequence.State(entities[alias])
			if err != nil || !present {
				t.Fatalf("state %s: %v", alias, err)
			}
			return state.CriticalValue()
		}()
		if alias == "fan.living_room_ceiling_fan" {
			if speed, ok := value.Fan(); !ok || speed != domain.FanSpeedOff {
				t.Fatalf("fan initial speed: %v", value)
			}
		}
	}
	nha077Await(t, "multiple healthy reconciliation periods", func() bool {
		snapshot := nha077Control(t, bridge.Control, map[string]any{"op": "snapshot"})
		return nha077RequestCount(snapshot, "ReadRequest", "/zone/2/status") >= 3
	})
	status, ok := supervisor.Status(instance)
	if !ok || status.Phase != integrations.PilotRunning || status.Attempts != 1 {
		t.Fatalf("healthy bridge consumed retry budget: %+v", status)
	}
	lightAlias := "light.living_room_lights"
	light := mapping.Functions[lightAlias]
	if light.ZoneID == "" {
		t.Fatal("test light source missing explicit zone")
	}
	lightURL := "/zone/" + light.ZoneID + "/status"
	nha077Control(t, bridge.Control, map[string]any{"op": "arm_after_command", "command_url": "/zone/" + light.ZoneID + "/commandprocessor", "read_url": lightURL})
	epoch, err := sequence.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	lightEntity, _ := entities[lightAlias].Value()
	level := 42.0
	on := true
	request := &actions.CommandRequest{CommandId: "nha077-light-42", AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: lightEntity}},
		CapabilitySchemaId: contracts.LightSchema, Endpoint: "level",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, BrightnessPercent: &level}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	nha077DispatchWithBridgeBarrier(t, ctx, service, actor, bridge.Control,
		"CreateRequest", "/zone/"+light.ZoneID+"/commandprocessor", request)
	commandID, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	nha077Await(t, "post-ACK LEAP read blocked", func() bool {
		return nha077Control(t, bridge.Control, map[string]any{"op": "entered", "operation": "ReadRequest", "url": lightURL}).Entered["ReadRequest "+lightURL]
	})
	ackDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(ackDeadline) {
		progress, err := host.ActionStatus(commandID)
		if err == nil && progress.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	progress, err := host.ActionStatus(commandID)
	if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		bridgeState := nha077Control(t, bridge.Control, map[string]any{"op": "snapshot"})
		pilotState, _ := supervisor.Status(instance)
		t.Fatalf("transport ACK absent: %+v %v; pilot=%+v; LEAP=%+v", progress, err, pilotState, bridgeState)
	}
	if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatalf("ACK distinct from observation: %+v %v", progress, err)
	}
	nha077Control(t, bridge.Control, map[string]any{"op": "level", "zone": light.ZoneID, "value": 42, "notify": true})
	progress, err = host.ActionStatus(commandID)
	if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatalf("coincident notification confirmed: %+v %v", progress, err)
	}
	nha077Control(t, bridge.Control, map[string]any{"op": "release", "operation": "ReadRequest", "url": lightURL})
	nha077Await(t, "post-ACK read confirms complete light value", func() bool {
		progress, err := host.ActionStatus(commandID)
		return err == nil && progress.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED
	})
	fanAlias := "fan.living_room_ceiling_fan"
	fanURL := "/zone/" + mapping.Functions[fanAlias].ZoneID + "/status"
	nha077Control(t, bridge.Control, map[string]any{"op": "arm_after_command", "command_url": "/zone/" + mapping.Functions[fanAlias].ZoneID + "/commandprocessor", "read_url": fanURL})
	fanEntity, _ := entities[fanAlias].Value()
	high := "High"
	fanRequest := &actions.CommandRequest{CommandId: "nha077-fan-high", AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: fanEntity}},
		CapabilitySchemaId: contracts.FanSchema, Endpoint: "speed",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &high}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	nha077DispatchWithBridgeBarrier(t, ctx, service, actor, bridge.Control,
		"CreateRequest", "/zone/"+mapping.Functions[fanAlias].ZoneID+"/commandprocessor", fanRequest)
	fanID, err := domain.ParseCommandID(fanRequest.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	nha077Await(t, "fan ACK before post-command read", func() bool {
		progress, err := host.ActionStatus(fanID)
		return err == nil && progress.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
	})
	nha077Control(t, bridge.Control, map[string]any{"op": "fan", "speed": "High", "notify": true})
	progress, err = host.ActionStatus(fanID)
	if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatalf("fan notification confirmed without read: %+v %v", progress, err)
	}
	nha077Control(t, bridge.Control, map[string]any{"op": "release", "operation": "ReadRequest", "url": fanURL})
	nha077Await(t, "structured fan speed confirmed by post-ACK read", func() bool {
		progress, err := host.ActionStatus(fanID)
		return err == nil && progress.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED
	})
	fanState, present, err := sequence.State(entities[fanAlias])
	if err != nil || !present {
		t.Fatalf("fan state: %v", err)
	}
	fanValue, fresh := fanState.CriticalValue()
	speed, isFan := fanValue.Fan()
	if !fresh || !isFan || speed != domain.FanSpeedHigh {
		t.Fatalf("fan state not complete: %v", fanValue)
	}
	awayAlias := "switch.unassigned_smart_away"
	awayEntity, _ := entities[awayAlias].Value()
	const awayURL = "/system/away/1/status"
	nha077Control(t, bridge.Control, map[string]any{"op": "arm_after_command", "command_url": awayURL, "read_url": awayURL})
	awayRequest := &actions.CommandRequest{CommandId: "nha077-away-on", AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: awayEntity}},
		CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "away",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	nha077DispatchWithBridgeBarrier(t, ctx, service, actor, bridge.Control,
		"UpdateRequest", awayURL, awayRequest)
	awayID, err := domain.ParseCommandID(awayRequest.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	nha077Await(t, "Smart Away ACK is not state", func() bool {
		progress, err := host.ActionStatus(awayID)
		return err == nil && progress.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
	})
	nha077Await(t, "Smart Away post-ACK read blocked", func() bool {
		return nha077Control(t, bridge.Control, map[string]any{"op": "entered", "operation": "ReadRequest", "url": awayURL}).Entered["ReadRequest "+awayURL]
	})
	nha077Control(t, bridge.Control, map[string]any{"op": "away", "enabled": true, "notify": true})
	progress, err = host.ActionStatus(awayID)
	if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatalf("Smart Away notification forged confirmation: %+v %v", progress, err)
	}
	nha077Control(t, bridge.Control, map[string]any{"op": "release", "operation": "ReadRequest", "url": awayURL})
	nha077Await(t, "Smart Away confirmed by post-ACK read", func() bool {
		progress, err := host.ActionStatus(awayID)
		return err == nil && progress.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED
	})
	nha077Control(t, bridge.Control, map[string]any{"op": "pico", "event": "Press"})
	nha077Control(t, bridge.Control, map[string]any{"op": "pico", "event": "Release"})
	nha077Await(t, "Pico press and release events", func() bool {
		cursor, err := sequence.Start()
		if err != nil {
			t.Fatal(err)
		}
		entries, _, err := sequence.ReadAfter(cursor, 1024)
		if err != nil {
			t.Fatal(err)
		}
		press, release := false, false
		for _, entry := range entries {
			_, event, ok := entry.Event()
			if !ok {
				continue
			}
			code, _, _, err := event.Components()
			if err != nil {
				t.Fatal(err)
			}
			press = press || code == "pico.press"
			release = release || code == "pico.release"
		}
		return press && release
	})
	beforeExpire := nha077Control(t, bridge.Control, map[string]any{"op": "snapshot"})
	beforeCommands := nha077RequestCount(beforeExpire, "CreateRequest", "") + nha077RequestCount(beforeExpire, "UpdateRequest", "")
	nha077Control(t, bridge.Control, map[string]any{"op": "expire", "url": "/system/away/1/status"})
	nha077Await(t, "expired subscription causes a supervised new attempt", func() bool {
		status, ok := supervisor.Status(instance)
		return ok && status.Attempts >= 2
	})
	nha077Await(t, "reconnected worker reconciles before ready", func() bool {
		snapshot, err := sequence.Capture()
		if err != nil {
			return false
		}
		report, ok := snapshot.Health[instance]
		if !ok {
			return false
		}
		status, _, err := report.Components()
		if err != nil || status != integrations.HealthReady {
			return false
		}
		worker, ok := supervisor.Status(instance)
		return ok && worker.Phase == integrations.PilotRunning && worker.Attempts == 2
	})
	afterExpire := nha077Control(t, bridge.Control, map[string]any{"op": "snapshot"})
	if got := nha077RequestCount(afterExpire, "CreateRequest", "") + nha077RequestCount(afterExpire, "UpdateRequest", ""); got != beforeCommands {
		t.Fatalf("automatic command during reconnect: before=%d after=%d", beforeCommands, got)
	}
}
