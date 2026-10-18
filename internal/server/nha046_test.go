package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// This follows the default plan consumed by Run through YAML, Configure and
// the real isolated worker/Host. It does not invoke Run or send a command.
func TestNHA046ConfiguredRunPlanJoinsFanOptInToWorkerAndHost(t *testing.T) {
	cluster := testpg.Start(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pythonPath, err := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
	if err != nil {
		t.Fatal(err)
	}
	python := strings.TrimSpace(string(pythonPath))
	fixtureDir, err := os.MkdirTemp("/tmp", "nha046-fake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fixtureDir) })
	fixtureCtx, cancelFixture := context.WithCancel(context.Background())
	fixture := exec.CommandContext(fixtureCtx, python, filepath.Join(root, "testworker/caseta_test_server.py"),
		"--root", fixtureDir, "--reconcile-seconds", "1")
	fixture.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var fixtureLog bytes.Buffer
	fixture.Stdout, fixture.Stderr = &fixtureLog, &fixtureLog
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancelFixture(); _ = fixture.Wait() })
	readyPath := filepath.Join(fixtureDir, "ready.json")
	nha077Await(t, "synthetic Caseta bridge", func() bool { _, err := os.Stat(readyPath); return err == nil })
	ready, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	var bridge nha077BridgeInfo
	if err := json.Unmarshal(ready, &bridge); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(bridge.Settings)
	if err != nil {
		t.Fatal(err)
	}
	var mapping struct {
		Functions map[string]nha077Function `json:"functions"`
	}
	if err := json.Unmarshal(settings, &mapping); err != nil {
		t.Fatal(err)
	}
	const aliasText = "fan.living_room_ceiling_fan"
	if len(mapping.Functions) != 13 || mapping.Functions[aliasText].SourceDevice == "" {
		t.Fatal("synthetic Caseta fixture lost its qualified fan source")
	}
	aliases := make([]string, 0, len(mapping.Functions))
	for alias := range mapping.Functions {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	dataDir := filepath.Join(t.TempDir(), "data")
	const instanceText = "nha046-caseta-run"
	var yaml strings.Builder
	fmt.Fprintf(&yaml, "listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nexternal_workers:\n  - instance: %q\n    executable: %q\n    args: [%q, %q, %q, %q, %q]\n    secret_names: [fake-ca, fake-cert, fake-key]\n    bindings:\n",
		dataDir, cluster.Port, instanceText, python, filepath.Join(root, "drivers/caseta/worker.py"), "--config", bridge.Settings, "--host-profile", "1.3")
	for _, alias := range aliases {
		f := mapping.Functions[alias]
		if alias == aliasText {
			fmt.Fprintf(&yaml, "      - {device: %q, entity: %q, alias: %q, ha_fan_profile: caseta_ha_fan_4_v1}\n", f.SourceDevice, f.SourceEntity, alias)
		} else {
			fmt.Fprintf(&yaml, "      - {device: %q, entity: %q, alias: %q}\n", f.SourceDevice, f.SourceEntity, alias)
		}
	}
	parsed, err := config.ParseWithEnv([]byte(yaml.String()), func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	workers := parsed.ExternalWorkers()
	if len(workers) != 1 || len(workers[0].Bindings()) != 13 {
		t.Fatal("configured Run lost the worker bindings")
	}
	pgSettings, err := parsed.PostgresSettings()
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
		handle := nha051ServerHandle(t, instanceText, name)
		if err := secrets.RotateCredential(ctx, handle, secret); err != nil {
			t.Fatal(err)
		}
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := defaultConfiguredRuntimePlan(mqtt.Settings{}, false, workers, store, sequence)
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
			t.Errorf("stop synthetic worker: %v", err)
		}
	})
	instance, err := domain.ParseIntegrationInstanceID(instanceText)
	if err != nil {
		t.Fatal(err)
	}
	function := mapping.Functions[aliasText]
	key, err := integrations.NewSourceKey(function.SourceDevice, function.SourceEntity)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := plan.Identity.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		t.Fatalf("configured SourceKey: %v", err)
	}
	nha077Await(t, "observed configured Caseta fan", func() bool {
		state, present, err := sequence.State(identities[0].Entity)
		if err != nil || !present {
			return false
		}
		_, fresh := state.CriticalValue()
		return fresh
	})
	alias, err := domain.ParseHAAlias(aliasText)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := service.CaptureFan(ctx, alias)
	if err != nil {
		t.Fatalf("configured worker did not attach private Caseta fan profile: %v", err)
	}
	percentage := uint8(75)
	id, err := domain.ParseCommandID("nha046-run-plan-fan-capture")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("trusted-nha046-run")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := capture.PrepareHA(actor, id, time.Now().Add(time.Minute), true, &percentage)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := prepared.Request()
	if err != nil || wire.GetArguments().GetSetFan().GetSpeed() != "MediumHigh" || wire.GetArguments().GetSetFan().On != nil {
		t.Fatalf("HA75 did not use configured Caseta four-speed fan profile: %v %v", wire, err)
	}
	snapshot := nha077Control(t, bridge.Control, map[string]any{"op": "snapshot"})
	if nha077RequestCount(snapshot, "CreateRequest", "") != 0 || nha077RequestCount(snapshot, "UpdateRequest", "") != 0 {
		t.Fatal("read-only composition sent a command to the synthetic bridge")
	}
}
