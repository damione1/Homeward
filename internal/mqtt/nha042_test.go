package mqtt

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA042WaitOnlyFilterRequiresAndConstructsActualFactory(t *testing.T) {
	rig := newNHA069Rig(t, ModePassive)
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha069-synthetic",
		Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password",
		Subscriptions: []string{"homeassistant/#", "synthetic/wait-only"}})
	if err != nil {
		t.Fatal(err)
	}
	filter := mustNHA069Filter(t, "synthetic/wait-only")
	proof, err := NewTriggerEdgePilot(settings, rig.repo, []TopicFilter{filter})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := proof.Instance()
	if err != nil || instance != rig.instance || !proof.Covers(instance, []TopicFilter{filter}) {
		t.Fatal("actual ingress coverage missing")
	}
	factory, err := proof.Factory()
	if err != nil || factory == nil {
		t.Fatalf("actual factory missing: %v", err)
	}
	// This constructs the real supervised discovery pilot, not a declaration
	// attached to an unrelated factory. The loopback transport proof follows.
	if pilot := factory(); pilot == nil {
		t.Fatal("proof did not construct its real pilot")
	}
	copied := proof.Filters()
	copied[0] = mustNHA069Filter(t, "different/topic")
	if !proof.Covers(instance, []TopicFilter{filter}) {
		t.Fatal("caller mutated private ingress proof")
	}
	other, _ := domain.ParseIntegrationInstanceID("different-instance")
	if proof.Covers(other, []TopicFilter{filter}) {
		t.Fatal("proof crossed integration instances")
	}
	if _, err := NewTriggerEdgePilot(settings, rig.repo, []TopicFilter{mustNHA069Filter(t, "synthetic/never-subscribed")}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("unsubscribed wait-only topic admitted: %v", err)
	}
	if _, err := (TriggerEdgePilot{}).Factory(); !errors.Is(err, ErrInvalidSettings) {
		t.Fatal("zero ingress proof admitted")
	}
}

func TestNHA042RealFactorySubscribesWaitOnlyFilterAndPublishesToExistingSequencer(t *testing.T) {
	broker := testbroker.Start(t)
	topic := "zigbee2mqtt/nha042/wait-only"
	broker.Publish(t, topic, []byte("retained"), true)
	rig := newNHA069Rig(t, ModePassive)
	catalogue, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := integrations.NewHost(rig.fixture, rig.fixture.Sequencer(), catalogue)
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.OpenSession(rig.instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnableNativeMQTTEdges(rig.instance); err != nil {
		t.Fatal(err)
	}
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic",
		Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password",
		Subscriptions: []string{"homeassistant/#", topic}})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := NewTriggerEdgePilot(settings, rig.repo, []TopicFilter{mustNHA069Filter(t, topic)})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := proof.Factory()
	if err != nil {
		t.Fatal(err)
	}
	pilot, ok := factory().(*discoveryPilot)
	if !ok || pilot == nil {
		t.Fatal("actual discovery pilot missing")
	}
	cursor, err := rig.fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	tasks := newNHA068Tasks()
	host := nha068BindingHost{Session: session, provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot.Pilot) })
	// Start returns after the real broker's successful SUBACK. There is no
	// automation trigger for this topic: it exists solely for script waits.
	readEdges := func() []integrations.NativeMQTTEdge {
		t.Helper()
		entries, _, err := rig.fixture.Sequencer().ReadAfter(cursor, 256)
		if err != nil {
			t.Fatal(err)
		}
		edges := make([]integrations.NativeMQTTEdge, 0)
		for _, entry := range entries {
			if edge, present := entry.NativeMQTTEdge(); present {
				edges = append(edges, edge)
			}
		}
		return edges
	}
	for count := 1; count <= 2; count++ {
		broker.Publish(t, topic, []byte("live"), false)
		deadline := time.Now().Add(time.Second)
		for len(readEdges()) < count && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		edges := readEdges()
		if len(edges) != count {
			t.Fatalf("wait-only actual broker subscription: %d edges, want %d", len(edges), count)
		}
		for _, edge := range edges {
			actualTopic, payload, sequence, err := edge.Components()
			if err != nil || actualTopic != topic || string(payload) != "live" || sequence == 0 {
				t.Fatalf("retained or invalid edge reached shared sequencer: %q %q %d %v", actualTopic, payload, sequence, err)
			}
		}
	}
}
