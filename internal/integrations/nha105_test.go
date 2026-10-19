package integrations_test

import (
	"context"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func nha105NativeSolar(t testing.TB) domain.SolarValue {
	t.Helper()
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i], _ = domain.SolarOccurrenceAt(time.Unix(int64(i+1)*100, 0))
	}
	input.Next[2], _ = domain.SolarOccurrenceAt(time.Unix(1000, 0))
	value, err := domain.NewSolarValue(input)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNHA105NativeHostRejectsCrossedKindsAndCommands(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	_, key, _ := rig.binding.Source()
	description := &registrywire.CapabilityDescription{SchemaId: contracts.SolarEventSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic dawn", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}
	candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatal(err)
	}
	rig.binding = bindings[0]
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	before, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range domain.SolarEventKinds() {
		event, _ := domain.NewSolarEventValue(kind, missing)
		value, _ := domain.NewSolarEventStateValue(event)
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		at := time.Now().UTC()
		observation, _ := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		err := rig.session.Publish(context.Background(), rig.binding, observation)
		if (err == nil) != (kind == domain.SolarDawn) {
			t.Fatal("Host confused solar event constraints")
		}
	}
	actor, _ := domain.ParseActorID("trusted-actor")
	for i, args := range []*actions.ActionArgs{
		{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}},
		{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{}}},
		{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{}}},
		{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{}}},
		{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{}}},
	} {
		request := proto.Clone(rig.request).(*actions.CommandRequest)
		request.CommandId += string(rune('a' + i))
		request.CapabilitySchemaId = contracts.SolarEventSchema
		request.Arguments = args
		response, err := rig.service.Dispatch(context.Background(), actor, request)
		if err == nil && response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatal("observation became a command")
		}
	}
	if driver.Count() != 0 {
		t.Fatal("solar action reached a driver")
	}
	after, _ := rig.fixture.Sequencer().Capture()
	if after.Cursor == before.Cursor {
		t.Fatal("valid typed-absent observation was not sequenced")
	}
	_, entity, _ := rig.binding.IDs()
	state := after.States[entity]
	value, ok := state.CurrentValue()
	if !ok {
		t.Fatal("astronomical absence became unavailable quality")
	}
	event, ok := value.SolarEvent()
	if !ok {
		t.Fatal("typed absence lost event kind")
	}
	_, occurrence, _ := event.Components()
	_, reason, kind, _ := occurrence.Components()
	if kind != domain.SolarOccurrenceAbsent || reason != domain.SolarNoEventWithinSearchHorizon {
		t.Fatal("absence replaced by invented timestamp")
	}
}
