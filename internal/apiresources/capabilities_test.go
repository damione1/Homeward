package apiresources

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func ptr[T any](v T) *T { return &v }

func home(t *testing.T) apitypes.HomeName {
	t.Helper()
	value, err := apitypes.ParseHomeName("homes/synthetic")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func entity(t *testing.T, text string) domain.EntityID {
	t.Helper()
	value, err := domain.ParseEntityID(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func observation(t *testing.T, id domain.EntityID, value *domain.StateValue, availability domain.Availability) domain.ObservedState {
	t.Helper()
	q, err := domain.NewQuality(availability, domain.FreshnessStale, domain.ProvenanceRestored)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_800_000_000, 0).UTC()
	source, err := domain.NewSourceStamp("synthetic-observation", ptr(uint64(1)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := domain.NewObservedStateSnapshot(id, q, value, domain.StateTimes{ObservedAt: &at, ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, source, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func byTrait(t *testing.T, items []Capability, expected Trait) *api.Capability {
	t.Helper()
	for _, item := range items {
		if item.Trait() == expected {
			return item.Resource()
		}
	}
	t.Fatalf("missing trait %d", expected)
	return nil
}

func light() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "light", DisplayName: "Synthetic dimmable lamp",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100, Step: ptr(1.0)}}}}
}

func thermostat() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic thermostat",
		Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{
			SetpointCelsius: &registry.NumberBounds{Minimum: 5, Maximum: 35, Step: ptr(0.5)}, ModeOptions: []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT},
			ObservedTemperatureCelsius: &registry.ClimateTemperatureObservation{Bounds: &registry.NumberBounds{Minimum: -50, Maximum: 100}},
			ObservedHumidityPercent:    &registry.NumberBounds{Minimum: 0, Maximum: 100},
			ObservedSetpointCelsius:    &registry.ClimateTemperatureObservation{},
			ObservedModeOptions:        []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT},
			ActivityOptions:            []registry.ClimateActivity{registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE, registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING},
			ObservesFanActivity:        true,
		}}}
}

func TestLampProjectsSimultaneousOffAndZeroWithoutMakingThemUnknown(t *testing.T) {
	id := entity(t, "entity-lamp")
	lightValue, err := domain.NewLightValue(domain.LightValueInput{On: false, Brightness: ptr(0.0), ColorTemperatureMireds: nil})
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewLightStateValue(lightValue)
	if err != nil {
		t.Fatal(err)
	}
	state := observation(t, id, &value, domain.AvailabilityAvailable)
	declaration := light()
	projected, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{declaration}, &state)
	if err != nil || len(projected) != 3 {
		t.Fatal("light did not project both traits", err)
	}
	power := byTrait(t, projected, Power).GetPower()
	level := byTrait(t, projected, Level).GetLevel()
	if power.Observation.Value == nil || *power.Observation.Value != api.PowerValue_POWER_VALUE_OFF || level.Observation.Percentage == nil || *level.Observation.Percentage != 0 {
		t.Fatal("OFF/zero was lost or filled from an absent value")
	}
	if level.Observation.Quality.Freshness != api.Freshness_FRESHNESS_STALE || level.Observation.Quality.Provenance != api.Provenance_PROVENANCE_RESTORED {
		t.Fatal("projection upgraded old restored evidence")
	}
	if !proto.Equal(power.Observation.ObservedTime, level.Observation.ObservedTime) {
		t.Fatal("projection manufactured independent source timestamps")
	}
	declaration.DisplayName = "A different label"
	*declaration.GetLight().BrightnessPercent.Step = 42
	if *byTrait(t, projected, Level).GetLevel().Bounds.Step != 1 {
		t.Fatal("resource aliases mutable input")
	}
	power.Observation.Quality.Provenance = api.Provenance_PROVENANCE_OBSERVED
	if byTrait(t, projected, Power).GetPower().Observation.Quality.Provenance != api.Provenance_PROVENANCE_RESTORED {
		t.Fatal("resource getter exposes mutable authority")
	}
}

func TestThermostatKeepsControlTemperatureHumidityAndFanEvidenceIndependent(t *testing.T) {
	id := entity(t, "entity-thermostat")
	temperature, _ := domain.NewCelsiusTemperature(0)
	humidity, _ := domain.NewRelativeHumidity(0)
	setpointTemperature, _ := domain.NewCelsiusTemperature(20)
	setpoint, _ := domain.NewSingleClimateSetpoint(setpointTemperature)
	climate, err := domain.NewClimateValue(domain.ClimateValueInput{CurrentTemperature: &temperature, Humidity: &humidity, Setpoint: &setpoint, Mode: ptr(domain.ClimateModeOff), Activity: ptr(domain.ClimateActivityIdle), FanActive: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewClimateStateValue(climate)
	if err != nil {
		t.Fatal(err)
	}
	state := observation(t, id, &value, domain.AvailabilityAvailable)
	declaration := thermostat()
	catalogue, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogue.RegisterDriverCapability(id, declaration); err != nil {
		t.Fatal("synthetic declaration was not admitted by the real source catalogue", err)
	}
	if err := catalogue.AdmitDomainClimate(id, declaration.Endpoint, climate); err != nil {
		t.Fatal("synthetic observation bypassed the real source constraints", err)
	}
	projected, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{declaration}, &state)
	if err != nil || len(projected) != 3 {
		t.Fatal("thermostat lost independent traits", err)
	}
	control := byTrait(t, projected, Thermal).GetTemperatureControl()
	temp := byTrait(t, projected, Temperature).GetNumericMeasurement()
	rh := byTrait(t, projected, Humidity).GetNumericMeasurement()
	if control.Observation.Mode == nil || *control.Observation.Mode != api.TemperatureMode_TEMPERATURE_MODE_OFF || control.Observation.Activity == nil || *control.Observation.Activity != api.TemperatureActivity_TEMPERATURE_ACTIVITY_IDLE || control.Observation.FanActive == nil || *control.Observation.FanActive {
		t.Fatal("mode/activity/false fan were conflated")
	}
	if temp.Observation.Value == nil || *temp.Observation.Value != 0 || rh.Observation.Value == nil || *rh.Observation.Value != 0 || temp.Unit != "Cel" || rh.Unit != "%" {
		t.Fatal("zero measurement presence or explicit units lost")
	}
	if control.Observation.FanTarget != nil || temp.Role != api.MeasurementRole_MEASUREMENT_ROLE_TEMPERATURE || rh.Role != api.MeasurementRole_MEASUREMENT_ROLE_HUMIDITY {
		t.Fatal("role inferred from a label or absent fan target filled")
	}
	if !proto.Equal(temp.Observation.Quality, rh.Observation.Quality) || !proto.Equal(temp.Observation.ObservedTime, control.Observation.ObservedTime) {
		t.Fatal("one source observation acquired inconsistent evidence")
	}
}

func TestReadOnlyThermalDeclarationsPreserveUnknownBoundsWithoutGrantingCommands(t *testing.T) {
	declaration := thermostat()
	limits := declaration.GetClimate()
	limits.SetpointCelsius = nil
	limits.ModeOptions = nil
	limits.ObservedSetpointRangeCelsius = &registry.ClimateSetpointRangeConstraints{}
	limits.ObservedTemperatureCelsius.Bounds = nil
	catalogue, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogue.AdmitReadOnlyDriverCapability(declaration); err != nil {
		t.Fatal("read-only fixture is not an admitted source declaration", err)
	}
	projected, err := ProjectCapabilities(home(t), entity(t, "entity-read-only-thermostat"), []*registry.CapabilityDescription{declaration}, nil)
	if err != nil {
		t.Fatal(err)
	}
	thermal := byTrait(t, projected, Thermal).GetTemperatureControl()
	if thermal.SetpointCelsius != nil || thermal.SetpointRangeCelsius != nil || len(thermal.ModeOptions) != 0 || thermal.ObservedSetpointCelsius == nil || thermal.ObservedSetpointCelsius.Bounds != nil || thermal.ObservedSetpointRangeCelsius == nil || thermal.ObservedSetpointRangeCelsius.LowCelsius != nil || thermal.ObservedSetpointRangeCelsius.HighCelsius != nil {
		t.Fatal("read-only observation disappeared, acquired invented bounds or became writable")
	}
	if byTrait(t, projected, Temperature).GetNumericMeasurement().Bounds != nil {
		t.Fatal("unknown thermometer bounds became a zero range")
	}
}

func TestMissingAndUnavailableReadingsNeverBecomeOffOrZero(t *testing.T) {
	id := entity(t, "entity-lamp")
	for _, reading := range []*domain.ObservedState{nil, ptr(observation(t, id, nil, domain.AvailabilityUnavailable))} {
		projected, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{light()}, reading)
		if err != nil {
			t.Fatal(err)
		}
		power := byTrait(t, projected, Power).GetPower()
		level := byTrait(t, projected, Level).GetLevel()
		if reading == nil {
			if power.Observation != nil || level.Observation != nil {
				t.Fatal("no source report became an observation")
			}
		} else if power.Observation.Value != nil || level.Observation.Percentage != nil || power.Observation.Quality.Availability != api.Availability_AVAILABILITY_UNAVAILABLE {
			t.Fatal("unavailable report fabricated an OFF/zero reading")
		}
	}
}

func TestNumericAndBinaryObservationsKeepTypedSemanticsAndZeroPresence(t *testing.T) {
	id := entity(t, "ent_0123456789abcdef0123456789abcdef")
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(0, unit, 2, domain.SemanticMeasurement, domain.AggregationMean)
	if err != nil {
		t.Fatal(err)
	}
	numeric, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	observed := observation(t, id, &numeric, domain.AvailabilityAvailable)
	declaration := &registry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, Endpoint: "watts", DisplayName: "Synthetic watts", Constraints: &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{
		Unit: "W", Precision: 2, Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN,
	}}}
	projected, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{declaration}, &observed)
	if err != nil {
		t.Fatal(err)
	}
	quantity := byTrait(t, projected, Measurement).GetNumericMeasurement()
	if quantity.Observation.Value == nil || *quantity.Observation.Value != 0 || quantity.Unit != "W" || quantity.Precision != 2 || quantity.Aggregation != api.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN || quantity.Role != api.MeasurementRole_MEASUREMENT_ROLE_GENERIC {
		t.Fatal("generic quantity acquired invented units, aggregation or missing zero")
	}
	declaration.GetMeasurement().Unit = "kW"
	if _, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{declaration}, &observed); !errors.Is(err, ErrProjection) {
		t.Fatal("reading was silently relabelled to a different unit", err)
	}
	value := domain.NewSwitchValue(false)
	observed = observation(t, id, &value, domain.AvailabilityAvailable)
	binary := &registry.CapabilityDescription{SchemaId: contracts.BinarySchema, Major: 1, Endpoint: "contact", DisplayName: "Synthetic contact", Constraints: &registry.CapabilityDescription_Binary{Binary: &registry.BinaryConstraints{DeviceClass: ptr("contact")}}}
	projected, err = ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{binary}, &observed)
	if err != nil {
		t.Fatal(err)
	}
	fact := byTrait(t, projected, Binary).GetBinaryObservation()
	if fact.Observation.Value == nil || *fact.Observation.Value || len(projected) != 1 || projected[0].Trait() != Binary {
		t.Fatal("known false became absence or a controllable power trait")
	}
}

func TestSlotIdentityIsStableUnambiguousAndNeverSelectsADuplicateRoute(t *testing.T) {
	id := entity(t, "entity-lamp")
	first, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{light()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := light()
	input.DisplayName = "Arbitrary display label"
	input.GetLight().BrightnessPercent.Maximum = 90
	second, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].Resource().Name != second[i].Resource().Name {
			t.Fatal("value, bound or display label changed identity")
		}
	}
	entityName, _ := EntityName(home(t), id)
	a, _ := authorization.NewCapabilitySlot("a/b", "c")
	b, _ := authorization.NewCapabilitySlot("a", "b/c")
	left, leftErr := capabilityName(entityName, a, Power)
	right, rightErr := capabilityName(entityName, b, Power)
	if leftErr != nil || rightErr != nil || left == right {
		t.Fatal("ambiguous schema/endpoint encoding")
	}
	if _, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{light(), light()}, nil); !errors.Is(err, ErrProjection) {
		t.Fatal("duplicate source silently chose a route", err)
	}
}

func TestPublicEntityNamesPreserveCurrentRegistryAndExtensibleInternalIdentities(t *testing.T) {
	seen := make(map[apitypes.EntityName]bool)
	for _, text := range []string{"ent_0123456789abcdef0123456789abcdef", "ent-0123456789abcdef0123456789abcdef", "synthetic-identity"} {
		id := entity(t, text)
		name, err := EntityName(home(t), id)
		if err != nil || !name.Valid() || name.Home() != home(t) || seen[name] {
			t.Fatal("internal IDs were normalized into an invalid or shared public ID", text, name, err)
		}
		seen[name] = true
		if again, err := EntityName(home(t), id); err != nil || again != name {
			t.Fatal("stable identity acquired a new public name")
		}
		if original, err := id.Value(); err != nil || original != text {
			t.Fatal("mapping modified the durable identity")
		}
	}
}

func TestProjectionRejectsZeroForeignCorruptAndUnsupportedInputs(t *testing.T) {
	id := entity(t, "entity-lamp")
	foreignValue := domain.NewSwitchValue(false)
	foreign := observation(t, entity(t, "entity-other"), &foreignValue, domain.AvailabilityAvailable)
	if _, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{light()}, &foreign); !errors.Is(err, ErrProjection) {
		t.Fatal("foreign observation attached to another identity", err)
	}
	if _, err := ProjectCapabilities(apitypes.HomeName{}, id, nil, nil); !errors.Is(err, ErrProjection) {
		t.Fatal("zero home admitted")
	}
	if _, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{{SchemaId: "future.schema", Endpoint: "future"}}, nil); !errors.Is(err, ErrProjection) {
		t.Fatal("unknown source turned into a default trait", err)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		invalid := light()
		invalid.GetLight().BrightnessPercent.Maximum = value
		if _, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{invalid}, nil); !errors.Is(err, ErrProjection) {
			t.Fatal("invalid bounds projected", value, err)
		}
	}
	invalid := thermostat()
	invalid.GetClimate().ModeOptions = append(invalid.GetClimate().ModeOptions, registry.ClimateMode(999))
	if _, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{invalid}, nil); !errors.Is(err, ErrProjection) {
		t.Fatal("unknown thermal enum projected")
	}
}

func enumCoverage[K ~int32, V ~int32](t *testing.T, descriptor protoreflect.EnumDescriptor, mappings map[K]V) {
	t.Helper()
	values := descriptor.Values()
	if values.Len() != len(mappings)+1 {
		t.Fatal("new enum value requires explicit projection", descriptor.FullName())
	}
	for i := range values.Len() {
		value := values.Get(i)
		_, mapped := mappings[K(value.Number())]
		if mapped != (value.Number() != 0) {
			t.Fatal("enum mapping disagrees with generated authority", value.FullName())
		}
	}
}

func TestNativeEnumProjectionCoversGeneratedVocabularies(t *testing.T) {
	enumCoverage(t, registry.ClimateMode(0).Descriptor(), temperatureModes)
	enumCoverage(t, registry.ClimateActivity(0).Descriptor(), temperatureActivities)
	enumCoverage(t, registry.ClimateFanTarget(0).Descriptor(), temperatureFanTargets)
	enumCoverage(t, registry.MeasurementSemantic(0).Descriptor(), measurementSemantics)
	enumCoverage(t, registry.MeasurementAggregation(0).Descriptor(), measurementAggregations)
	for i := 1; i <= int(Media); i++ {
		if Trait(i).id() == "" {
			t.Fatal("trait has no admitted identity component")
		}
	}
	if Trait(0).id() != "" || Trait(255).id() != "" {
		t.Fatal("zero/unknown trait admitted")
	}
	// Observation quality conversion is deliberate for the source's full enums.
	for _, descriptor := range []protoreflect.EnumDescriptor{state.Availability(0).Descriptor(), state.Freshness(0).Descriptor(), state.Provenance(0).Descriptor()} {
		for i := 1; i < descriptor.Values().Len(); i++ {
			q := &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}
			switch descriptor.FullName() {
			case "home.state.v1.Availability":
				q.Availability = state.Availability(descriptor.Values().Get(i).Number())
			case "home.state.v1.Freshness":
				q.Freshness = state.Freshness(descriptor.Values().Get(i).Number())
			case "home.state.v1.Provenance":
				q.Provenance = state.Provenance(descriptor.Values().Get(i).Number())
			}
			if _, err := quality(q); err != nil {
				t.Fatal("unmapped quality value", descriptor.FullName(), err)
			}
		}
	}
}
