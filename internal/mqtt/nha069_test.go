package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/eclipse/paho.golang/paho"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha069Generation struct{}

type nha069RejectingPublication struct{ *integrations.Session }

func (nha069RejectingPublication) Publish(context.Context, integrations.Binding, integrations.StateObservation) error {
	return integrations.ErrInactiveSession
}

func TestNHA069HostPublicationFailureIsNotAStateDiagnostic(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	r.configure("failure-sensor", "failure-device")
	r.send("homeassistant/sensor/failure/config", `{"unique_id":"failure-sensor","device":{"identifiers":["failure-device"]},"device_class":"power","unit_of_measurement":"W","state_topic":"zigbee2mqtt/failure","value_template":"{{ value_json.power }}"}`, ObservationRestored)
	err := r.mapper.Observe(context.Background(), nha069RejectingPublication{r.session}, Observation{topic: mustNHA069Topic(t, "zigbee2mqtt/failure"), payload: []byte(`{"power":10}`), class: ObservationLive})
	if !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("Host publication failure was swallowed: %v", err)
	}
	if len(r.mapper.Diagnostics()) != 0 {
		t.Fatal("Host failure mislabeled as malformed MQTT state")
	}
}

func (nha069Generation) WithCurrent(ctx context.Context, run func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(7)
}

func TestNHA069ActiveCommandRouterWritesOnlyAfterDispatch(t *testing.T) {
	broker := testbroker.Start(t)
	r := newNHA069Rig(t, ModeActive)
	switchKey := r.configure("switch-wire", "command-device")
	lightKey := r.configure("light-wire", "command-device")
	keys := []integrations.SourceKey{switchKey, lightKey}
	ids, err := r.fixture.Resolve(context.Background(), r.instance, keys)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]integrations.RegistryRecord, 0, 2)
	for i, aliasText := range []string{"switch.synthetic_relay", "light.synthetic_lamp"} {
		alias, err := domain.ParseHAAlias(aliasText)
		if err != nil {
			t.Fatal(err)
		}
		record, err := integrations.NewRegistryRecord(ids[i].Device, ids[i].Entity, r.instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := r.fixture.PublishRegistry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic", Mode: "active", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/+/state"}, CommandTopics: []string{"zigbee2mqtt/+/set"}, Will: &WillInput{Topic: "zigbee2mqtt/presence/pilot", Payload: "offline"}})
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
	observer := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha069-command-observer", true, 0, nil)
	packets := observer.Subscribe(t, "zigbee2mqtt/+/set")
	tasks := newNHA068Tasks()
	host := nha068BindingHost{Session: r.session, provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	broker.Publish(t, "homeassistant/switch/relay/config", []byte(`{"unique_id":"switch-wire","device":{"identifiers":["command-device"]},"state_topic":"zigbee2mqtt/relay/state","command_topic":"zigbee2mqtt/relay/set"}`), true)
	broker.Publish(t, "homeassistant/light/lamp/config", []byte(`{"unique_id":"light-wire","device":{"identifiers":["command-device"]},"schema":"json","state_topic":"zigbee2mqtt/lamp/state","command_topic":"zigbee2mqtt/lamp/set","brightness":true,"brightness_scale":254,"supported_color_modes":["color_temp"],"min_mireds":250,"max_mireds":454}`), true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, switchErr := r.manager.Capabilities(ids[0].Entity)
		_, lightErr := r.manager.Capabilities(ids[1].Entity)
		if switchErr == nil && lightErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker discovery did not reach Host: %v %v; diagnostics %v", switchErr, lightErr, r.mapper.Diagnostics())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := broker.PublishCount(settings.clientID); got != 0 {
		t.Fatalf("startup/discovery published %d packets", got)
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
	actor, err := domain.ParseActorID("synthetic-actor")
	if err != nil {
		t.Fatal(err)
	}
	for i, scenario := range []struct {
		id, schema, topic string
		args              *actions.ActionArgs
	}{
		{"nha069-switch-command", contracts.SwitchSchema, "zigbee2mqtt/relay/set", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}},
		{"nha069-light-command", contracts.LightSchema, "zigbee2mqtt/lamp/set", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: func() *bool { v := true; return &v }(), BrightnessPercent: func() *float64 { v := 50.0; return &v }(), ColorTempMireds: func() *uint32 { v := uint32(300); return &v }()}}}},
	} {
		entityText, _ := ids[i].Entity.Value()
		request := &actions.CommandRequest{CommandId: scenario.id, AdmissionEpoch: epoch, ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entityText}}, CapabilitySchemaId: scenario.schema, Endpoint: "mqtt", Arguments: scenario.args, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
		response, err := router.Dispatch(context.Background(), actor, request)
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("command %s: %v %v", scenario.id, response, err)
		}
		select {
		case packet := <-packets:
			if packet.Topic != scenario.topic {
				t.Fatalf("command topic %q, want %q", packet.Topic, scenario.topic)
			}
			if i == 0 && string(packet.Payload) != "ON" {
				t.Fatalf("switch payload %q", packet.Payload)
			}
			if i == 1 {
				var fields map[string]any
				if err := json.Unmarshal(packet.Payload, &fields); err != nil || fields["state"] != "ON" || fields["brightness"] != float64(127) || fields["color_temp"] != float64(300) {
					t.Fatalf("light payload %q: %v", packet.Payload, err)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("command %s did not reach isolated broker", scenario.id)
		}
	}
	if got := broker.PublishCount(settings.clientID); got != 2 {
		t.Fatalf("active pilot published %d packets, want only two dispatched commands", got)
	}
	_ = observer.Client.Disconnect(&paho.Disconnect{ReasonCode: 0})
}

type nha069MemoryRegistry struct {
	mu                 sync.Mutex
	configured         map[string]integrations.SourceKey
	topics             map[string][]integrations.SourceKey
	retired            map[domain.EntityID]bool
	topicLookupEntered chan struct{}
	topicLookupRelease <-chan struct{}
	topicLookupError   error
	tombstoneEntered   chan struct{}
	tombstoneRelease   <-chan struct{}
}

func TestNHA069RetirementFencesKnownSourceBeforeSQLTopicLookup(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	oldKey := r.configure("lookup-old", "lookup-device-a")
	neighborKey := r.configure("lookup-neighbor", "lookup-device-b")
	oldTopic := "homeassistant/switch/lookup-old/config"
	r.send(oldTopic, `{"unique_id":"lookup-old","device":{"identifiers":["lookup-device-a"]},"state_topic":"zigbee2mqtt/lookup-old"}`, ObservationRestored)
	r.send("homeassistant/switch/lookup-neighbor/config", `{"unique_id":"lookup-neighbor","device":{"identifiers":["lookup-device-b"]},"state_topic":"zigbee2mqtt/lookup-neighbor"}`, ObservationRestored)
	oldBinding := r.mapper.documents[mustNHA069Topic(t, oldTopic)][0].binding
	neighborBinding := r.mapper.documents[mustNHA069Topic(t, "homeassistant/switch/lookup-neighbor/config")][0].binding
	r.repo.topicLookupEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	r.repo.topicLookupRelease = release
	lookupFailure := errors.New("synthetic SQL topic lookup failure")
	r.repo.topicLookupError = lookupFailure
	done := make(chan error, 1)
	go func() {
		done <- r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(t, oldTopic), class: ObservationLive})
	}()
	select {
	case <-r.repo.topicLookupEntered:
	case <-time.After(time.Second):
		t.Fatal("topic SQL lookup did not reach barrier")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	observation, _ := integrations.NewStateObservation("mqtt", quality, &value, nil, time.Now().UTC(), nil)
	if err := r.session.Publish(context.Background(), oldBinding, observation); err == nil {
		t.Fatal("old binding published while topic SQL lookup was blocked")
	}
	if err := r.session.Publish(context.Background(), neighborBinding, observation); err != nil {
		t.Fatalf("neighbor blocked by unrelated SQL lookup: %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, lookupFailure) {
			t.Fatalf("topic SQL failure not surfaced: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SQL lookup barrier did not finish")
	}
	oldID, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{oldKey})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities, err := r.manager.Capabilities(oldID[0].Entity); err == nil && len(capabilities) != 0 {
		t.Fatal("failed SQL lookup left old catalogue active")
	}
	if _, ok := r.state(neighborKey).CriticalValue(); !ok {
		t.Fatal("neighbor lost its observed state")
	}
}

func TestNHA069ExcludedOnlyDeviceUpdateFencesBeforeSQLTopicLookup(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	r.configure("device-switch", "device-id")
	topic := "homeassistant/device/excluded-only/config"
	r.send(topic, `{"origin":{"name":"Zigbee2MQTT"},"device":{"identifiers":["device-id"]},"components":{"relay":{"platform":"switch","unique_id":"device-switch","state_topic":"zigbee2mqtt/device-switch"}}}`, ObservationRestored)
	binding := r.mapper.documents[mustNHA069Topic(t, topic)][0].binding
	r.repo.topicLookupEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	r.repo.topicLookupRelease = release
	r.repo.topicLookupError = errors.New("synthetic SQL lookup failure")
	done := make(chan error, 1)
	update := `{"origin":{"name":"Zigbee2MQTT"},"device":{"identifiers":["device-id"]},"components":{"setting":{"platform":"number","unique_id":"excluded-setting"}}}`
	go func() {
		done <- r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(t, topic), payload: []byte(update), class: ObservationLive})
	}()
	select {
	case <-r.repo.topicLookupEntered:
	case <-time.After(time.Second):
		t.Fatal("excluded-only device update did not reach SQL lookup")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	observation, _ := integrations.NewStateObservation("mqtt", quality, &value, nil, time.Now().UTC(), nil)
	if err := r.session.Publish(context.Background(), binding, observation); err == nil {
		t.Fatal("device update with zero admitted components left prior binding active")
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, r.repo.topicLookupError) {
			t.Fatalf("SQL lookup failure was swallowed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("excluded-only update did not finish")
	}
}

func TestNHA069ValidDeviceOmissionFencesBeforeSQLTopicLookup(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	oldKey := r.configure("omitted-switch", "omission-device")
	remainingKey := r.configure("remaining-switch", "omission-device")
	topic := "homeassistant/device/omission/config"
	r.send(topic, `{"origin":{"name":"Zigbee2MQTT"},"device":{"identifiers":["omission-device"]},"components":{"old":{"platform":"switch","unique_id":"omitted-switch","state_topic":"zigbee2mqtt/omitted"},"keep":{"platform":"switch","unique_id":"remaining-switch","state_topic":"zigbee2mqtt/remaining"}}}`, ObservationRestored)
	var oldBinding, remainingBinding integrations.Binding
	for _, runtime := range r.mapper.documents[mustNHA069Topic(t, topic)] {
		switch runtime.key {
		case oldKey:
			oldBinding = runtime.binding
		case remainingKey:
			remainingBinding = runtime.binding
		}
	}
	if _, _, err := oldBinding.IDs(); err != nil {
		t.Fatal("old binding absent")
	}
	if _, _, err := remainingBinding.IDs(); err != nil {
		t.Fatal("remaining binding absent")
	}
	r.repo.topicLookupEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	r.repo.topicLookupRelease = release
	r.repo.topicLookupError = errors.New("synthetic SQL lookup failure")
	done := make(chan error, 1)
	update := `{"origin":{"name":"Zigbee2MQTT"},"device":{"identifiers":["omission-device"]},"components":{"keep":{"platform":"switch","unique_id":"remaining-switch","state_topic":"zigbee2mqtt/remaining"}}}`
	go func() {
		done <- r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(t, topic), payload: []byte(update), class: ObservationLive})
	}()
	select {
	case <-r.repo.topicLookupEntered:
	case <-time.After(time.Second):
		t.Fatal("valid omission did not reach SQL lookup")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	observation, _ := integrations.NewStateObservation("mqtt", quality, &value, nil, time.Now().UTC(), nil)
	if err := r.session.Publish(context.Background(), oldBinding, observation); err == nil {
		t.Fatal("omitted binding published during SQL topic lookup")
	}
	if err := r.session.Publish(context.Background(), remainingBinding, observation); err != nil {
		t.Fatalf("remaining component blocked during omission: %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, r.repo.topicLookupError) {
			t.Fatalf("SQL lookup failure was swallowed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("valid omission did not finish")
	}
}

func TestNHA069MochiRetainedOrderAndLiveEmptyDeletion(t *testing.T) {
	broker := testbroker.Start(t)
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("broker-sensor", "broker-device")
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic", Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/#"}})
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
	defer cancel()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	stateTopic := "zigbee2mqtt/nha069/state"
	availabilityTopic := "zigbee2mqtt/nha069/availability"
	configTopic := "homeassistant/sensor/nha069/config"
	broker.Publish(t, stateTopic, []byte(`{"power":10.5}`), true)
	broker.Publish(t, availabilityTopic, []byte("online"), true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mapper.mu.Lock()
		count := len(r.mapper.snapshots)
		r.mapper.mu.Unlock()
		if count >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("broker snapshots did not arrive before discovery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	config := `{"unique_id":"broker-sensor","device":{"identifiers":["broker-device"]},"device_class":"power","unit_of_measurement":"W","state_class":"measurement","state_topic":"zigbee2mqtt/nha069/state","value_template":"{{ value_json.power }}","availability_topic":"zigbee2mqtt/nha069/availability"}`
	broker.Publish(t, configTopic, []byte(config), true)
	var entity domain.EntityID
	ids, _ := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
	entity = ids[0].Entity
	for {
		if state, ok := r.fixture.State(entity); ok {
			_, quality, _, _, _, _ := state.Components()
			a, f, p, _ := quality.Components()
			if a == domain.AvailabilityAvailable && f == domain.FreshnessStale && p == domain.ProvenanceObserved {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker cache replay failed: diagnostics %v", r.mapper.Diagnostics())
		}
		time.Sleep(10 * time.Millisecond)
	}
	broker.Publish(t, configTopic, nil, true)
	for {
		r.repo.mu.Lock()
		retired := r.repo.retired[entity]
		r.repo.mu.Unlock()
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live empty deletion not processed: %v", r.mapper.Diagnostics())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count := broker.PublishCount(settings.clientID); count != 0 {
		t.Fatalf("passive pilot published %d packets", count)
	}
}

func TestNHA069MochiStoredRetainedReplayOnSubscribe(t *testing.T) {
	broker := testbroker.Start(t)
	stateTopic := "zigbee2mqtt/nha069/stored-state"
	availabilityTopic := "zigbee2mqtt/nha069/stored-availability"
	configTopic := "homeassistant/sensor/nha069/stored/config"
	broker.Publish(t, stateTopic, []byte(`{"power":0}`), true)
	broker.Publish(t, availabilityTopic, []byte("online"), true)
	broker.Publish(t, configTopic, []byte(`{"unique_id":"stored-sensor","device":{"identifiers":["stored-device"]},"device_class":"power","unit_of_measurement":"W","state_class":"measurement","state_topic":"zigbee2mqtt/nha069/stored-state","value_template":"{{ value_json.power }}","availability_topic":"zigbee2mqtt/nha069/stored-availability"}`), true)
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("stored-sensor", "stored-device")
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic", Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/#"}})
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
	defer cancel()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	ids, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if state, exists := r.fixture.State(ids[0].Entity); exists {
			_, quality, _, _, _, _ := state.Components()
			a, f, p, _ := quality.Components()
			if a == domain.AvailabilityAvailable && f == domain.FreshnessStale && p == domain.ProvenanceRestored {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored retained replay failed: %v", r.mapper.Diagnostics())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count := broker.PublishCount(settings.clientID); count != 0 {
		t.Fatalf("passive startup published %d packets", count)
	}
}

func TestNHA069DiscoveryValidationAndMixedDeviceOmission(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	keySensor := r.configure("sensor-one", "wire-device")
	keySwitch := r.configure("switch-one", "wire-device")
	base := `{"origin":{"name":"Zigbee2MQTT"},"device":{"identifiers":["wire-device"],"via_device":"bridge"},"components":{"power":{"platform":"sensor","unique_id":"sensor-one","device_class":"power","unit_of_measurement":"W","state_class":"measurement","state_topic":"zigbee2mqtt/mixed","value_template":"{{ value_json.power }}"},"relay":{"platform":"switch","unique_id":"switch-one","state_topic":"zigbee2mqtt/relay","command_topic":"zigbee2mqtt/relay/set"},"setting":{"platform":"number","unique_id":"excluded-number","state_topic":"zigbee2mqtt/mixed"}}}`
	topic := "homeassistant/device/mixed/config"
	r.send(topic, base, ObservationRestored)
	if got := r.mapper.Diagnostics(); len(got) != 1 || got[0] != DiagnosticUnknownDiscovery {
		t.Fatalf("excluded neighbor diagnostic: %v", got)
	}
	if records := r.mapper.DiagnosticRecords(); len(records) != 1 || records[0].Code != DiagnosticUnknownDiscovery || records[0].Component != "setting" || records[0].Topic != mustNHA069Topic(t, topic) {
		t.Fatalf("excluded neighbor lost diagnostic provenance: %+v", records)
	}
	r.send("zigbee2mqtt/mixed", `{"power":10}`, ObservationLive)
	if _, ok := r.state(keySensor).CriticalValue(); !ok {
		t.Fatal("admitted sensor absent")
	}
	r.send("zigbee2mqtt/relay", "OFF", ObservationLive)
	if _, ok := r.state(keySwitch).CriticalValue(); !ok {
		t.Fatal("admitted switch absent")
	}
	invalid := `{"origin":{},"device":{"identifiers":["wire-device"]},"components":{"power":{"platform":"sensor","unique_id":"sensor-one","state_topic":"zigbee2mqtt/mixed"}}}`
	r.send(topic, invalid, ObservationLive)
	r.send("zigbee2mqtt/relay", "ON", ObservationLive)
	if value, ok := r.state(keySwitch).CriticalValue(); !ok {
		t.Fatal("invalid update removed old route")
	} else if on, _ := value.Switch(); !on {
		t.Fatal("invalid update changed old route")
	}
	omitted := `{"origin":{"name":"Zigbee2MQTT"},"device":{"identifiers":["wire-device"],"via_device":"bridge"},"components":{"power":{"platform":"sensor","unique_id":"sensor-one","device_class":"power","unit_of_measurement":"W","state_class":"measurement","state_topic":"zigbee2mqtt/mixed","value_template":"{{ value_json.power }}"},"setting":{"platform":"number","unique_id":"excluded-number"}}}`
	r.send(topic, omitted, ObservationLive)
	ids, _ := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{keySwitch})
	if !r.repo.retired[ids[0].Entity] {
		t.Fatal("omitted component was not tombstoned")
	}
	r.send("zigbee2mqtt/relay", "OFF", ObservationLive)
	if value, ok := r.state(keySwitch).CriticalValue(); ok {
		t.Fatalf("retired switch critical: %v", value)
	}
	if _, ok := r.state(keySensor).CriticalValue(); !ok {
		t.Fatal("neighbor retired")
	}
	r.send(topic, "", ObservationLive)
	ids, _ = r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{keySensor})
	if !r.repo.retired[ids[0].Entity] {
		t.Fatal("live retained deletion did not tombstone")
	}
}

func TestNHA069RetainedBeforeDiscoveryAndDuplicateRebirth(t *testing.T) {
	for _, firstState := range []bool{true, false} {
		t.Run(fmt.Sprintf("state-first-%v", firstState), func(t *testing.T) {
			r := newNHA069Rig(t, ModePassive)
			key := r.configure("retained-sensor", "retained-device")
			state := func() { r.send("zigbee2mqtt/pre/state", `{"power":10.5}`, ObservationRestored) }
			availability := func() { r.send("zigbee2mqtt/pre/availability", `{"state":"online"}`, ObservationRestored) }
			if firstState {
				state()
				availability()
			} else {
				availability()
				state()
			}
			config := `{"unique_id":"retained-sensor","device":{"identifiers":["retained-device"]},"device_class":"power","unit_of_measurement":"W","state_class":"measurement","state_topic":"zigbee2mqtt/pre/state","value_template":"{{ value_json.power }}","availability_topic":"zigbee2mqtt/pre/availability","availability_template":"{{ value_json.state }}"}`
			r.send("homeassistant/sensor/pre/config", config, ObservationRestored)
			snapshot := r.state(key)
			if _, ok := snapshot.CriticalValue(); ok {
				t.Fatal("retained state became critical")
			}
			_, quality, _, _, revision, _ := snapshot.Components()
			a, f, p, _ := quality.Components()
			if a != domain.AvailabilityAvailable || f != domain.FreshnessStale || p != domain.ProvenanceRestored {
				t.Fatalf("replayed retained quality: %v %v %v", a, f, p)
			}
			r.send("homeassistant/sensor/pre/config", config, ObservationLive)
			_, _, _, _, secondRevision, _ := r.state(key).Components()
			if secondRevision != revision {
				t.Fatalf("identical rebirth reset state %d -> %d", revision, secondRevision)
			}
		})
	}
}

func TestNHA069LiveBeforeDiscoveryIsOnlyStaleSnapshot(t *testing.T) {
	for _, caseName := range []string{"live-before-config", "retained-then-live-before-config"} {
		t.Run(caseName, func(t *testing.T) {
			r := newNHA069Rig(t, ModePassive)
			key := r.configure("prelive", "prelive-device")
			if caseName == "retained-then-live-before-config" {
				r.send("zigbee2mqtt/prelive/availability", "offline", ObservationRestored)
				r.send("zigbee2mqtt/prelive/state", "OFF", ObservationRestored)
			}
			r.send("zigbee2mqtt/prelive/availability", "online", ObservationLive)
			r.send("zigbee2mqtt/prelive/state", "ON", ObservationLive)
			r.send("homeassistant/switch/prelive/config", `{"unique_id":"prelive","device":{"identifiers":["prelive-device"]},"state_topic":"zigbee2mqtt/prelive/state","availability_topic":"zigbee2mqtt/prelive/availability"}`, ObservationRestored)
			state := r.state(key)
			if _, ok := state.CriticalValue(); ok {
				t.Fatal("replayed live snapshot became fresh")
			}
			_, quality, _, _, _, _ := state.Components()
			a, f, p, _ := quality.Components()
			if a != domain.AvailabilityAvailable || f != domain.FreshnessStale || p != domain.ProvenanceObserved {
				t.Fatalf("live snapshot quality: %v %v %v", a, f, p)
			}
			r.send("zigbee2mqtt/prelive/state", "ON", ObservationLive)
			if _, ok := r.state(key).CriticalValue(); !ok {
				t.Fatal("new live state did not become fresh")
			}
		})
	}
}

func TestNHA069AvailabilityLiveBeforeConfigWithoutReplay(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("bridge-online", "bridge-device")
	r.send("zigbee2mqtt/bridge/availability", "online", ObservationLive)
	r.send("homeassistant/switch/bridge/config", `{"unique_id":"bridge-online","device":{"identifiers":["bridge-device"]},"state_topic":"zigbee2mqtt/bridge/switch","availability_topic":"zigbee2mqtt/bridge/availability"}`, ObservationRestored)
	r.send("zigbee2mqtt/bridge/switch", "ON", ObservationLive)
	if _, ok := r.state(key).CriticalValue(); !ok {
		t.Fatal("cached online availability did not admit subsequent live state")
	}
}

func TestNHA069AvailabilityLatestDefaultAndMalformedUTF8(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("switch-avty", "device-avty")
	config := `{"unique_id":"switch-avty","device":{"identifiers":["device-avty"]},"state_topic":"zigbee2mqtt/switch-avty","availability":[{"topic":"zigbee2mqtt/avty/a"},{"topic":"zigbee2mqtt/avty/b"}]}`
	r.send("homeassistant/switch/avty/config", config, ObservationRestored)
	r.send("zigbee2mqtt/switch-avty", "ON", ObservationLive)
	_, quality, _, _, _, _ := r.state(key).Components()
	a, _, _, _ := quality.Components()
	if a != domain.AvailabilityUnknown {
		t.Fatalf("initial availability %v", a)
	}
	r.send("zigbee2mqtt/avty/a", "online", ObservationLive)
	if _, ok := r.state(key).CriticalValue(); !ok {
		t.Fatal("latest a=online not available")
	}
	r.send("zigbee2mqtt/avty/b", "offline", ObservationLive)
	_, quality, _, _, _, _ = r.state(key).Components()
	a, _, _, _ = quality.Components()
	if a != domain.AvailabilityUnavailable {
		t.Fatalf("latest b=offline: %v", a)
	}
	r.send("zigbee2mqtt/avty/a", "online", ObservationLive)
	_, quality, _, _, _, _ = r.state(key).Components()
	a, _, _, _ = quality.Components()
	if a != domain.AvailabilityUnknown {
		t.Fatalf("online without new state: %v", a)
	}
	if utf8.Valid([]byte{0xff}) {
		t.Fatal("bad test byte")
	}
	observation := Observation{topic: mustNHA069Topic(t, "homeassistant/switch/invalid/config"), payload: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, class: ObservationLive}
	if err := r.mapper.Observe(context.Background(), r.session, observation); err != nil {
		t.Fatal(err)
	}
	got := r.mapper.Diagnostics()
	if got[len(got)-1] != DiagnosticInvalidDiscovery {
		t.Fatalf("malformed UTF-8 diagnostic: %v", got)
	}
}

func TestNHA069WithdrawalFencesOneBindingBeforeBlockedSQL(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	oldKey := r.configure("old-switch", "device-a")
	neighborKey := r.configure("neighbor-switch", "device-b")
	oldTopic := "homeassistant/switch/old/config"
	neighborTopic := "homeassistant/switch/neighbor/config"
	r.send(oldTopic, `{"unique_id":"old-switch","device":{"identifiers":["device-a"]},"state_topic":"zigbee2mqtt/old"}`, ObservationRestored)
	r.send(neighborTopic, `{"unique_id":"neighbor-switch","device":{"identifiers":["device-b"]},"state_topic":"zigbee2mqtt/neighbor"}`, ObservationRestored)
	oldRuntime := r.mapper.documents[mustNHA069Topic(t, oldTopic)][0]
	neighborRuntime := r.mapper.documents[mustNHA069Topic(t, neighborTopic)][0]
	r.repo.tombstoneEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	r.repo.tombstoneRelease = release
	done := make(chan error, 1)
	go func() {
		done <- r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(t, oldTopic), class: ObservationLive})
	}()
	select {
	case <-r.repo.tombstoneEntered:
	case <-time.After(time.Second):
		t.Fatal("tombstone SQL barrier not reached")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	observation, _ := integrations.NewStateObservation("mqtt", quality, &value, nil, time.Now().UTC(), nil)
	if err := r.session.Publish(context.Background(), oldRuntime.binding, observation); err == nil {
		t.Fatal("retired binding published while SQL blocked")
	}
	event, _ := integrations.NewDeviceEvent("synthetic.press", time.Now().UTC(), nil)
	if err := r.session.Emit(context.Background(), oldRuntime.binding, event); err == nil {
		t.Fatal("retired binding emitted while SQL blocked")
	}
	if err := r.session.Publish(context.Background(), neighborRuntime.binding, observation); err != nil {
		t.Fatalf("neighbor blocked: %v", err)
	}
	if _, ok := r.state(neighborKey).CriticalValue(); !ok {
		t.Fatal("neighbor did not stay active")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked tombstone did not finish")
	}
	newSession, err := r.manager.OpenSession(r.instance)
	if err != nil {
		t.Fatal(err)
	}
	description, _ := discoveryDescription(oldRuntime.profile, ModePassive, nil)
	candidate, _ := integrations.NewCandidate(oldKey, []*registry.CapabilityDescription{description})
	if _, err := newSession.Discover(context.Background(), []integrations.Candidate{candidate}); err == nil {
		t.Fatal("new session re-admitted retired source")
	}
}

func TestNHA069PilotStopJoinsBoundedBlockedTombstone(t *testing.T) {
	broker := testbroker.Start(t)
	r := newNHA069Rig(t, ModePassive)
	r.configure("stop-switch", "stop-device")
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://" + broker.Address, Instance: "nha069-synthetic", Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "zigbee2mqtt/#"}})
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
	defer cancel()
	if err := pilot.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	topic := "homeassistant/switch/stop/config"
	broker.Publish(t, topic, []byte(`{"unique_id":"stop-switch","device":{"identifiers":["stop-device"]},"state_topic":"zigbee2mqtt/stop/state"}`), true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mapper.mu.Lock()
		count := len(r.mapper.documents)
		r.mapper.mu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("discovery not admitted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.repo.tombstoneEntered = make(chan struct{}, 1)
	broker.Publish(t, topic, nil, true)
	select {
	case <-r.repo.tombstoneEntered:
	case <-time.After(time.Second):
		t.Fatal("SQL barrier not reached")
	}
	started := time.Now()
	tasks.close(t, pilot)
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("Stop exceeded bounded SQL finalization: %v", elapsed)
	}
	if count := broker.PublishCount(settings.clientID); count != 0 {
		t.Fatalf("passive Stop published %d packets", count)
	}
}

func (r *nha069MemoryRegistry) MatchMQTTSource(_ context.Context, _ domain.IntegrationInstanceID, ids []string, unique string) (integrations.SourceKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.configured[unique]
	if !ok {
		return integrations.SourceKey{}, errors.New("not configured")
	}
	device, _, _ := key.Components()
	for _, id := range ids {
		if id == device {
			return key, nil
		}
	}
	return integrations.SourceKey{}, errors.New("configured identifier absent")
}
func (r *nha069MemoryRegistry) RememberMQTTTopics(_ context.Context, _ domain.IntegrationInstanceID, topic string, keys []integrations.SourceKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topics[topic] = append([]integrations.SourceKey(nil), keys...)
	return nil
}
func (r *nha069MemoryRegistry) MQTTTopicSources(ctx context.Context, _ domain.IntegrationInstanceID, topic string) ([]integrations.SourceKey, error) {
	if r.topicLookupEntered != nil {
		select {
		case r.topicLookupEntered <- struct{}{}:
		default:
		}
		select {
		case <-r.topicLookupRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.topicLookupError != nil {
		return nil, r.topicLookupError
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]integrations.SourceKey(nil), r.topics[topic]...), nil
}
func (r *nha069MemoryRegistry) Tombstone(ctx context.Context, entity domain.EntityID) error {
	if r.tombstoneEntered != nil {
		select {
		case r.tombstoneEntered <- struct{}{}:
		default:
		}
		select {
		case <-r.tombstoneRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retired[entity] = true
	return nil
}

type nha069Rig struct {
	t        *testing.T
	mapper   *DiscoveryMapper
	repo     *nha069MemoryRegistry
	session  *integrations.Session
	fixture  *fake.Fixture
	manager  *integrations.Manager
	instance domain.IntegrationInstanceID
}

func newNHA069Rig(t *testing.T, mode Mode) *nha069Rig {
	t.Helper()
	instance, _ := domain.ParseIntegrationInstanceID("nha069-synthetic")
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	repo := &nha069MemoryRegistry{configured: map[string]integrations.SourceKey{}, topics: map[string][]integrations.SourceKey{}, retired: map[domain.EntityID]bool{}}
	settings := Settings{profile: ProfileMQTT5, mode: mode, instance: instance}
	if mode == ModeActive {
		settings.commands = []TopicFilter{mustNHA069Filter(t, "zigbee2mqtt/+/set")}
	}
	mapper, err := newDiscoveryMapper(settings, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &nha069Rig{t: t, mapper: mapper, repo: repo, session: session, fixture: fixture, manager: host, instance: instance}
}

func mustNHA069Filter(t *testing.T, text string) TopicFilter {
	t.Helper()
	f, err := ParseTopicFilter(text)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func mustNHA069Topic(t *testing.T, text string) Topic {
	t.Helper()
	topic, err := ParseTopic(text)
	if err != nil {
		t.Fatal(err)
	}
	return topic
}
func (r *nha069Rig) configure(unique, device string) integrations.SourceKey {
	r.t.Helper()
	key, err := integrations.NewSourceKey(device, unique)
	if err != nil {
		r.t.Fatal(err)
	}
	r.repo.configured[unique] = key
	return key
}
func (r *nha069Rig) send(topic string, payload string, class ObservationClass) {
	r.t.Helper()
	err := r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(r.t, topic), payload: []byte(payload), class: class})
	if err != nil {
		r.t.Fatal(err)
	}
}
func (r *nha069Rig) state(key integrations.SourceKey) domain.ObservedState {
	r.t.Helper()
	identities, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
	if err != nil {
		r.t.Fatal(err)
	}
	state, ok := r.fixture.State(identities[0].Entity)
	if !ok {
		r.t.Fatal("missing Host state")
	}
	return state
}

func TestNHA069LightRetainedDeltaAndAvailabilityFreshness(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("lamp-unique", "configured-device")
	discovery := `{"unique_id":"lamp-unique","device":{"identifiers":["other-device","configured-device"],"via_device":"bridge"},"schema":"json","state_topic":"zigbee2mqtt/Lamp","command_topic":"zigbee2mqtt/Lamp/set","brightness":true,"brightness_scale":254,"supported_color_modes":["color_temp"],"min_mireds":250,"max_mireds":454,"effect":true,"effect_list":["blink"],"availability_topic":"zigbee2mqtt/bridge/state"}`
	r.send("homeassistant/light/lamp/config", discovery, ObservationRestored)
	if got := r.mapper.Diagnostics(); len(got) != 1 || got[0] != DiagnosticUnsupportedEffect {
		t.Fatalf("effect diagnostic: %v", got)
	}
	r.send("zigbee2mqtt/bridge/state", "online", ObservationRestored)
	r.send("zigbee2mqtt/Lamp", `{"state":"ON","brightness":127,"color_temp":300}`, ObservationRestored)
	if _, ok := r.state(key).CriticalValue(); ok {
		t.Fatal("retained light became critical")
	}
	r.send("zigbee2mqtt/Lamp", `{"state":"ON"}`, ObservationLive)
	live, ok := r.state(key).CriticalValue()
	if !ok {
		t.Fatal("live light was not critical")
	}
	light, ok := live.Light()
	if !ok {
		t.Fatal("lost structured light")
	}
	on, brightness, ct, err := light.Components()
	if err != nil || !on || brightness != nil || ct != nil {
		t.Fatalf("stale attributes promoted: %v %v %v %v", on, brightness, ct, err)
	}
	r.send("zigbee2mqtt/Lamp", `{"state":"ON","brightness":127,"color_temp":300}`, ObservationLive)
	live, ok = r.state(key).CriticalValue()
	if !ok {
		t.Fatal("complete live light missing")
	}
	light, _ = live.Light()
	on, brightness, ct, _ = light.Components()
	if !on || brightness == nil || *brightness != 50 || ct == nil || *ct != 300 {
		t.Fatalf("complete light: %v %v %v", on, brightness, ct)
	}
	r.send("zigbee2mqtt/bridge/state", "offline", ObservationLive)
	if _, ok := r.state(key).CriticalValue(); ok {
		t.Fatal("offline state critical")
	}
	r.send("zigbee2mqtt/bridge/state", "online", ObservationLive)
	state := r.state(key)
	if _, ok := state.CriticalValue(); ok {
		t.Fatal("availability-only online revived old state")
	}
	_, quality, _, _, _, _ := state.Components()
	availability, _, _, _ := quality.Components()
	if availability != domain.AvailabilityUnknown {
		t.Fatalf("online without state: %v", availability)
	}
	r.send("zigbee2mqtt/Lamp", `{"state":"OFF"}`, ObservationLive)
	live, ok = r.state(key).CriticalValue()
	if !ok {
		t.Fatal("fresh off missing")
	}
	light, _ = live.Light()
	on, brightness, ct, _ = light.Components()
	if on || brightness != nil || ct != nil {
		t.Fatalf("off invented fields: %v %v %v", on, brightness, ct)
	}
}

func TestNHA069MoistureJSONBooleanAndSensorPrecisionThroughHost(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	leak := r.configure("leak-unique", "leak-device")
	r.send("homeassistant/binary_sensor/leak/config", `{"unique_id":"leak-unique","device":{"identifiers":["leak-device"]},"device_class":"moisture","payload_on":true,"payload_off":false,"state_topic":"zigbee2mqtt/Leak","value_template":"{{ value_json.water_leak }}"}`, ObservationRestored)
	r.send("zigbee2mqtt/Leak", `{"water_leak":false}`, ObservationLive)
	value, ok := r.state(leak).CriticalValue()
	if !ok {
		t.Fatal("false was not present")
	}
	on, isSwitch := value.Switch()
	if !isSwitch || on {
		t.Fatalf("false boolean: %v %v", on, isSwitch)
	}
	for _, tc := range []struct{ class, unit, stateClass, raw string }{
		{"power", "W", "measurement", "0"}, {"power", "W", "measurement", "10.5"}, {"energy", "kWh", "total_increasing", "1e3"}, {"battery", "%", "measurement", "90"}, {"illuminance", "lx", "measurement", "42.25"},
	} {
		name := fmt.Sprintf("sensor-%s-%s", tc.class, tc.raw)
		key := r.configure(name, name)
		topic := "homeassistant/sensor/" + name + "/config"
		stateTopic := "zigbee2mqtt/" + name
		config := fmt.Sprintf(`{"unique_id":%q,"device":{"identifiers":[%q]},"device_class":%q,"unit_of_measurement":%q,"state_class":%q,"state_topic":%q,"value_template":"{{ value_json.value }}"}`, name, name, tc.class, tc.unit, tc.stateClass, stateTopic)
		r.send(topic, config, ObservationRestored)
		r.send(stateTopic, fmt.Sprintf(`{"value":%s}`, tc.raw), ObservationLive)
		state := r.state(key)
		value, ok := state.CriticalValue()
		if !ok {
			t.Fatalf("%s did not pass Host.Publish; diagnostics %v", name, r.mapper.Diagnostics())
		}
		measurement, ok := value.Measurement()
		if !ok {
			t.Fatalf("%s was not measurement", name)
		}
		parts, err := measurement.Components()
		if err != nil || parts.Precision != 3 {
			t.Fatalf("%s precision: %+v %v", name, parts, err)
		}
	}
}
