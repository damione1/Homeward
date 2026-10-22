package mqtt

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/eclipse/paho.golang/paho"
)

func TestNHA114CorrelationCodecStrictBoundsAndPresence(t *testing.T) {
	scope := correlationScope{1}
	cause, _ := domain.ParseCausationID("event-114")
	trace, _ := domain.ParseCorrelationID("cmd-114")
	for _, tc := range []struct {
		cause *domain.CausationID
		trace *domain.CorrelationID
	}{{nil, nil}, {&cause, nil}, {nil, &trace}, {&cause, &trace}} {
		causality, err := domain.NewCausality(tc.cause, tc.trace)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encodeCorrelation(causality, scope)
		if err != nil {
			t.Fatal(err)
		}
		wire := []byte(encoded.wire)
		parsed, err := parseCorrelation(wire)
		if err != nil {
			t.Fatal(err)
		}
		wire[0] = 255 // The admitted value must not alias caller packet bytes.
		got, err := parsed.causality()
		if err != nil || got != causality {
			t.Fatalf("roundtrip: %+v %v", got, err)
		}
	}
	longCause, _ := domain.ParseCausationID(strings.Repeat("c", maxCausalID))
	longTrace, _ := domain.ParseCorrelationID(strings.Repeat("t", maxCausalID))
	maximal, _ := domain.NewCausality(&longCause, &longTrace)
	encoded, err := encodeCorrelation(maximal, scope)
	if err != nil || len(encoded.wire) != maxCorrelation {
		t.Fatalf("maximal metadata: %d %v", len(encoded.wire), err)
	}
	tooLongCause, _ := domain.ParseCausationID(strings.Repeat("c", maxCausalID+1))
	tooLongTrace, _ := domain.ParseCorrelationID(strings.Repeat("t", maxCausalID+1))
	for _, ids := range []struct {
		cause *domain.CausationID
		trace *domain.CorrelationID
	}{{&tooLongCause, nil}, {nil, &tooLongTrace}} {
		causality, _ := domain.NewCausality(ids.cause, ids.trace)
		if _, err := encodeCorrelation(causality, scope); !errors.Is(err, domain.ErrInvalidCausality) {
			t.Fatalf("oversized ID admitted: %v", err)
		}
	}
	if _, err := encodeCorrelation(maximal, correlationScope{}); !errors.Is(err, domain.ErrInvalidCausality) {
		t.Fatalf("zero outgoing scope admitted: %v", err)
	}
	framed := func(flags byte, tail ...byte) []byte {
		return append(append([]byte{correlationVersion, flags}, scope[:]...), tail...)
	}
	for name, wire := range map[string][]byte{
		"absent": nil, "short": {correlationVersion}, "legacy": {1, 0},
		"version": append([]byte{255}, []byte(encoded.wire)[1:]...),
		"flags":   framed(4), "scope-absent": {correlationVersion, 0},
		"truncated-scope": append([]byte{correlationVersion, 0}, scope[:correlationScopeSize-1]...),
		"zero-scope":      append([]byte{correlationVersion, 0}, make([]byte, correlationScopeSize)...),
		"empty-present":   framed(1, 0, 0), "truncated-length": framed(1, 0),
		"truncated-value": framed(2, 0, 2, 'a'), "invalid-utf8": framed(2, 0, 1, 255),
		"oversized-field": append(framed(2, 1, 1), bytes.Repeat([]byte{'x'}, 257)...),
		"trailing":        framed(0, 'x'), "oversized-total": bytes.Repeat([]byte{1}, maxCorrelation+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCorrelation(wire); !errors.Is(err, domain.ErrInvalidCausality) {
				t.Fatalf("malformed metadata admitted: %v", err)
			}
		})
	}
}

func TestNHA114CommandMetadataValidationPrecedesTransport(t *testing.T) {
	settings := nha068Settings(t, "127.0.0.1:1", ModeActive)
	pilot, err := NewPilot(settings, UnmappedObservations{})
	if err != nil {
		t.Fatal(err)
	}
	command, err := NewCommandPublication(mustNHA069Topic(t, "nha068/command/relay"), []byte("on"))
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := domain.ParseCorrelationID("test-command")
	cause, _ := domain.NewCausality(nil, &trace)
	outside, err := command.withResponseCausality(mustNHA069Topic(t, "foreign/reply"), cause, correlationScope{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := pilot.WriteCommand(context.Background(), outside); !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("foreign response route admitted: %v", err)
	}
	command.responseTopic = mustNHA069Topic(t, "nha068/state/relay")
	command.correlation = correlationMetadata{wire: string(bytes.Repeat([]byte{1}, maxCorrelation+1))}
	if err := pilot.WriteCommand(context.Background(), command); !errors.Is(err, domain.ErrInvalidCausality) {
		t.Fatalf("malformed outgoing metadata admitted: %v", err)
	}
}

// This test uses the existing action authority and real isolated MQTT transport.
// The synthetic endpoint explicitly echoes opaque MQTT5 metadata; ordinary HA
// discovery/Zigbee2MQTT peers are not assumed to implement that echo.
func TestNHA114MQTT5ExplicitEchoConfirmsOnlyCurrentDirectState(t *testing.T) {
	r := newNHA180Rig(t, false)
	producer := r.broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha114-echo", true, 0, nil)
	on := true
	first := nha114DispatchPacket(t, r, "nha114-echo-first", &actions.SetLight{On: &on, BrightnessPercent: ptrNHA180(50)})
	if first.Properties == nil || first.Properties.ResponseTopic != "zigbee2mqtt/kitchen/state" || first.Retain || first.QoS != 0 {
		t.Fatalf("command metadata: %+v", first)
	}
	cause, err := r.driver.last.Causality()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := encodeCorrelation(cause, nha114CurrentScope(r))
	if err != nil || string(first.Properties.CorrelationData) != expected.wire {
		t.Fatalf("command correlation differs from admitted command: %v", err)
	}
	foreignID, _ := domain.ParseCorrelationID("foreign")
	foreignCause, _ := domain.NewCausality(nil, &foreignID)
	foreign, err := encodeCorrelation(foreignCause, nha114CurrentScope(r))
	if err != nil {
		t.Fatal(err)
	}
	good := []byte(`{"state":"ON","brightness":127}`)
	for _, tc := range []struct {
		name                 string
		payload, correlation []byte
	}{
		{"plain-state", good, nil},
		{"incompatible", []byte(`{"state":"OFF","brightness":127}`), first.Properties.CorrelationData},
		{"malformed", good, []byte{2, 0}},
		{"oversized", good, bytes.Repeat([]byte{'x'}, maxCorrelation+1)},
		{"unknown-token", good, []byte(foreign.wire)},
		// ON and brightness=127 are already in the cache; neither partial
		// packet may turn the other cached field into direct proof.
		{"partial-on", []byte(`{"state":"ON"}`), first.Properties.CorrelationData},
		{"partial-brightness", []byte(`{"brightness":127}`), first.Properties.CorrelationData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nha114PublishAndWait(t, r, producer, first.Properties.ResponseTopic, tc.payload, tc.correlation, false)
			r.status(t, "nha114-echo-first", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
		})
	}
	nha114PublishAndWait(t, r, producer, first.Properties.ResponseTopic, good, first.Properties.CorrelationData, false)
	r.status(t, "nha114-echo-first", actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	second := nha114DispatchPacket(t, r, "nha114-echo-second", &actions.SetLight{On: &on, BrightnessPercent: ptrNHA180(50)})
	if bytes.Equal(first.Properties.CorrelationData, second.Properties.CorrelationData) {
		t.Fatal("distinct commands share correlation")
	}
	nha114PublishAndWait(t, r, producer, second.Properties.ResponseTopic, good, first.Properties.CorrelationData, false)
	r.status(t, "nha114-echo-second", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	nha114PublishAndWait(t, r, producer, second.Properties.ResponseTopic, good, second.Properties.CorrelationData, false)
	r.status(t, "nha114-echo-second", actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	if got := r.broker.PublishCount(r.settings.clientID); got != 2 {
		t.Fatalf("echo caused %d commands, want2", got)
	}
}

func TestNHA114RetainedEchoCannotConfirm(t *testing.T) {
	r := newNHA180Rig(t, false)
	producer := r.broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha114-retained", true, 0, nil)
	on := true
	packet := nha114DispatchPacket(t, r, "nha114-retained-command", &actions.SetLight{On: &on})
	client := r.driver.pilot.Pilot.client
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ack, err := client.Unsubscribe(ctx, &paho.Unsubscribe{Topics: []string{"zigbee2mqtt/+/state"}})
	if err != nil || ack == nil || len(ack.Reasons) != 1 || ack.Reasons[0] >= 0x80 {
		t.Fatalf("isolated unsubscribe: %+v %v", ack, err)
	}
	// Store the echo while the pilot has no state subscription, then request
	// the retained snapshot. RAP=false must classify it as restoration.
	nha114Publish(t, producer, packet.Properties.ResponseTopic, []byte(`{"state":"ON"}`), packet.Properties.CorrelationData, true)
	before := nha114StateRevision(r)
	suback, err := client.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: "zigbee2mqtt/+/state", QoS: 1, RetainAsPublished: false}}})
	if err != nil || suback == nil || len(suback.Reasons) != 1 || suback.Reasons[0] >= 0x80 {
		t.Fatalf("isolated resubscribe: %+v %v", suback, err)
	}
	nha114WaitStateReceipt(t, r, before)
	r.status(t, "nha114-retained-command", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	state, _ := r.fixture.State(r.entity)
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		t.Fatal(err)
	}
	_, freshness, provenance, err := quality.Components()
	if err != nil || freshness != domain.FreshnessStale || provenance != domain.ProvenanceRestored {
		t.Fatalf("retained echo quality: %+v %v", quality, err)
	}
	if got := r.broker.PublishCount(r.settings.clientID); got != 1 {
		t.Fatalf("restoration replayed command: %d", got)
	}
}

func TestNHA114AvailabilityAndSnapshotsNeverReuseCorrelation(t *testing.T) {
	r := newNHA180Rig(t, false)
	discovery := strings.TrimSuffix(nha178DiscoveryBase, "}") + `,"availability_topic":"zigbee2mqtt/kitchen/availability"}`
	r.send(nha178DiscoveryTopic, discovery, ObservationRestored)
	on := true
	packet := nha114DispatchPacket(t, r, "nha114-unavailable-echo", &actions.SetLight{On: &on})
	metadata, err := parseCorrelation(packet.Properties.CorrelationData)
	if err != nil {
		t.Fatal(err)
	}
	// An echoed state while availability is unknown remains ordinary state.
	if err := r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(t, "zigbee2mqtt/kitchen/state"), payload: []byte(`{"state":"ON"}`), class: ObservationLive, correlation: metadata}); err != nil {
		t.Fatal(err)
	}
	r.status(t, "nha114-unavailable-echo", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	// Availability republishes the cached compatible value, without proof.
	if err := r.mapper.Observe(context.Background(), r.session, Observation{topic: mustNHA069Topic(t, "zigbee2mqtt/kitchen/availability"), payload: []byte("online"), class: ObservationLive, correlation: metadata}); err != nil {
		t.Fatal(err)
	}
	r.status(t, "nha114-unavailable-echo", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	// Explicit snapshot replay likewise has neither live quality nor metadata.
	r.mapper.mu.Lock()
	runtime := r.mapper.entities[r.entity]
	err = r.mapper.replaySnapshots(context.Background(), r.session, runtime)
	r.mapper.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.status(t, "nha114-unavailable-echo", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	// A fresh mapper is the factory's reconnect boundary: no lastValue,
	// snapshot or packet correlation is transferable into the next session.
	fresh, err := newDiscoveryMapper(r.settings, r.repo, nil)
	if err != nil || len(fresh.snapshots) != 0 || len(fresh.entities) != 0 {
		t.Fatalf("fresh mapper carried state: %+v %v", fresh, err)
	}
}

func TestNHA114EchoCannotCrossEntityProfileOrSession(t *testing.T) {
	for _, boundary := range []string{"entity", "profile", "session"} {
		t.Run(boundary, func(t *testing.T) {
			r := newNHA180Rig(t, false)
			on := true
			packet := nha114DispatchPacket(t, r, "nha114-boundary-"+boundary, &actions.SetLight{On: &on})
			metadata, err := parseCorrelation(packet.Properties.CorrelationData)
			if err != nil {
				t.Fatal(err)
			}
			mapper, session := r.mapper, r.session
			stateTopic := packet.Properties.ResponseTopic
			switch boundary {
			case "entity":
				key := r.configure("other-driver", "other-device")
				ids, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
				if err != nil {
					t.Fatal(err)
				}
				alias, _ := domain.ParseHAAlias("light.nha114_other")
				record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, r.instance, key, alias, alias, []domain.HAAlias{alias}, false)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := r.fixture.Sequencer().Capture()
				if err != nil {
					t.Fatal(err)
				}
				records := []integrations.RegistryRecord{record}
				for _, existing := range snapshot.Registry {
					records = append(records, existing)
				}
				if err := r.fixture.PublishRegistry(context.Background(), records); err != nil {
					t.Fatal(err)
				}
				other := strings.ReplaceAll(strings.ReplaceAll(nha178DiscoveryBase, "kitchen-driver", "other-driver"), "kitchen-device", "other-device")
				other = strings.ReplaceAll(other, "zigbee2mqtt/kitchen/", "zigbee2mqtt/other/")
				r.send("homeassistant/light/other/config", other, ObservationRestored)
				if len(r.mapper.entities) != 2 {
					t.Fatal("second entity was not admitted")
				}
				stateTopic = "zigbee2mqtt/other/state"
			case "profile":
				r.send(nha178DiscoveryTopic, strings.Replace(nha178DiscoveryBase, `"brightness_scale":254`, `"brightness_scale":255`, 1), ObservationLive)
			case "session":
				session, err = r.manager.OpenSession(r.instance)
				if err != nil {
					t.Fatal(err)
				}
				mapper, err = newDiscoveryMapper(r.settings, r.repo, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := mapper.Observe(context.Background(), session, Observation{topic: mustNHA069Topic(t, nha178DiscoveryTopic), payload: []byte(nha178DiscoveryBase), class: ObservationRestored}); err != nil {
					t.Fatal(err)
				}
			}
			if err := mapper.Observe(context.Background(), session, Observation{topic: mustNHA069Topic(t, stateTopic), payload: []byte(`{"state":"ON"}`), class: ObservationLive, correlation: metadata}); err != nil {
				t.Fatal(err)
			}
			r.status(t, "nha114-boundary-"+boundary, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
		})
	}
}

func nha114DispatchPacket(t *testing.T, r *nha180Rig, id string, light *actions.SetLight) *paho.Publish {
	t.Helper()
	response, err := r.router.Dispatch(context.Background(), r.actor, r.request(id, light))
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("dispatch: %+v %v", response, err)
	}
	select {
	case packet := <-r.packets:
		return packet
	case <-time.After(3 * time.Second):
		t.Fatal("command packet absent")
		return nil
	}
}

func nha114Publish(t *testing.T, peer *testbroker.Peer, topic string, payload, correlation []byte, retained bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var properties *paho.PublishProperties
	if correlation != nil {
		properties = &paho.PublishProperties{CorrelationData: append([]byte(nil), correlation...)}
	}
	ack, err := peer.Client.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload, QoS: 1, Retain: retained, Properties: properties})
	if err != nil || ack == nil || ack.ReasonCode >= 0x80 {
		t.Fatalf("synthetic state publication: %+v %v", ack, err)
	}
}

func nha114StateRevision(r *nha180Rig) uint64 {
	state, ok := r.fixture.State(r.entity)
	if !ok {
		return 0
	}
	_, _, _, _, revision, _ := state.Components()
	return revision
}

func nha114WaitStateReceipt(t *testing.T, r *nha180Rig, before uint64) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if nha114StateRevision(r) > before {
			// Observe holds this lock through Host receipt and tracker handling.
			r.mapper.mu.Lock()
			r.mapper.mu.Unlock()
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("MQTT state did not reach Host")
		}
	}
}

func nha114PublishAndWait(t *testing.T, r *nha180Rig, peer *testbroker.Peer, topic string, payload, correlation []byte, retained bool) {
	t.Helper()
	before := nha114StateRevision(r)
	nha114Publish(t, peer, topic, payload, correlation, retained)
	nha114WaitStateReceipt(t, r, before)
}

func nha114CurrentScope(r *nha180Rig) correlationScope {
	r.mapper.mu.Lock()
	defer r.mapper.mu.Unlock()
	return r.mapper.entities[r.entity].correlationScope
}

// Keep state and boolean capability identical while changing the wire profile.
// The real broker and admitted action tracker must reject both replacement and
// A→B→A echoes, while unchanged discovery and fresh commands remain confirmable.
func TestNHA114SwitchEchoFencedByProfileLifetime(t *testing.T) {
	for _, boundary := range []string{"command-topic", "payload", "aba", "unchanged"} {
		t.Run(boundary, func(t *testing.T) {
			r := newNHA180Rig(t, false)
			// Admit a separate switch identity while keeping the real session,
			// transport and action tracker. A tombstoned SourceKey cannot be
			// re-admitted in this Host and must never be bypassed by a fixture.
			key := r.configure("switch-profile-driver", "switch-profile-device")
			ids, err := r.fixture.Resolve(context.Background(), r.instance, []integrations.SourceKey{key})
			if err != nil {
				t.Fatal(err)
			}
			alias, _ := domain.ParseHAAlias("switch.nha114_profile")
			record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, r.instance, key, alias, alias, []domain.HAAlias{alias}, false)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := r.fixture.Sequencer().Capture()
			if err != nil {
				t.Fatal(err)
			}
			records := []integrations.RegistryRecord{record}
			for _, existing := range snapshot.Registry {
				records = append(records, existing)
			}
			if err := r.fixture.PublishRegistry(context.Background(), records); err != nil {
				t.Fatal(err)
			}
			r.entity = ids[0].Entity
			const discoveryTopic = "homeassistant/switch/relay/config"
			const original = `{"unique_id":"switch-profile-driver","device":{"identifiers":["switch-profile-device"]},"state_topic":"zigbee2mqtt/relay/state","command_topic":"zigbee2mqtt/relay/set"}`
			r.send(discoveryTopic, original, ObservationLive)
			r.mapper.mu.Lock()
			runtime := r.mapper.entities[r.entity]
			validIdentity := runtime != nil && runtime.key == key && runtime.profile.kind == componentSwitch
			r.mapper.mu.Unlock()
			if !validIdentity {
				t.Fatal("switch profile not admitted for resolved switch identity")
			}
			peer := r.broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha114-profile-peer", true, 0, nil)
			observer := r.broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha114-profile-observer", true, 0, nil)
			packets := observer.Subscribe(t, "zigbee2mqtt/+/set")
			dispatch := func(id string) *paho.Publish {
				t.Helper()
				request := r.request(id, nil)
				request.CapabilitySchemaId = contracts.SwitchSchema
				request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}
				response, err := r.router.Dispatch(context.Background(), r.actor, request)
				if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
					t.Fatalf("switch dispatch: %+v %v", response, err)
				}
				select {
				case packet := <-packets:
					if packet.Properties == nil || len(packet.Properties.CorrelationData) == 0 {
						t.Fatal("missing switch echo metadata")
					}
					return packet
				case <-time.After(3 * time.Second):
					t.Fatal("switch command packet absent")
					return nil
				}
			}
			oldID := "nha114-profile-old-" + boundary
			first := dispatch(oldID)
			oldScope := nha114CurrentScope(r)
			profile := original
			state := []byte("ON")
			switch boundary {
			case "command-topic", "aba":
				profile = strings.Replace(original, "zigbee2mqtt/relay/set", "zigbee2mqtt/replacement/set", 1)
			case "payload":
				profile = strings.TrimSuffix(original, "}") + `,"payload_on":"YES","payload_off":"NO"}`
				state = []byte("YES")
			case "unchanged":
				// Different document bytes force parsing; canonical profile
				// fingerprint and admitted binding remain the same.
				profile = "  " + original
			}
			r.send(discoveryTopic, profile, ObservationLive)
			if boundary == "aba" {
				middleScope := nha114CurrentScope(r)
				if middleScope == oldScope {
					t.Fatal("replacement retained old profile scope")
				}
				r.send(discoveryTopic, original, ObservationLive)
				if nha114CurrentScope(r) == middleScope {
					t.Fatal("ABA retained intermediate profile scope")
				}
			}
			if same := nha114CurrentScope(r) == oldScope; same != (boundary == "unchanged") {
				t.Fatalf("scope continuity = %v for %s", same, boundary)
			}
			nha114PublishAndWait(t, r, peer, first.Properties.ResponseTopic, state, first.Properties.CorrelationData, false)
			if boundary == "unchanged" {
				r.status(t, oldID, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
			} else {
				r.status(t, oldID, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
			}
			newID := "nha114-profile-new-" + boundary
			second := dispatch(newID)
			if boundary == "command-topic" && second.Topic != "zigbee2mqtt/replacement/set" {
				t.Fatalf("fresh command ignored replacement topic: %s", second.Topic)
			}
			if boundary == "payload" && string(second.Payload) != "YES" {
				t.Fatalf("fresh command ignored replacement payload: %q", second.Payload)
			}
			nha114PublishAndWait(t, r, peer, second.Properties.ResponseTopic, state, second.Properties.CorrelationData, false)
			r.status(t, newID, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
			if boundary != "unchanged" {
				r.status(t, oldID, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
			}
			if got := r.broker.PublishCount(r.settings.clientID); got != 2 {
				t.Fatalf("profile update/echo published %d commands, want 2", got)
			}
		})
	}
}
