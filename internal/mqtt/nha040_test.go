package mqtt

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtttopic"
)

type nha040EdgeHost struct {
	*integrations.Session
	edges []integrations.NativeMQTTEdge
}

func (h *nha040EdgeHost) PublishNativeMQTTEdge(_ context.Context, edge integrations.NativeMQTTEdge) error {
	h.edges = append(h.edges, edge)
	return nil
}

func TestNHA040MQTTTopicValidatorParity(t *testing.T) {
	for _, name := range []string{"zigbee2mqtt/button/action", "$SYS/broker/uptime", "a/b", "éclairage/bouton"} {
		topic, err := ParseTopic(name)
		if err != nil || !mqtttopic.ValidName(name) {
			t.Fatalf("valid publication %q refused: %v", name, err)
		}
		if got, err := topic.Value(); err != nil || got != name {
			t.Fatalf("topic round trip %q: %q %v", name, got, err)
		}
	}
	for _, name := range []string{"", "a//b", "a/+", "a/#", "$share/group/a", "a/\x00b", "a/\nb", string([]byte{0xff})} {
		if _, err := ParseTopic(name); !errors.Is(err, ErrInvalidTopic) || mqtttopic.ValidName(name) {
			t.Fatalf("invalid publication %q admitted: %v", name, err)
		}
	}
	for _, input := range []struct {
		filter, topic string
		matches       bool
	}{
		{"zigbee2mqtt/+/action", "zigbee2mqtt/button/action", true},
		{"zigbee2mqtt/#", "zigbee2mqtt/button/action", true},
		{"#", "$SYS/broker/uptime", false},
		{"$SYS/#", "$SYS/broker/uptime", true},
		{"zigbee2mqtt/+/action", "zigbee2mqtt/button/state", false},
	} {
		filter, err := ParseTopicFilter(input.filter)
		if err != nil {
			t.Fatal(err)
		}
		topic, err := ParseTopic(input.topic)
		if err != nil || filter.Matches(topic) != input.matches {
			t.Fatalf("filter %q topic %q: %v", input.filter, input.topic, err)
		}
	}
	for _, filter := range []string{"a#", "a/+b", "a/#/b", "$share/group/a", "a//b"} {
		if _, err := ParseTopicFilter(filter); !errors.Is(err, ErrInvalidTopic) {
			t.Fatalf("invalid filter %q admitted: %v", filter, err)
		}
	}
}

func TestNHA040NativeWrapperRetainedAndIdenticalLiveEdges(t *testing.T) {
	rig := newNHA069Rig(t, ModePassive)
	filter, err := ParseTopicFilter("zigbee2mqtt/+/action")
	if err != nil {
		t.Fatal(err)
	}
	handler := &triggerEdgeHandler{mapper: rig.mapper, filters: []TopicFilter{filter}}
	host := &nha040EdgeHost{Session: rig.session}
	observe := func(name, payload string, class ObservationClass) {
		t.Helper()
		if err := handler.Observe(context.Background(), host, Observation{topic: mustNHA069Topic(t, name), payload: []byte(payload), class: class}); err != nil {
			t.Fatal(err)
		}
	}
	observe("zigbee2mqtt/button/action", "on", ObservationRestored)
	observe("zigbee2mqtt/button/state", "on", ObservationLive)
	observe("zigbee2mqtt/button/action", "on", ObservationLive)
	observe("zigbee2mqtt/button/action", "on", ObservationLive)
	if len(host.edges) != 2 {
		t.Fatalf("expected two live edges, got %d", len(host.edges))
	}
	for i, edge := range host.edges {
		name, payload, sequence, err := edge.Components()
		if err != nil || name != "zigbee2mqtt/button/action" || string(payload) != "on" || sequence != uint64(i+2) {
			t.Fatalf("edge %d: %q %q %d %v", i, name, payload, sequence, err)
		}
	}
}

func TestNHA040TriggerFactoryRequiresExistingExactSubscription(t *testing.T) {
	rig := newNHA069Rig(t, ModePassive)
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha069-synthetic",
		Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password",
		Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/+/action"}})
	if err != nil {
		t.Fatal(err)
	}
	allowed := mustNHA069Filter(t, "zigbee2mqtt/+/action")
	subscriptions, err := settings.Subscriptions()
	if err != nil || len(subscriptions) != 2 {
		t.Fatalf("admitted subscriptions unavailable: %v", err)
	}
	subscriptions[1] = mustNHA069Filter(t, "zigbee2mqtt/#")
	factory, err := NewDiscoveryPilotFactoryWithTriggerEdges(settings, rig.repo, []TopicFilter{allowed})
	if err != nil || factory == nil || factory() == nil {
		t.Fatalf("admitted filter did not construct a native pilot: %v", err)
	}
	for _, unauthorized := range []TopicFilter{
		mustNHA069Filter(t, "zigbee2mqtt/#"),
		mustNHA069Filter(t, "zigbee2mqtt/button/action"),
	} {
		if _, err := NewDiscoveryPilotFactoryWithTriggerEdges(settings, rig.repo, []TopicFilter{unauthorized}); !errors.Is(err, ErrInvalidSettings) {
			t.Fatalf("unconfigured filter %v admitted: %v", unauthorized, err)
		}
	}
}
