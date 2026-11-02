package mqtt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
)

func nha193Input(address string) Input {
	return Input{Profile: "5", Broker: "mqtt://" + address, Instance: "nha069-synthetic", Mode: "active", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password",
		Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/#"}, CommandTopics: []string{"zigbee2mqtt/admitted-relay/set"},
		Will:     &WillInput{Topic: "homeward/test-presence", Payload: "offline"},
		Z2MReads: []Z2MReadInput{{StateTopic: "zigbee2mqtt/Test lamp", Properties: []string{"state", "brightness"}}}}
}

func TestNHA193ReadAdmissionCannotBecomeACommandOrPassivePublication(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Input)
	}{
		{"passive", func(i *Input) { i.Mode = "passive"; i.CommandTopics = nil; i.Will = nil }},
		{"wildcard", func(i *Input) { i.Z2MReads[0].StateTopic = "zigbee2mqtt/+" }},
		{"bridge", func(i *Input) { i.Z2MReads[0].StateTopic = "zigbee2mqtt/bridge/request/restart" }},
		{"set-route", func(i *Input) { i.Z2MReads[0].StateTopic = "zigbee2mqtt/relay/set" }},
		{"unsubscribed", func(i *Input) { i.Subscriptions = []string{"homeassistant/#"} }},
		{"effect", func(i *Input) { i.Z2MReads[0].Properties = []string{"state", "effect"} }},
		{"write-value", func(i *Input) { i.Z2MReads[0].Properties = []string{"state=ON"} }},
		{"partial-state", func(i *Input) { i.Z2MReads[0].Properties = []string{"brightness"} }},
		{"duplicate-property", func(i *Input) { i.Z2MReads[0].Properties = []string{"state", "state"} }},
		{"duplicate-route", func(i *Input) { i.Z2MReads = append(i.Z2MReads, i.Z2MReads[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := nha193Input("127.0.0.1:1883")
			test.edit(&input)
			if _, err := ParseSettings(input); !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("unsafe read configuration admitted: %v", err)
			}
		})
	}
	settings, err := ParseSettings(nha193Input("127.0.0.1:1883"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPilot(settings, UnmappedObservations{}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatal("read admitted without discovery ownership")
	}
}

func TestNHA193NativeReadWaitsForDiscoveryAndRealStateAcrossReconnect(t *testing.T) {
	broker := testbroker.Start(t)
	settings, err := ParseSettings(nha193Input(broker.Address))
	if err != nil {
		t.Fatal(err)
	}
	observer := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha193-read-observer", true, 0, nil)
	requests := observer.Subscribe(t, "zigbee2mqtt/Test lamp/get")
	const discoveryTopic = "homeassistant/light/test-lamp/config"
	const discovery = `{"unique_id":"test-lamp","device":{"identifiers":["test-device"],"manufacturer":"IKEA","model_id":"LED2103G5"},"schema":"json","state_topic":"zigbee2mqtt/Test lamp","command_topic":"zigbee2mqtt/Test lamp/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness"],"availability_topic":"zigbee2mqtt/bridge/state"}`
	broker.Publish(t, "zigbee2mqtt/Test lamp", []byte(`{"state":"OFF","brightness":25}`), true)
	for session := 0; session < 2; session++ {
		r := newNHA069Rig(t, ModeActive)
		key := r.configure("test-lamp", "test-device")
		r.mapper, err = newDiscoveryMapper(settings, r.repo, nil)
		if err != nil {
			t.Fatal(err)
		}
		pilot, err := NewPilot(settings, r.mapper)
		if err != nil {
			t.Fatal(err)
		}
		tasks := newNHA068Tasks()
		host := nha068BindingHost{Session: r.session, provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		err = pilot.Start(ctx, host, tasks)
		cancel()
		if err != nil {
			tasks.close(t, pilot)
			t.Fatal(err)
		}
		t.Cleanup(func() { tasks.close(t, pilot) })
		if session == 0 {
			// A matching topic with an unconfigured identity grants no read.
			broker.Publish(t, discoveryTopic, []byte(strings.Replace(discovery, "test-device", "unconfigured-device", 1)), true)
			nha193Wait(t, func() bool { return len(r.mapper.Diagnostics()) != 0 })
			if got := broker.PublishCount(settings.clientID); got != 0 {
				t.Fatalf("published before configured discovery: %d", got)
			}
			broker.Publish(t, discoveryTopic, []byte(discovery), true)
			nha193Wait(t, func() bool {
				r.mapper.mu.Lock()
				defer r.mapper.mu.Unlock()
				return len(r.mapper.documents) == 1
			})
			if got := broker.PublishCount(settings.clientID); got != 0 {
				t.Fatalf("published before declared availability: %d", got)
			}
			broker.Publish(t, "zigbee2mqtt/bridge/state", []byte("online"), true)
		}
		select {
		case request := <-requests:
			if request.Topic != "zigbee2mqtt/Test lamp/get" || string(request.Payload) != `{"brightness":"","state":""}` || request.QoS != 0 || request.Retain || request.Properties != nil && (request.Properties.ResponseTopic != "" || len(request.Properties.CorrelationData) != 0) {
				t.Fatalf("unsafe native read: %+v", request)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("native get absent after subscribed, admitted discovery")
		}
		// Native get transport acceptance never promotes a retained value.
		if _, ok := r.state(key).CriticalValue(); ok {
			t.Fatal("read request fabricated a fresh device state")
		}
		broker.Publish(t, "zigbee2mqtt/Test lamp", []byte(`{"state":"ON","brightness":127}`), false)
		nha193Wait(t, func() bool { _, ok := r.state(key).CriticalValue(); return ok })
		value, _ := r.state(key).CriticalValue()
		light, _ := value.Light()
		on, brightness, _, err := light.Components()
		if err != nil || !on || brightness == nil || *brightness != 50 {
			t.Fatalf("native report mapping: on=%t brightness=%v err=%v", on, brightness, err)
		}
		// Repeated discovery is not an implicit read or command replay.
		broker.Publish(t, discoveryTopic, []byte(discovery), true)
		publication, _ := NewCommandPublication(mustNHA069Topic(t, "zigbee2mqtt/Test lamp/set"), []byte("ON"))
		if err := pilot.WriteCommand(context.Background(), publication); !errors.Is(err, ErrInvalidTopic) {
			t.Fatalf("read permission granted command permission: %v", err)
		}
		tasks.close(t, pilot)
		broker.WaitSessionRemoved(t, settings.clientID)
		if got := broker.PublishCount(settings.clientID); got != session+1 {
			t.Fatalf("session %d replayed read or command: %d publications", session, got)
		}
	}
}

func TestNHA193AvailabilityCannotRefreshSavedDeviceValue(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("saved-lamp", "saved-device")
	r.send("homeassistant/switch/saved/config", `{"unique_id":"saved-lamp","device":{"identifiers":["saved-device"]},"state_topic":"zigbee2mqtt/saved","availability_topic":"zigbee2mqtt/bridge/state"}`, ObservationRestored)
	r.send("zigbee2mqtt/bridge/state", "online", ObservationRestored)
	r.send("zigbee2mqtt/saved", "ON", ObservationLive)
	if _, ok := r.state(key).CriticalValue(); !ok {
		t.Fatal("direct live device report was not fresh")
	}
	for _, class := range []ObservationClass{ObservationLive, ObservationRestored} {
		r.send("zigbee2mqtt/bridge/state", "online", class)
		state := r.state(key)
		if _, ok := state.CriticalValue(); ok {
			t.Fatal("availability renewed saved state freshness")
		}
		_, quality, _, _, _, _ := state.Components()
		_, freshness, provenance, _ := quality.Components()
		if freshness != domain.FreshnessStale || provenance != domain.ProvenanceObserved {
			t.Fatalf("saved state quality = %v/%v", freshness, provenance)
		}
	}
	r.send("zigbee2mqtt/saved", "OFF", ObservationLive)
	if _, ok := r.state(key).CriticalValue(); !ok {
		t.Fatal("subsequent real state did not refresh")
	}
}

func nha193Wait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for !ready() {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("expected MQTT observation absent")
		}
	}
}
