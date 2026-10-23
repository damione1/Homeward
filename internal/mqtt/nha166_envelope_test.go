package mqtt

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
)

// The bytes are synthetic. Their length matches the passive packet-size
// observation; no broker payload or household identifier enters this fixture.
func TestNHA166RetainedSyntheticBridgeMetadata109418AcceptedByPassiveIngress(t *testing.T) {
	broker := testbroker.Start(t)
	const topic = "zigbee2mqtt/bridge/synthetic-metadata"
	payload := bytes.Repeat([]byte{'S'}, 109418)
	broker.Publish(t, topic, payload, true)

	settings, err := ParseSettings(Input{
		Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha166-envelope-fixture",
		Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password",
		Subscriptions: []string{"zigbee2mqtt/#"},
	})
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan Observation, 1)
	pilot, err := NewPilot(settings, nha068Capture{observations: captured})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := nha056Journal(t)
	cells, err := journal.Cells()
	if err != nil {
		t.Fatal(err)
	}
	if err := pilot.AttachObservation(cells, journal); err != nil {
		t.Fatal(err)
	}
	tasks := newNHA068Tasks()
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 4)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = pilot.Start(ctx, host, tasks)
	cancel()
	if err != nil {
		tasks.cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })

	select {
	case observation := <-captured:
		if observation.class != ObservationRestored {
			t.Fatal("retained metadata was not classified as restoration")
		}
		gotTopic, gotPayload, _, err := observation.Record()
		if err != nil {
			t.Fatal(err)
		}
		value, _ := gotTopic.Value()
		if value != topic || !bytes.Equal(gotPayload, payload) {
			t.Fatal("synthetic metadata was not delivered intact")
		}
		gotPayload[0] = 'X'
		if observation.payload[0] != 'S' {
			t.Fatal("Record exposed the queued packet buffer")
		}
	case err := <-tasks.errors:
		t.Fatalf("passive metadata ingress failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic retained metadata was not consumed")
	}
	queue, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND)
	if err != nil || queue.GetInstalled() == nil {
		t.Fatal("MQTT queue unavailable after metadata delivery", err)
	}
	installed := queue.GetInstalled()
	if installed.CapacityEntries.GetValue() != queueCapacity || installed.RejectedTotal.GetExact() != 0 || installed.LostDeliveriesTotal.GetExact() != 0 {
		t.Fatal("passive metadata delivery changed queue bounds or lost a packet")
	}
	if broker.PublishCount(settings.clientID) != 0 {
		t.Fatal("passive metadata ingestion sent an application publication")
	}
}

func TestNHA166InboundEnvelopePreservesDiscoveryAndCommandPayloadLimits(t *testing.T) {
	prefix := `{"synthetic":"`
	suffix := `"}`
	largeDocument := []byte(prefix + strings.Repeat("S", maxPayload+1-len(prefix)-len(suffix)) + suffix)
	if len(largeDocument) != maxPayload+1 {
		t.Fatal("synthetic discovery document fixture has the wrong length")
	}
	if _, err := decodeDiscoveryJSON(largeDocument); !errors.Is(err, errDiscoveryProfile) {
		t.Fatalf("oversized discovery document was admitted: %v", err)
	}
	topic, err := ParseTopic("zigbee2mqtt/synthetic/set")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCommandPublication(topic, bytes.Repeat([]byte{'S'}, maxPayload+1)); !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("oversized outbound command was admitted: %v", err)
	}
}
