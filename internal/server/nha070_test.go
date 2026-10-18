package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
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
	mochi "github.com/mochi-mqtt/server/v2"
)

func nha070JournalEvents(t *testing.T, sequence *sequencer.Sequencer, cursor sequencer.Cursor, want int) []sequencer.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _, err := sequence.ReadAfter(cursor, 1024)
		if err != nil {
			t.Fatal(err)
		}
		events := make([]sequencer.Entry, 0)
		for _, entry := range entries {
			if entry.Kind() == sequencer.KindEvent {
				events = append(events, entry)
			}
		}
		if len(events) >= want || time.Now().After(deadline) {
			if len(events) != want {
				for i, event := range events {
					_, source, _, _ := event.Origin()
					t.Logf("unexpected event %d source: %+v", i, source)
				}
				t.Fatalf("MQTT→Host→sequencer events = %d, want %d", len(events), want)
			}
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A fresh battery value proves that the subscribed pilot has consumed a live
// packet after retained discovery. It also fences the retained action replay
// before the exact event-prefix assertion below.
func nha070WaitBatteryCanary(t *testing.T, broker *testbroker.Broker, sequence *sequencer.Sequencer, entity domain.EntityID, value byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		broker.Publish(t, "zigbee2mqtt/IKEA-SW-01/battery", []byte{value}, false)
		state, present, err := sequence.State(entity)
		if err != nil {
			t.Fatal(err)
		}
		if present {
			if current, fresh := state.CriticalValue(); fresh {
				if measurement, ok := current.Measurement(); ok {
					components, err := measurement.Components()
					if err == nil && components.Value == float64(value-'0') {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("live battery canary %q was not admitted", value)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNHA070DefaultRuntimeBrokerHostSequencer(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	const instanceText = "nha070-server-pilot"
	const batteryDiscovery = "homeassistant/sensor/ikea-battery/config"
	const triggerDiscovery = "homeassistant/device_automation/ikea/action_on/config"
	const actionTopic = "zigbee2mqtt/IKEA-SW-01/action"
	configBytes := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nmqtt:\n  profile: '5'\n  broker: 'mqtt://%s'\n  instance: '%s'\n  mode: passive\n  username_secret: mqtt-username\n  password_secret: mqtt-password\n  subscriptions: ['homeassistant/#', 'zigbee2mqtt/#']\n", dataDir, cluster.Port, broker.Address, instanceText))
	c, err := config.ParseWithEnv(configBytes, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
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
	key, err := integrations.NewSourceKey("configured-ikea-wire-id", "synthetic-battery-unique")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := repository.ConfigureBinding(ctx, instance, key, "sensor.ikea_sw_01_battery")
	if err != nil {
		t.Fatal(err)
	}
	settings, enabled := c.MQTTSettings()
	if !enabled {
		t.Fatal("MQTT settings missing")
	}
	// Retained action and state predate subscription. Only discovery may be
	// restored into an admitted route; neither state nor action can emit edge.
	broker.Publish(t, actionTopic, []byte("on"), true)
	broker.Publish(t, "zigbee2mqtt/IKEA-SW-01", []byte(`{"action":"on"}`), true)
	broker.Publish(t, batteryDiscovery, []byte(`{"unique_id":"synthetic-battery-unique","device":{"identifiers":["configured-ikea-wire-id"]},"device_class":"battery","unit_of_measurement":"%","state_topic":"zigbee2mqtt/IKEA-SW-01/battery"}`), true)
	broker.Publish(t, triggerDiscovery, []byte(`{"automation_type":"trigger","type":"action","subtype":"on","payload":"on","topic":"zigbee2mqtt/IKEA-SW-01/action","device":{"identifiers":["configured-ikea-wire-id"]},"origin":{"name":"Zigbee2MQTT","sw_version":"2.14.1"}}`), true)
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
		stop, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := supervisor.Stop(stop); err != nil {
			t.Errorf("supervisor stop: %v", err)
		}
	})
	digest := sha256.Sum256([]byte(instanceText))
	clientID := "homeward-" + hex.EncodeToString(digest[:12])
	var first *mochi.Client
	deadline := time.Now().Add(5 * time.Second)
	for {
		sources, lookupErr := repository.MQTTTopicSources(ctx, instance, batteryDiscovery)
		client, connected := broker.Server.Clients.Get(clientID)
		if lookupErr == nil && len(sources) == 1 && sources[0] == key && connected && client.State.Subscriptions.Len() > 0 {
			first = client
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("configured discovery was not admitted: %v, %v", sources, lookupErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	start, err := sequence.Start()
	if err != nil {
		t.Fatal(err)
	}
	nha070WaitBatteryCanary(t, broker, sequence, identity.Entity, '6')
	nha070JournalEvents(t, sequence, start, 0)
	broker.Publish(t, actionTopic, []byte("on"), false)
	nha070JournalEvents(t, sequence, start, 1)
	snapshot, err := sequence.Capture()
	if err != nil {
		t.Fatal(err)
	}
	baseline := snapshot.Cursor
	// The second pair has identical payloads and no delivery ID.
	broker.Publish(t, actionTopic, []byte("on"), false)
	time.Sleep(100 * time.Millisecond)
	broker.Publish(t, actionTopic, []byte("on"), false)
	events := nha070JournalEvents(t, sequence, baseline, 2)
	for i, entry := range events {
		entity, event, ok := entry.Event()
		if !ok || entity != identity.Entity {
			t.Fatalf("event %d escaped configured anchor", i)
		}
		code, _, sourceSequence, err := event.Components()
		if err != nil || code != "z2m.on" || sourceSequence != nil {
			t.Fatalf("event %d identity: %q %v %v", i, code, sourceSequence, err)
		}
	}
	_, firstStamp, _, hasOrigin := events[0].Origin()
	if !hasOrigin {
		t.Fatal("first accepted action has no session origin")
	}
	firstSession, _, err := firstStamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	first.Stop(fmt.Errorf("synthetic network loss"))
	deadline = time.Now().Add(6 * time.Second)
	for {
		client, ok := broker.Server.Clients.Get(clientID)
		if ok && client != first && !client.Closed() && client.State.Subscriptions.Len() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not reconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	nha070WaitBatteryCanary(t, broker, sequence, identity.Entity, '5')
	nha070JournalEvents(t, sequence, baseline, 2) // retained /action was not replayed
	broker.Publish(t, actionTopic, []byte("on"), false)
	events = nha070JournalEvents(t, sequence, baseline, 3)
	entity, event, ok := events[2].Event()
	if !ok || entity != identity.Entity {
		t.Fatal("post-reconnect action escaped configured anchor")
	}
	code, _, sourceSequence, err := event.Components()
	if err != nil || code != "z2m.on" || sourceSequence != nil {
		t.Fatalf("post-reconnect action: %q %v %v", code, sourceSequence, err)
	}
	_, newStamp, _, hasOrigin := events[2].Origin()
	if !hasOrigin {
		t.Fatal("post-reconnect action has no session origin")
	}
	newSession, _, err := newStamp.Components()
	if err != nil || newSession == firstSession {
		t.Fatalf("post-reconnect action reused old session %q (new %q): %v", firstSession, newSession, err)
	}
	if got := broker.PublishCount(clientID); got != 0 {
		t.Fatalf("passive MQTT pilot emitted %d application PUBLISH packets", got)
	}
}
