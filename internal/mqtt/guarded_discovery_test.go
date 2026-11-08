package mqtt

import (
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestCurrentGuardedSwitchDiscovery(t *testing.T) {
	topic, _ := ParseTopic("homeassistant/switch/synthetic/switch/config")
	payload := []byte(`{"availability":[{"topic":"zigbee2mqtt/bridge/state","value_template":"{{ value_json.state }}"}],"command_topic":"zigbee2mqtt/Synthetic relay/set","default_entity_id":"switch.synthetic-relay","device":{"hw_version":"1","identifiers":["synthetic-relay"],"manufacturer":"IKEA","model":"TRETAKT smart plug","model_id":"E22x4","name":"Synthetic relay","sw_version":"2.4.25","via_device":"synthetic-bridge"},"name":null,"object_id":"synthetic-relay","origin":{"name":"Zigbee2MQTT","sw":"2.14.3","url":"https://www.zigbee2mqtt.io"},"payload_off":"OFF","payload_on":"ON","state_topic":"zigbee2mqtt/Synthetic relay","unique_id":"synthetic-switch","value_template":"{% if \"state\" in value_json %}{{ value_json[\"state\"] }}{% endif %}"}`)
	_, _, _, err := parseDiscoveryDocument(topic, payload)
	if err != nil {
		t.Fatal(err)
	}
}

func TestGuardedDiscoveryPartialStateDoesNotRefresh(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("synthetic-switch", "synthetic-device")
	r.send("homeassistant/switch/synthetic/config", `{"unique_id":"synthetic-switch","device":{"identifiers":["synthetic-device"]},"state_topic":"zigbee2mqtt/synthetic","value_template":"{% if \"state\" in value_json %}{{ value_json[\"state\"] }}{% endif %}"}`, ObservationRestored)
	r.send("zigbee2mqtt/synthetic", `{"state":"ON"}`, ObservationRestored)
	before := r.state(key)
	r.send("zigbee2mqtt/synthetic", `{"linkquality":100}`, ObservationLive)
	after := r.state(key)
	_, bq, bt, bs, br, _ := before.Components()
	_, aq, at, as, ar, _ := after.Components()
	if bq != aq || bt != at || bs != as || br != ar {
		t.Fatal("partial packet renewed quality")
	}
	if _, ok := after.CriticalValue(); ok {
		t.Fatal("retained state became fresh")
	}
	r.send("zigbee2mqtt/synthetic", `{"state":"OFF"}`, ObservationLive)
	if _, ok := r.state(key).CriticalValue(); !ok {
		t.Fatal("real state not fresh")
	}
	_, q, _, _, _, _ := r.state(key).Components()
	_, f, p, _ := q.Components()
	if f != domain.FreshnessFresh || p != domain.ProvenanceObserved {
		t.Fatal("wrong quality")
	}
	if len(r.mapper.Diagnostics()) != 0 {
		t.Fatal(r.mapper.Diagnostics())
	}
}
func TestGuardedDiscoveryRejectsOtherBlocks(t *testing.T) {
	for _, source := range []string{
		`{% if "state" in value_json %}{{ value_json["other"] }}{% endif %}`,
		`{% if "state' in value_json %}{{ value_json["state"] }}{% endif %}`,
		`{% if "state" in value_json %}{{ value_json["state"] }}{% else %}OFF{% endif %}`,
		`{% if "state" in value_json %}{{ value_json["state"] | lower }}{% endif %}`,
	} {
		if _, field := guardedDiscoveryValue(source); field != "" {
			t.Fatal("unexpected template admitted")
		}
	}
}

func TestGuardedDiscoveryMalformedPayloadPreservesObservation(t *testing.T) {
	r := newNHA069Rig(t, ModePassive)
	key := r.configure("guarded-switch", "guarded-device")
	r.send("homeassistant/switch/guarded/config", `{"unique_id":"guarded-switch","device":{"identifiers":["guarded-device"]},"state_topic":"zigbee2mqtt/guarded","value_template":"{% if 'state' in value_json %}{{ value_json['state'] }}{% endif %}"}`, ObservationRestored)
	r.send("zigbee2mqtt/guarded", `{"state":"OFF"}`, ObservationLive)
	_, _, _, _, revision, _ := r.state(key).Components()
	for _, payload := range []string{`{"state":null}`, `{"state":"ON","state":"OFF"}`, `[]`, `{"state":"ON"} trailing`} {
		r.send("zigbee2mqtt/guarded", payload, ObservationLive)
		_, _, _, _, after, _ := r.state(key).Components()
		if revision != after {
			t.Fatal("invalid packet changed observation")
		}
	}
	if len(r.mapper.Diagnostics()) != 4 {
		t.Fatalf("expected four invalid packets: %v", r.mapper.Diagnostics())
	}
}
