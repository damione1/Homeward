package mqtt

import (
	"context"
	"encoding/json"
	"errors"
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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha180Driver struct {
	pilot   *discoveryPilot
	last    integrations.AdmittedCommand
	entered chan integrations.AdmittedCommand
	release chan struct{}
}

func (d *nha180Driver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	d.last = command
	if d.entered != nil {
		select {
		case d.entered <- command:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.pilot.Dispatch(ctx, command)
}

type nha180Rig struct {
	*nha069Rig
	broker   *testbroker.Broker
	driver   *nha180Driver
	router   *integrations.CommandRouter
	settings Settings
	entity   domain.EntityID
	actor    domain.ActorID
	epoch    string
	packets  <-chan *paho.Publish
}

func newNHA180Rig(t *testing.T, optIn bool) *nha180Rig {
	t.Helper()
	broker := testbroker.Start(t)
	r := newNHA069Rig(t, ModeActive)
	key := r.configure("kitchen-driver", "kitchen-device")
	ids, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("light.nha180_kitchen")
	record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, r.instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	input := Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic", Mode: "active", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/+/state"}, CommandTopics: []string{"zigbee2mqtt/+/set"}, Will: &WillInput{Topic: "zigbee2mqtt/presence/pilot", Payload: "offline"}}
	if optIn {
		input.TransitionLights = []TransitionLightInput{{SourceDevice: "kitchen-device", SourceEntity: "kitchen-driver", Profile: ikeaDriver30WProfile}}
	}
	settings, err := ParseSettings(input)
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
	driver := &nha180Driver{pilot: &discoveryPilot{Pilot: pilot, mapper: r.mapper}}
	if err := r.manager.AttachCommandDriver(r.session, driver); err != nil {
		t.Fatal(err)
	}
	observer := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha180-command-observer", true, 0, nil)
	packets := observer.Subscribe(t, "zigbee2mqtt/kitchen/set")
	tasks := newNHA068Tasks()
	host := nha068BindingHost{Session: r.session, provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	broker.Publish(t, nha178DiscoveryTopic, []byte(nha178DiscoveryBase), true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		descriptions, err := r.manager.Capabilities(ids[0].Entity)
		if err == nil && len(descriptions) == 1 && descriptions[0].SchemaId == contracts.LightSchema {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("light discovery did not reach Host: %v %v %v", descriptions, err, r.mapper.Diagnostics())
		}
		time.Sleep(10 * time.Millisecond)
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
	actor, _ := domain.ParseActorID("nha180-synthetic-actor")
	return &nha180Rig{nha069Rig: r, broker: broker, driver: driver, router: router, settings: settings, entity: ids[0].Entity, actor: actor, epoch: epoch, packets: packets}
}

func (r *nha180Rig) request(id string, light *actions.SetLight) *actions.CommandRequest {
	entity, _ := r.entity.Value()
	return &actions.CommandRequest{CommandId: id, AdmissionEpoch: r.epoch, ConfigRevision: 7,
		Target:             &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}},
		CapabilitySchemaId: contracts.LightSchema, Endpoint: "mqtt",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: light}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
}

func (r *nha180Rig) dispatch(t *testing.T, id string, light *actions.SetLight) map[string]any {
	t.Helper()
	response, err := r.router.Dispatch(context.Background(), r.actor, r.request(id, light))
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("dispatch %s: %v %v", id, response, err)
	}
	select {
	case packet := <-r.packets:
		var fields map[string]any
		if err := json.Unmarshal(packet.Payload, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	case <-time.After(3 * time.Second):
		t.Fatalf("missing MQTT packet for %s", id)
		return nil
	}
}

func (r *nha180Rig) status(t *testing.T, id string, want actions.ActionStatus) {
	t.Helper()
	commandID, _ := domain.ParseCommandID(id)
	got, err := r.manager.ActionStatus(commandID)
	if err != nil || got.Status != want {
		t.Fatalf("%s progression: %+v %v, want %v", id, got, err, want)
	}
}

func (r *nha180Rig) correlatedLight(t *testing.T, on bool, brightness *float64) {
	t.Helper()
	light, err := domain.NewLightValue(on, brightness, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewLightStateValue(light)
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	observation, err := integrations.NewStateObservation("mqtt", quality, &value, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	causality, err := r.driver.last.Causality()
	if err != nil {
		t.Fatal(err)
	}
	observation, err = observation.WithCausality(causality)
	if err != nil {
		t.Fatal(err)
	}
	r.mapper.mu.Lock()
	binding := r.mapper.documents[mustNHA069Topic(t, nha178DiscoveryTopic)][0].binding
	r.mapper.mu.Unlock()
	if err := r.session.Publish(context.Background(), binding, observation); err != nil {
		t.Fatal(err)
	}
}

func TestNHA180RealMQTTEncodingAndEvidence(t *testing.T) {
	r := newNHA180Rig(t, true)
	trueValue, falseValue := true, false
	for i, tc := range []struct {
		light *actions.SetLight
		want  map[string]any
	}{
		{&actions.SetLight{On: &trueValue, BrightnessPercent: ptrNHA180(30)}, map[string]any{"state": "ON", "brightness": float64(76)}},
		{&actions.SetLight{On: &trueValue, BrightnessPercent: ptrNHA180(50)}, map[string]any{"state": "ON", "brightness": float64(127)}},
		{&actions.SetLight{On: &trueValue, BrightnessPercent: ptrNHA180(0)}, map[string]any{"state": "ON", "brightness": float64(2)}},
		{&actions.SetLight{BrightnessPercent: ptrNHA180(0.1)}, map[string]any{"brightness": float64(2)}},
		{&actions.SetLight{BrightnessPercent: ptrNHA180(0.4)}, map[string]any{"brightness": float64(2)}},
		{&actions.SetLight{On: &trueValue}, map[string]any{"state": "ON"}},
		{&actions.SetLight{On: &falseValue, BrightnessPercent: ptrNHA180(30)}, map[string]any{"state": "OFF", "brightness": float64(76)}},
		{&actions.SetLight{On: &falseValue, BrightnessPercent: ptrNHA180(0)}, map[string]any{"state": "OFF", "brightness": float64(0)}},
		{&actions.SetLight{BrightnessPercent: ptrNHA180(0)}, map[string]any{"brightness": float64(0)}},
	} {
		id := fmt.Sprintf("nha180-real-%d", i)
		fields := r.dispatch(t, id, tc.light)
		if len(fields) != len(tc.want) {
			t.Fatalf("%s fields: %v, want %v", id, fields, tc.want)
		}
		for name, want := range tc.want {
			if fields[name] != want {
				t.Fatalf("%s %s = %v, want %v", id, name, fields[name], want)
			}
		}
		if i == 0 {
			// The actual mapper produces a value match but no causality.
			r.send("zigbee2mqtt/kitchen/state", `{"state":"ON","brightness":76}`, ObservationLive)
			r.status(t, id, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
			wrong := float64(75) * 100 / 254
			r.correlatedLight(t, true, &wrong)
			r.status(t, id, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
			wrong = float64(77) * 100 / 254
			r.correlatedLight(t, true, &wrong)
			r.status(t, id, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
			exact := float64(76) * 100 / 254
			r.correlatedLight(t, true, &exact)
			r.status(t, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
		}
		if i == 3 {
			exact := float64(2) * 100 / 254
			r.correlatedLight(t, true, &exact)
			r.status(t, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
		}
		if i == 6 {
			exact := float64(76) * 100 / 254
			r.correlatedLight(t, false, &exact)
			r.status(t, id, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
		}
	}
	if got := r.broker.PublishCount(r.settings.clientID); got != 9 {
		t.Fatalf("pilot published %d commands, want 9", got)
	}
}

func ptrNHA180(value float64) *float64 { return &value }

func TestNHA180LocalProfileFencePreservesCanonicalRefresh(t *testing.T) {
	r := newNHA180Rig(t, false)
	r.driver.entered = make(chan integrations.AdmittedCommand, 1)
	r.driver.release = make(chan struct{})
	before, err := r.manager.Capabilities(r.entity)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	queued := func(id string) <-chan result {
		t.Helper()
		out := make(chan result, 1)
		go func() {
			response, err := r.router.Dispatch(context.Background(), r.actor, r.request(id, &actions.SetLight{BrightnessPercent: ptrNHA180(30)}))
			out <- result{response, err}
		}()
		select {
		case <-r.driver.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("driver did not reach barrier")
		}
		return out
	}
	first := queued("nha180-canonical-refresh")
	r.send(nha178DiscoveryTopic, nha178DiscoveryBase+"\n", ObservationLive)
	close(r.driver.release)
	if got := <-first; got.err != nil || got.response == nil || got.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("canonical refresh fenced unchanged command: %+v", got)
	}
	select {
	case packet := <-r.packets:
		var fields map[string]any
		if err := json.Unmarshal(packet.Payload, &fields); err != nil || fields["brightness"] != float64(76) {
			t.Fatalf("refreshed payload: %q %v", packet.Payload, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canonical refresh lost command packet")
	}
	r.driver.release = make(chan struct{})
	second := queued("nha180-scale-revision")
	changed := `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"],"manufacturer":"IKEA","model_id":"ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2"},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":255,"supported_color_modes":["brightness"]}`
	r.send(nha178DiscoveryTopic, changed, ObservationLive)
	after, err := r.manager.Capabilities(r.entity)
	if err != nil || len(before) != 1 || len(after) != 1 || !proto.Equal(before[0], after[0]) {
		t.Fatalf("scale revision unexpectedly changed public description: %v %v %v", before, after, err)
	}
	close(r.driver.release)
	if got := <-second; !errors.Is(got.err, integrations.ErrCommandRouteUnavailable) || got.response == nil || got.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("revised profile did not fence queued command: %+v", got)
	}
	if got := r.broker.PublishCount(r.settings.clientID); got != 1 {
		t.Fatalf("profile revision emitted %d commands, want one prior packet", got)
	}
	r.driver.entered = nil
	fields := r.dispatch(t, "nha180-generic-low", &actions.SetLight{BrightnessPercent: ptrNHA180(0.1)})
	if len(fields) != 1 || fields["brightness"] != float64(0) {
		t.Fatalf("generic low brightness payload changed: %v", fields)
	}
	zero := float64(0)
	r.correlatedLight(t, false, &zero)
	r.status(t, "nha180-generic-low", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	if got := r.broker.PublishCount(r.settings.clientID); got != 2 {
		t.Fatalf("pilot published %d commands, want two", got)
	}
}
