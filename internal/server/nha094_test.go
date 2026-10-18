package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha094Generation struct{}

func (nha094Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

func nha094WaitMarker(t *testing.T, marker, fragment string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		bytes, err := os.ReadFile(marker)
		if err == nil && strings.Contains(string(bytes), fragment) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker marker %q missing: %s (%v)", fragment, bytes, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNHA094RunConfiguredWorkerAndSharedCommandStatePlan(t *testing.T) {
	cluster := testpg.Start(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pythonPath, err := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
	if err != nil {
		t.Fatalf("run scripts/ensure-worker-python.sh: %v", err)
	}
	workerScript := filepath.Join(root, "testworker/fake_worker.py")
	marker := filepath.Join(t.TempDir(), "worker.log")
	dataDir := filepath.Join(t.TempDir(), "data")
	const instanceText = "nha094-server-worker"
	configuration := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nexternal_workers:\n  - instance: %q\n    executable: %q\n    args: [%q, %q, %q, %q]\n    secret_names: [nha094-token]\n",
		dataDir, cluster.Port, instanceText, strings.TrimSpace(string(pythonPath)), workerScript, "--mode=secret", "--marker", marker))
	c, err := config.ParseWithEnv(configuration, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	settings, err := c.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedSecrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	seedValue, err := storage.NewSecretValue([]byte("isolated-NHA094-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seedSecrets.RotateCredential(ctx, nha051ServerHandle(t, instanceText, "nha094-token"), seedValue); err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID(instanceText)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{"switch.arc_soundbar_crossfade", "switch.arc_soundbar_loudness"}
	var entities [2]domain.EntityID
	for i := range aliases {
		key, err := integrations.NewSourceKey(fmt.Sprintf("synthetic-device-%d", i+1), fmt.Sprintf("relay-%d", i+1))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := repository.ConfigureBinding(ctx, instance, key, aliases[i])
		if err != nil {
			t.Fatal(err)
		}
		entities[i] = identity.Entity
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	ready := make(chan net.Addr, 1)
	runDone := make(chan error, 1)
	var listen net.ListenConfig
	go func() { runDone <- Run(runCtx, c, listen.Listen, func(address net.Addr) { ready <- address }) }()
	select {
	case <-ready:
	case err := <-runDone:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not serve")
	}
	nha094WaitMarker(t, marker, "second_stream_rejected")
	nha094WaitMarker(t, marker, "secret_read")
	cancelRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Run did not join worker")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The same constructor used by Run now exposes its Host/sequencer to the
	// existing command service; NHA-147's public API is deliberately absent.
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
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
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
			t.Errorf("supervisor stop: %v", err)
		}
	})
	nha094WaitMarker(t, marker, "second_stream_rejected")
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := sequence.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	for i, entity := range entities {
		state, present, err := sequence.State(entity)
		if err != nil || !present {
			t.Fatalf("worker state %d absent: %v", i+1, err)
		}
		value, fresh := state.CriticalValue()
		if !fresh {
			t.Fatalf("worker state %d not fresh", i+1)
		}
		if on, ok := value.Switch(); !ok || on {
			t.Fatalf("worker initial state %d: %v", i+1, value)
		}
		entityText, _ := entity.Value()
		id := fmt.Sprintf("nha094-server-command-%d", i+1)
		request := &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7,
			Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}},
			CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay",
			Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}},
			ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
		response, err := service.Dispatch(ctx, actor, request)
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("shared plan command %d: %v %v", i+1, response, err)
		}
		nha094WaitMarker(t, marker, fmt.Sprintf("state:%d:%s", i+1, id))
	}
}
