package integrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	env "github.com/damione1/Homeward/gen/home/environment/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func TestNHF010HostPublishesOnlyTheMatchingReadOnlyEnvironmentVariant(t *testing.T) {
	weather, _ := domain.NewWeatherValue(domain.WeatherParts{Temperature: &domain.Temperature{Value: 0, Unit: domain.TemperatureKelvin}})
	w, _ := domain.NewWeatherStateValue(weather)
	person, _ := domain.ParsePersonID("synthetic-person")
	presence, _ := domain.NewPresenceValue(domain.PresenceParts{Person: person, Residence: domain.ResidenceUnknown, Confidence: domain.PresenceConfidenceUnknown})
	p, _ := domain.NewPresenceStateValue(presence)
	details, _ := domain.NewPresenceDetailsValue(domain.PresenceDetailsParts{Person: person, Coordinates: &domain.Coordinates{}})
	d, _ := domain.NewPresenceDetailsStateValue(details)
	for _, item := range []struct {
		description *registry.CapabilityDescription
		value       domain.StateValue
	}{
		{&registry.CapabilityDescription{SchemaId: contracts.WeatherSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic weather", Constraints: &registry.CapabilityDescription_Weather{Weather: &env.WeatherConstraints{}}}, w},
		{&registry.CapabilityDescription{SchemaId: contracts.PresenceSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic aggregate", Constraints: &registry.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}}, p},
		{&registry.CapabilityDescription{SchemaId: contracts.PresenceDetailsSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic private location", Constraints: &registry.CapabilityDescription_PresenceDetails{PresenceDetails: &env.PresenceDetailsConstraints{}}}, d},
	} {
		t.Run(item.description.SchemaId, func(t *testing.T) {
			driver := &nha025Driver{}
			rig := nha025Setup(t, driver, nil)
			_, key, _ := rig.binding.Source()
			candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{item.description})
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
			if err != nil || len(bindings) != 1 {
				t.Fatal("environment discovery", err)
			}
			rig.binding = bindings[0]
			quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
			publish := func(value domain.StateValue) error {
				at := time.Now().UTC()
				observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
				if err != nil {
					return err
				}
				return rig.session.Publish(context.Background(), rig.binding, observation)
			}
			kind, _ := item.value.Kind()
			before, _ := rig.fixture.Sequencer().Capture()
			for _, foreign := range []domain.StateValue{domain.NewSwitchValue(false), w, p, d} {
				otherKind, _ := foreign.Kind()
				if otherKind == kind {
					continue
				}
				if err := publish(foreign); err == nil {
					t.Fatal("foreign payload admitted by read-only capability")
				}
			}
			after, _ := rig.fixture.Sequencer().Capture()
			if after.Cursor != before.Cursor {
				t.Fatal("rejected observation partially published")
			}
			if err := publish(item.value); err != nil {
				t.Fatal(err)
			}
			_, id, err := rig.binding.IDs()
			if err != nil {
				t.Fatal(err)
			}
			snapshot, _ := rig.fixture.Sequencer().Capture()
			observed, ok := snapshot.States[id]
			if !ok {
				t.Fatal("accepted environment state missing")
			}
			actual, present := observed.CurrentValue()
			actualKind, _ := actual.Kind()
			if !present || actualKind != kind {
				t.Fatal("environment changed state kind")
			}
			rig.request.CapabilitySchemaId = item.description.SchemaId
			response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
			if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || response.Error == nil || response.Error.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY || response.Error.FieldPath != "capability_schema_id" {
				t.Fatalf("read-only environment did not return the typed observation-only refusal: %v %v", response, err)
			}
			if driver.Count() != 0 {
				t.Fatal("read-only report reached command driver")
			}
			if err := rig.session.Withdraw(context.Background(), rig.binding); err != nil {
				t.Fatal(err)
			}
			before, _ = rig.fixture.Sequencer().Capture()
			if err := publish(item.value); err == nil {
				t.Fatal("withdrawn environment binding published again")
			}
			after, _ = rig.fixture.Sequencer().Capture()
			if after.Cursor != before.Cursor {
				t.Fatal("withdrawn report changed the sequencer")
			}
		})
	}
}
