package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

func TestNHA069RunAndSharedDefaultPlanAdmitConfiguredDiscovery(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	const instanceText = "nha069-server-pilot"
	const discoveryTopic = "homeassistant/sensor/nha069-power/config"
	const stateTopic = "zigbee2mqtt/nha069-power/state"
	configBytes := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nmqtt:\n  profile: '5'\n  broker: 'mqtt://%s'\n  instance: '%s'\n  mode: passive\n  username_secret: mqtt-username\n  password_secret: mqtt-password\n  subscriptions: ['homeassistant/#', 'zigbee2mqtt/#']\n", dataDir, cluster.Port, broker.Address, instanceText))
	c, err := config.ParseWithEnv(configBytes, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	startup := nha029RunStartupFromRaw(t, configBytes)
	pg, err := c.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, pg)
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
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if err := secrets.RotateCredential(ctx, nha051ServerHandle(t, instanceText, name), secret); err != nil {
			t.Fatal(err)
		}
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
	key, err := integrations.NewSourceKey("configured-wire-device", "wire-power-unique")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := repository.ConfigureBinding(ctx, instance, key, "sensor.washing_machine_power")
	if err != nil {
		t.Fatal(err)
	}
	settings, enabled := c.MQTTSettings()
	if !enabled {
		t.Fatal("MQTT configuration not parsed")
	}
	broker.Publish(t, stateTopic, []byte(`{"power":10.5}`), true)
	broker.Publish(t, discoveryTopic, []byte(`{"unique_id":"wire-power-unique","device":{"identifiers":["other-wire-id","configured-wire-device"]},"device_class":"power","unit_of_measurement":"W","state_class":"measurement","state_topic":"zigbee2mqtt/nha069-power/state","value_template":"{{ value_json.power }}"}`), true)
	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	var listen net.ListenConfig
	go func() {
		runDone <- RunWithStartup(runCtx, startup, listen.Listen, func(address net.Addr) { ready <- address })
	}()
	select {
	case <-ready:
	case err := <-runDone:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not start")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		sources, err := repository.MQTTTopicSources(ctx, instance, discoveryTopic)
		if err == nil && len(sources) == 1 && sources[0] == key {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Run did not persist configured discovery provenance: %v %v", sources, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Run did not join discovery pilot")
	}
	// Run and this inspection use the exact same default plan constructor.
	// The second session replays the retained broker snapshot into one Host and
	// its sequencer; no serving API from NHA-147 is assumed.
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := defaultMQTTRuntimePlan(settings, store, sequence)
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
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer stopCancel()
		if err := supervisor.Stop(stopCtx); err != nil {
			t.Errorf("supervisor stop: %v", err)
		}
	})
	deadline = time.Now().Add(5 * time.Second)
	for {
		state, present, stateErr := sequence.State(identity.Entity)
		descriptions, capabilityErr := host.Capabilities(identity.Entity)
		if stateErr == nil && present && capabilityErr == nil && len(descriptions) == 1 {
			_, quality, _, _, _, err := state.Components()
			if err != nil {
				t.Fatal(err)
			}
			a, f, p, _ := quality.Components()
			if a != domain.AvailabilityAvailable || f != domain.FreshnessStale || p != domain.ProvenanceRestored || descriptions[0].GetMeasurement() == nil || descriptions[0].GetMeasurement().Unit != "W" {
				t.Fatalf("shared Host/sequencer admission: %v/%v/%v %+v", a, f, p, descriptions)
			}
			if _, critical := state.CriticalValue(); critical {
				t.Fatal("restored MQTT snapshot became critical")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared plan did not admit state/capability: state=%v/%v/%v capability=%v/%v", state, present, stateErr, descriptions, capabilityErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	digest := sha256.Sum256([]byte(instanceText))
	clientID := "homeward-" + hex.EncodeToString(digest[:12])
	if got := broker.PublishCount(clientID); got != 0 {
		t.Fatalf("default passive Run/plan emitted %d application publishes", got)
	}
}
