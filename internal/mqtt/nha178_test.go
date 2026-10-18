package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/eclipse/paho.golang/paho"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nha178DiscoveryTopic = "homeassistant/light/kitchen/config"
const nha178DiscoveryBase = `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness"]}`

func TestNHA178DiscoveryQualificationIsExactAndOptedIn(t *testing.T) {
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha178-mqtt", Mode: "active", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#"}, CommandTopics: []string{"zigbee2mqtt/+/set"}, Will: &WillInput{Topic: "zigbee2mqtt/presence/pilot", Payload: "offline"}, TransitionLights: []TransitionLightInput{{SourceDevice: "kitchen-device", SourceEntity: "kitchen-driver", Profile: ikeaDriver30WProfile}}})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("kitchen-device", "kitchen-driver")
	mapper := &DiscoveryMapper{mode: ModeActive, transitionLights: settings.transitionLights}
	for _, test := range []struct {
		name, discovery string
		qualified       bool
	}{
		{"exact", nha178DiscoveryBase, true},
		{"explicit-false", `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness"],"transition":false}`, false},
		{"other-model", `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"other"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness"]}`, false},
		{"other-scale", `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":255,"supported_color_modes":["brightness"]}`, false},
		{"color-temperature", `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness","color_temp"],"min_mireds":250,"max_mireds":454}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			object, err := decodeDiscoveryJSON([]byte(test.discovery))
			if err != nil {
				t.Fatal(err)
			}
			profile, err := parseDiscoveryComponent(object, componentLight, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			qualified := mapper.admitTransition(key, profile) == transitionIkeaDriver30W
			if qualified != test.qualified {
				t.Fatalf("qualification %v, want %v", qualified, test.qualified)
			}
		})
	}
	mapper.transitionLights = nil
	object, _ := decodeDiscoveryJSON([]byte(nha178DiscoveryBase))
	profile, _ := parseDiscoveryComponent(object, componentLight, nil, nil)
	if mapper.admitTransition(key, profile) != transitionUnspecified {
		t.Fatal("discovery alone advertised transition")
	}
	for _, bad := range []Input{
		{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha178-mqtt", Mode: "active", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#"}, CommandTopics: []string{"zigbee2mqtt/+/set"}, Will: &WillInput{Topic: "zigbee2mqtt/presence/pilot", Payload: "offline"}, TransitionLights: []TransitionLightInput{{SourceDevice: "kitchen-device", SourceEntity: "kitchen-driver", Profile: "unknown"}}},
	} {
		if _, err := ParseSettings(bad); err == nil {
			t.Fatal("unknown transition profile admitted")
		}
	}
}

func TestNHA178FakeBrokerExactSecondsAndDiscoveryWithdrawal(t *testing.T) {
	broker := testbroker.Start(t)
	r := newNHA069Rig(t, ModeActive)
	key := r.configure("kitchen-driver", "kitchen-device")
	ids, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("light.kitchen_lights")
	record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, r.instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic", Mode: "active", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/+/state"}, CommandTopics: []string{"zigbee2mqtt/+/set"}, Will: &WillInput{Topic: "zigbee2mqtt/presence/pilot", Payload: "offline"}, TransitionLights: []TransitionLightInput{{SourceDevice: "kitchen-device", SourceEntity: "kitchen-driver", Profile: ikeaDriver30WProfile}}})
	if err != nil {
		t.Fatal(err)
	}
	r.mapper, err = newDiscoveryMapper(settings, r.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	pilot, err := NewPilot(settings, r.mapper)
	if err != nil {
		t.Fatal(err)
	}
	driver := &discoveryPilot{Pilot: pilot, mapper: r.mapper}
	if err := r.manager.AttachCommandDriver(r.session, driver.CommandDriver()); err != nil {
		t.Fatal(err)
	}
	observer := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha178-command-observer", true, 0, nil)
	packets := observer.Subscribe(t, "zigbee2mqtt/kitchen/set")
	tasks := newNHA068Tasks()
	host := nha068BindingHost{Session: r.session, provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	entity := ids[0].Entity
	waitTransition := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			descriptions, err := r.manager.Capabilities(entity)
			if err == nil && len(descriptions) == 1 && (descriptions[0].GetLight().TransitionSeconds != nil) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("transition capability did not become %v: %v %v; diagnostics %v", want, descriptions, err, r.mapper.Diagnostics())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	broker.Publish(t, nha178DiscoveryTopic, []byte(nha178DiscoveryBase), true)
	waitTransition(true)
	if count := broker.PublishCount(settings.clientID); count != 0 {
		t.Fatalf("discovery published %d application packets", count)
	}
	router, err := integrations.NewCommandRouter(r.manager, nha069Generation{})
	if err != nil {
		t.Fatal(err)
	}
	start, err := r.fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := domain.ParseActorID("synthetic-actor")
	entityText, _ := entity.Value()
	dispatch := func(id string, light *actions.SetLight, expected bool) {
		t.Helper()
		request := &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7,
			Target:             &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entityText}},
			CapabilitySchemaId: contracts.LightSchema, Endpoint: "mqtt",
			Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: light}},
			ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
		response, err := router.Dispatch(context.Background(), actor, request)
		if expected && (err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED) {
			t.Fatalf("command %s rejected: %v %v", id, response, err)
		}
		if !expected && err == nil && response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("command %s dispatched after profile withdrawal", id)
		}
	}
	trueValue, falseValue := true, false
	for i, test := range []struct {
		light *actions.SetLight
		want  map[string]any
	}{
		{&actions.SetLight{On: &trueValue, TransitionSeconds: func() *float64 { v := 0.0; return &v }()}, map[string]any{"state": "ON", "transition": float64(0)}},
		{&actions.SetLight{On: &trueValue, TransitionSeconds: func() *float64 { v := 1.0; return &v }()}, map[string]any{"state": "ON", "transition": float64(1)}},
		{&actions.SetLight{BrightnessPercent: func() *float64 { v := 50.0; return &v }(), TransitionSeconds: func() *float64 { v := 2.0; return &v }()}, map[string]any{"brightness": float64(127), "transition": float64(2)}},
		{&actions.SetLight{On: &falseValue, TransitionSeconds: func() *float64 { v := 10.0; return &v }()}, map[string]any{"state": "OFF", "transition": float64(10)}},
	} {
		dispatch(fmt.Sprintf("nha178-mqtt-%d", i), test.light, true)
		select {
		case packet := <-packets:
			var fields map[string]any
			if err := json.Unmarshal(packet.Payload, &fields); err != nil || len(fields) != len(test.want) {
				t.Fatalf("unexpected MQTT command: %q %v", packet.Payload, err)
			}
			for key, want := range test.want {
				if fields[key] != want {
					t.Fatalf("MQTT %s=%v, want %v", key, fields[key], want)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing isolated broker command")
		}
	}
	if count := broker.PublishCount(settings.clientID); count != 4 {
		t.Fatalf("pilot published %d packets, want 4", count)
	}
	zero := 0.0
	dispatch("nha178-off-with-brightness", &actions.SetLight{On: &falseValue, BrightnessPercent: &zero, TransitionSeconds: &zero}, false)
	dispatch("nha178-transition-alone", &actions.SetLight{TransitionSeconds: &zero}, false)
	if count := broker.PublishCount(settings.clientID); count != 4 {
		t.Fatalf("rejected transition published %d packets", count)
	}
	for i, replacement := range []string{
		`{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"other"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness"]}`,
		`{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":255,"supported_color_modes":["brightness"]}`,
		`{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["brightness"],"transition":false}`,
	} {
		broker.Publish(t, nha178DiscoveryTopic, []byte(replacement), true)
		waitTransition(false)
		seconds := float64(1)
		dispatch(fmt.Sprintf("nha178-withdrawn-%d", i), &actions.SetLight{On: &trueValue, TransitionSeconds: &seconds}, false)
		if count := broker.PublishCount(settings.clientID); count != 4 {
			t.Fatalf("withdrawn transition published %d packets", count)
		}
		if i != 2 {
			broker.Publish(t, nha178DiscoveryTopic, []byte(nha178DiscoveryBase), true)
			waitTransition(true)
		}
	}
	_ = observer.Client.Disconnect(&paho.Disconnect{ReasonCode: 0})
}
