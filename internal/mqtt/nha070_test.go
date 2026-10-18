package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

func nha070JSON(t *testing.T, value map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func nha070Anchor(r *nha069Rig, unique, device, topic string) integrations.SourceKey {
	r.t.Helper()
	key := r.configure(unique, device)
	r.send("homeassistant/sensor/"+unique+"/config", nha070JSON(r.t, map[string]any{
		"unique_id": unique, "device": map[string]any{"identifiers": []string{device}},
		"device_class": "battery", "unit_of_measurement": "%", "state_topic": topic,
	}), ObservationRestored)
	return key
}

func nha070Trigger(r *nha069Rig, discovery, device, actionTopic, action string) {
	r.t.Helper()
	r.send(discovery, nha070JSON(r.t, map[string]any{
		"automation_type": "trigger", "type": "action", "subtype": action,
		"payload": action, "topic": actionTopic,
		"device": map[string]any{"identifiers": []string{device}},
		"origin": map[string]any{"name": "Zigbee2MQTT", "sw_version": "2.14.1"},
	}), ObservationRestored)
}

func nha070Events(t *testing.T, r *nha069Rig, want ...string) {
	t.Helper()
	events, err := r.fixture.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i, event := range events {
		code, _, sourceSequence, err := event.Event.Components()
		if err != nil || code != want[i] || sourceSequence != nil {
			t.Fatalf("event[%d] = %q, seq %v, err %v", i, code, sourceSequence, err)
		}
	}
}

func TestNHA070SameDeviceMultipleBindingsAndAmbiguousDevices(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	battery := nha070Anchor(r, "a-battery", "wire-one", "zigbee2mqtt/DESK Switch/battery")
	nha070Anchor(r, "z-other", "wire-one", "zigbee2mqtt/DESK Switch/other")
	nha070Anchor(r, "other-battery", "wire-two", "zigbee2mqtt/OTHER/battery")
	actionTopic := "zigbee2mqtt/DESK Switch/action"
	nha070Trigger(r, "homeassistant/device_automation/wire-one/on/config", "wire-one", actionTopic, "on")
	r.send(actionTopic, "on", ObservationLive)
	nha070Events(t, r, "z2m.on")
	ids, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{battery})
	if err != nil || len(ids) != 1 {
		t.Fatalf("anchor identity: %v %v", ids, err)
	}
	events, _ := r.fixture.Events()
	if events[0].Entity != ids[0].Entity {
		t.Fatal("event did not use stable same-device anchor")
	}
	r.send("homeassistant/device_automation/ambiguous/config", nha070JSON(t, map[string]any{
		"automation_type": "trigger", "type": "action", "subtype": "off", "payload": "off",
		"topic": "zigbee2mqtt/AMBIG/action", "device": map[string]any{"identifiers": []string{"wire-one", "wire-two"}},
	}), ObservationRestored)
	r.send("zigbee2mqtt/AMBIG/action", "off", ObservationLive)
	nha070Events(t, r, "z2m.on")
	if !nha070HasDiagnostic(r, DiagnosticAmbiguousActionDevice) {
		t.Fatal("distinct configured device IDs were not diagnosed")
	}
}

func nha070HasDiagnostic(r *nha069Rig, wanted DiscoveryDiagnostic) bool {
	for _, got := range r.mapper.Diagnostics() {
		if got == wanted {
			return true
		}
	}
	return false
}

func TestNHA070CanonicalTopicPinnedAndLegacyClear(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "remote-battery", "remote-device", "zigbee2mqtt/SONOS-REMOTE-KITCHEN/battery")
	legacyTopic := "homeassistant/sensor/remote-action/config"
	jsonTopic := "zigbee2mqtt/SONOS-REMOTE-KITCHEN"
	actionTopic := jsonTopic + "/action"
	r.send(legacyTopic, nha070JSON(t, map[string]any{
		"unique_id": "remote-action", "device": map[string]any{"identifiers": []string{"remote-device"}},
		"state_topic": jsonTopic, "value_template": `{{ value_json["action"] }}`,
	}), ObservationRestored)
	r.send(jsonTopic, `{"action":"toggle"}`, ObservationLive)
	// Z2M 2.14.1 may discover /action between its JSON and /action publish.
	nha070Trigger(r, "homeassistant/device_automation/remote/toggle/config", "remote-device", actionTopic, "toggle")
	r.send(actionTopic, "toggle", ObservationLive)
	nha070Events(t, r, "z2m.toggle")
	r.send(jsonTopic, `{"action":""}`, ObservationLive)
	r.send(jsonTopic, `{"action":"toggle"}`, ObservationLive)
	nha070Events(t, r, "z2m.toggle", "z2m.toggle")
	r.send(jsonTopic, `{"action":"unlisted"}`, ObservationLive)
	nha070Events(t, r, "z2m.toggle", "z2m.toggle")
	if !nha070HasDiagnostic(r, DiagnosticInvalidAction) {
		t.Fatal("unknown legacy payload was not diagnosed")
	}
	// Removing only the legacy declaration leaves the configured battery intact,
	// and does not activate the previously ignored /action route.
	r.send(legacyTopic, "", ObservationRestored)
	r.send(actionTopic, "toggle", ObservationLive)
	r.send(jsonTopic, `{"action":"toggle"}`, ObservationLive)
	nha070Events(t, r, "z2m.toggle", "z2m.toggle")
}

func TestNHA070TriggerPayloadRemovalKeepsTopicAndExactTopicSelection(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "desk-battery", "desk-device", "zigbee2mqtt/DESK Switch/battery")
	first := "zigbee2mqtt/DESK Switch/action"
	second := "zigbee2mqtt/DESK Switch/other/action"
	nha070Trigger(r, "homeassistant/device_automation/desk/on/config", "desk-device", first, "on")
	nha070Trigger(r, "homeassistant/device_automation/desk/off/config", "desk-device", first, "off")
	nha070Trigger(r, "homeassistant/device_automation/desk/other/config", "desk-device", second, "on")
	r.send(first, "on", ObservationLive)
	r.send(second, "on", ObservationLive)
	nha070Events(t, r, "z2m.on")
	r.send("homeassistant/device_automation/desk/on/config", "", ObservationRestored)
	r.send(first, "on", ObservationLive)
	r.send(first, "off", ObservationLive)
	nha070Events(t, r, "z2m.on", "z2m.off")
	r.send("homeassistant/device_automation/desk/off/config", "", ObservationRestored)
	r.send(second, "on", ObservationLive)
	nha070Trigger(r, "homeassistant/device_automation/desk/on/config", "desk-device", first, "on")
	r.send(first, "on", ObservationLive)
	nha070Events(t, r, "z2m.on", "z2m.off")
}

func TestNHA070RetainedSnapshotRebirthAndAnchorRetirement(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	const actionTopic = "zigbee2mqtt/IKEA-SW-01/action"
	// This was live on arrival, but no authorized discovery existed then.
	r.send(actionTopic, "on", ObservationLive)
	r.send(actionTopic, "off", ObservationRestored)
	key := nha070Anchor(r, "ikea-battery", "ikea-device", "zigbee2mqtt/IKEA-SW-01/battery")
	declaration := "homeassistant/device_automation/ikea/on/config"
	nha070Trigger(r, declaration, "ikea-device", actionTopic, "on")
	nha070Events(t, r)
	r.send(actionTopic, "on", ObservationRestored)
	nha070Trigger(r, declaration, "ikea-device", actionTopic, "on") // rebirth
	nha070Events(t, r)
	r.send(actionTopic, "on", ObservationLive)
	time.Sleep(100 * time.Millisecond)
	r.send(actionTopic, "on", ObservationLive)
	nha070Events(t, r, "z2m.on", "z2m.on")
	identities, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	selected := r.mapper.actionDevices[identities[0].Device]
	if selected == nil {
		t.Fatal("no action anchor")
	}
	old := selected.binding
	r.send("homeassistant/sensor/ikea-battery/config", "", ObservationRestored)
	r.send(actionTopic, "on", ObservationLive)
	nha070Events(t, r, "z2m.on", "z2m.on")
	event, err := integrations.NewDeviceEvent("z2m.on", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.session.Emit(context.Background(), old, event); err == nil {
		t.Fatal("retired Host binding emitted after anchor removal")
	}
}

func TestNHA070OriginalLiveJSONSnapshotNeverEmitsOnDiscoveryOrRefresh(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	const stateTopic = "zigbee2mqtt/BATHROOM-REMOTE-SONOS"
	r.send(stateTopic, `{"action":"volume_up","battery":88}`, ObservationLive)
	key := r.configure("bath-battery", "bath-device")
	batteryDiscovery := `{"unique_id":"bath-battery","device":{"identifiers":["bath-device"]},"device_class":"battery","unit_of_measurement":"%","state_topic":"zigbee2mqtt/BATHROOM-REMOTE-SONOS","value_template":"{{ value_json.battery }}"}`
	r.send("homeassistant/sensor/bath-battery/config", batteryDiscovery, ObservationRestored)
	legacyDiscovery := `{"unique_id":"bath-action","device":{"identifiers":["bath-device"]},"state_topic":"zigbee2mqtt/BATHROOM-REMOTE-SONOS","value_template":"{{ value_json[\"action\"] }}"}`
	r.send("homeassistant/sensor/bath-action/config", legacyDiscovery, ObservationRestored)
	nha070Events(t, r)
	_, quality, _, _, _, err := r.state(key).Components()
	if err != nil {
		t.Fatal(err)
	}
	_, freshness, _, err := quality.Components()
	if err != nil || freshness != domain.FreshnessStale {
		t.Fatalf("cached live state did not replay stale: %v %v", freshness, err)
	}
	r.send("homeassistant/sensor/bath-action/config", legacyDiscovery, ObservationRestored)                                                                                                                                                                                                                                // rebirth
	r.send("homeassistant/sensor/bath-battery/config", `{"unique_id":"bath-battery","device":{"identifiers":["bath-device"]},"device_class":"battery","unit_of_measurement":"%","icon":"mdi:battery","state_topic":"zigbee2mqtt/BATHROOM-REMOTE-SONOS","value_template":"{{ value_json.battery }}"}`, ObservationRestored) // hot refresh
	nha070Events(t, r)
	r.send(stateTopic, `{"action":"volume_up","battery":88}`, ObservationLive)
	nha070Events(t, r, "z2m.volume_up")
}

func TestNHA070CompositeDeviceTriggerAndBounds(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "composite-battery", "composite-device", "zigbee2mqtt/KITCHEN-IKEA-SWITCH/battery")
	r.send("homeassistant/device/combined/config", nha070JSON(t, map[string]any{
		"origin": map[string]any{"name": "Zigbee2MQTT"}, "device": map[string]any{"identifiers": []string{"composite-device"}},
		"components": map[string]any{"press": map[string]any{
			"p": "device_automation", "automation_type": "trigger", "type": "action", "subtype": "on",
			"payload": "on", "topic": "zigbee2mqtt/KITCHEN-IKEA-SWITCH/action",
		}},
	}), ObservationRestored)
	r.send("zigbee2mqtt/KITCHEN-IKEA-SWITCH/action", "on", ObservationLive)
	nha070Events(t, r, "z2m.on")
	// A trigger-only document removal never tombstones its battery anchor.
	r.send("homeassistant/device/combined/config", "", ObservationRestored)
	r.send("zigbee2mqtt/KITCHEN-IKEA-SWITCH/action", "on", ObservationLive)
	nha070Events(t, r, "z2m.on")
	if len(r.mapper.entities) != 1 {
		t.Fatal("trigger deletion retired the independent battery anchor")
	}
	declarations := make([]actionDeclaration, 65)
	topic := mustNHA069Topic(t, "zigbee2mqtt/KITCHEN-IKEA-SWITCH/action")
	for i := range declarations {
		declarations[i] = actionDeclaration{identifiers: []string{"composite-device"}, topic: topic, kind: actionTopic, action: 1}
	}
	if err := r.mapper.setActionDocument(mustNHA069Topic(t, "homeassistant/device_automation/overflow/config"), declarations); !errors.Is(err, ErrObservationGap) {
		t.Fatalf("per-device overflow was not explicit: %v", err)
	}
}

func TestNHA070AllInventoriedActionsHaveCanonicalCodes(t *testing.T) {
	// Distinct payloads transcribed from the redacted 57-row Z2M inventory
	// (MQTT - contrats de découverte, lines 5488-5544). The synthetic test
	// source/identity exercises every payload through Observe, Host and journal;
	// the inventory does not contain wire IDs to authorize a real installation.
	var inventory = [...]string{
		"on", "off", "toggle", "brightness_move_up", "brightness_move_down", "brightness_stop",
		"arrow_left_click", "arrow_right_click", "volume_up", "volume_down", "volume_up_hold",
		"volume_down_hold", "track_next", "track_previous", "dots_1_initial_press",
		"dots_1_long_press", "dots_1_double_press", "dots_1_short_release", "dots_1_long_release",
		"dots_2_initial_press", "dots_2_long_press", "dots_2_double_press",
		"dots_2_short_release", "dots_2_long_release",
	}
	if len(inventory) != 24 {
		t.Fatal("fixture must cover all 24 distinct inventory payloads")
	}
	r := newNHA069Rig(t, ModePassive)
	const topic = "zigbee2mqtt/corpus-synthetic/action"
	nha070Anchor(r, "corpus-battery", "corpus-device", "zigbee2mqtt/corpus-synthetic/battery")
	for i, payload := range inventory {
		nha070Trigger(r, fmt.Sprintf("homeassistant/device_automation/corpus/%02d/config", i), "corpus-device", topic, payload)
	}
	for _, payload := range inventory {
		r.send(topic, payload, ObservationLive)
	}
	events, err := r.fixture.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(inventory) {
		t.Fatalf("journal actions = %d, want %d", len(events), len(inventory))
	}
	for i, payload := range inventory {
		code, _, sequence, err := events[i].Event.Components()
		if err != nil || code != "z2m."+payload || sequence != nil {
			t.Fatalf("journal action %d: code %q, sequence %v, error %v; want %q", i, code, sequence, err, payload)
		}
	}
}

func TestNHA070MixedUpdateRejectedAsOneGeneration(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "mixed-battery", "mixed-device", "zigbee2mqtt/IKEA-SW-02/battery")
	const discovery = "homeassistant/device/mixed/config"
	base := map[string]any{
		"origin": map[string]any{"name": "Zigbee2MQTT"},
		"device": map[string]any{"identifiers": []string{"mixed-device"}},
		"components": map[string]any{"press": map[string]any{
			"p": "device_automation", "automation_type": "trigger", "type": "action", "subtype": "on",
			"payload": "on", "topic": "zigbee2mqtt/IKEA-SW-02/action",
		}},
	}
	r.send(discovery, nha070JSON(t, base), ObservationRestored)
	r.send("zigbee2mqtt/IKEA-SW-02/action", "on", ObservationLive)
	nha070Events(t, r, "z2m.on")
	base["components"] = map[string]any{
		"press": map[string]any{
			"p": "device_automation", "automation_type": "trigger", "type": "action", "subtype": "off",
			"payload": "off", "topic": "zigbee2mqtt/IKEA-SW-02/action",
		},
		"unconfigured": map[string]any{
			"p": "sensor", "unique_id": "unconfigured-neighbor", "state_topic": "zigbee2mqtt/IKEA-SW-02/other",
			"device_class": "battery", "unit_of_measurement": "%",
		},
	}
	r.send(discovery, nha070JSON(t, base), ObservationRestored)
	r.send("zigbee2mqtt/IKEA-SW-02/action", "off", ObservationLive)
	r.send("zigbee2mqtt/IKEA-SW-02/action", "on", ObservationLive)
	nha070Events(t, r, "z2m.on", "z2m.on")
	if !nha070HasDiagnostic(r, DiagnosticUnconfiguredSource) {
		t.Fatal("invalid mixed update had no source diagnostic")
	}
}

func TestNHA070ActionDiscoveryRejectsForeignAndInvalidFields(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "field-battery", "field-device", "zigbee2mqtt/field/battery")
	base := map[string]any{
		"automation_type": "trigger", "type": "action", "subtype": "on", "payload": "on",
		"topic": "zigbee2mqtt/field/action", "device": map[string]any{"identifiers": []string{"field-device"}},
	}
	for name, mutate := range map[string]func(map[string]any){
		"invalid origin":      func(v map[string]any) { v["origin"] = 17 },
		"foreign command":     func(v map[string]any) { v["command_topic"] = "zigbee2mqtt/field/set" },
		"foreign light":       func(v map[string]any) { v["brightness"] = true },
		"contradictory alias": func(v map[string]any) { v["atype"] = "trigger" },
		"future qos":          func(v map[string]any) { v["qos"] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			v := make(map[string]any, len(base)+1)
			for key, value := range base {
				v[key] = value
			}
			mutate(v)
			r.send("homeassistant/device_automation/field/"+name+"/config", nha070JSON(t, v), ObservationRestored)
			r.send("zigbee2mqtt/field/action", "on", ObservationLive)
			nha070Events(t, r)
		})
	}
	if !nha070HasDiagnostic(r, DiagnosticInvalidDiscovery) {
		t.Fatal("invalid trigger fields were not diagnosed")
	}
}

func TestNHA070HistoricalChoicesAreBoundedAndEdgeLossExplicit(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "loss-battery", "loss-device", "zigbee2mqtt/loss/battery")
	for i := 0; i < 1024; i++ {
		id, err := domain.ParseDeviceID(fmt.Sprintf("historical-device-%04d", i))
		if err != nil {
			t.Fatal(err)
		}
		r.mapper.actionDevices[id] = &actionDevice{selection: selectionDisabled}
	}
	declaration := actionDeclaration{identifiers: []string{"loss-device"}, topic: mustNHA069Topic(t, "zigbee2mqtt/loss/action"), kind: actionTopic, action: 1}
	if err := r.mapper.setActionDocument(mustNHA069Topic(t, "homeassistant/device_automation/loss/config"), []actionDeclaration{declaration}); !errors.Is(err, ErrObservationGap) {
		t.Fatalf("historical choice overflow did not fail closed: %v", err)
	}
	if len(r.mapper.actionDevices) != 1024 {
		t.Fatal("historical choice was evicted")
	}
	// Use a fresh bounded fixture for the subscriber proof.
	r = newNHA069Rig(t, ModePassive)
	nha070Anchor(r, "loss-battery", "loss-device", "zigbee2mqtt/loss/battery")
	nha070Trigger(r, "homeassistant/device_automation/loss/on/config", "loss-device", "zigbee2mqtt/loss/action", "on")
	sub, _, err := r.fixture.Sequencer().SubscribeEdge()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for i := 0; i < 130; i++ {
		r.send("zigbee2mqtt/loss/action", "on", ObservationLive)
	}
	status, err := sub.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Lost == 0 || !status.Degraded || status.PendingLoss == nil || status.PendingLoss.Reason != sequencer.LossEdgeOverflow {
		t.Fatalf("bounded edge loss was not explicit: %+v", status)
	}
}
