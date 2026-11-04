package apiresources

import (
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	environment "github.com/damione1/Homeward/gen/home/environment/v1"
	helpers "github.com/damione1/Homeward/gen/home/helpers/v1"
	nativeMedia "github.com/damione1/Homeward/gen/home/media/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	solar "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"testing"
)

// A future native family cannot disappear silently from the public catalogue.
// Each current family has its specific codec; only registered extensions are opaque.
func TestEveryNativeFamilyHasPublicProjection(t *testing.T) {
	samples := map[protoreflect.Name]*registry.CapabilityDescription{
		"switch":           {Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}}},
		"light":            {Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true}}},
		"fan":              {Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SetOn: true, SpeedOptions: []string{"Off", "Low"}}}},
		"climate":          {Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{}}},
		"measurement":      {Constraints: &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{Unit: "W", Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}},
		"binary":           {Constraints: &registry.CapabilityDescription_Binary{Binary: &registry.BinaryConstraints{}}},
		"helper":           {Constraints: &registry.CapabilityDescription_Helper{Helper: &helpers.HelperConstraints{Kind: helpers.HelperKind_HELPER_KIND_COUNTER}}},
		"solar":            {Constraints: &registry.CapabilityDescription_Solar{Solar: &solar.SolarConstraints{}}},
		"solar_event":      {Constraints: &registry.CapabilityDescription_SolarEvent{SolarEvent: &solar.SolarEventConstraints{Event: solar.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}},
		"cover":            {Constraints: &registry.CapabilityDescription_Cover{Cover: &controls.CoverConstraints{Operations: []controls.CoverOperation{controls.CoverOperation_COVER_OPERATION_STOP}, ObservedStates: []controls.CoverState{controls.CoverState_COVER_STATE_CLOSED}, ObservesPositionPercent: true}}},
		"lock":             {Constraints: &registry.CapabilityDescription_Lock{Lock: &controls.LockConstraints{Operations: []controls.LockOperation{controls.LockOperation_LOCK_OPERATION_LOCK}, ObservedStates: []controls.LockState{controls.LockState_LOCK_STATE_JAMMED}}}},
		"alarm":            {Constraints: &registry.CapabilityDescription_Alarm{Alarm: &controls.AlarmConstraints{Operations: []controls.AlarmOperation{controls.AlarmOperation_ALARM_OPERATION_DISARM}, ObservedStates: []controls.AlarmState{controls.AlarmState_ALARM_STATE_TRIGGERED}}}},
		"weather":          {Constraints: &registry.CapabilityDescription_Weather{Weather: &environment.WeatherConstraints{}}},
		"presence":         {Constraints: &registry.CapabilityDescription_Presence{Presence: &environment.PresenceConstraints{}}},
		"presence_details": {Constraints: &registry.CapabilityDescription_PresenceDetails{PresenceDetails: &environment.PresenceDetailsConstraints{}}},
		"extension":        {SchemaId: "example.extension", Major: 1, DescriptorSha256: make([]byte, 32), Constraints: &registry.CapabilityDescription_Extension{Extension: &registry.ExtensionConstraints{}}},
	}
	fields := (&registry.CapabilityDescription{}).ProtoReflect().Descriptor().Oneofs().ByName("constraints").Fields()
	if fields.Len() != len(samples) {
		t.Fatal("new native family needs explicit public mapping")
	}
	for i := 0; i < fields.Len(); i++ {
		name := fields.Get(i).Name()
		d, ok := samples[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		t.Run(string(name), func(t *testing.T) {
			items, err := projectSource(d, nil)
			if err != nil || len(items) == 0 {
				t.Fatalf("projection %v", err)
			}
			for _, item := range items {
				if name == "extension" {
					if item.value.GetOpaque() == nil {
						t.Fatal("extension should be metadata only")
					}
				} else if item.value.GetOpaque() != nil {
					t.Fatal("known native family became opaque")
				}
			}
		})
	}
}

func TestAtomicIntentBridgePreservesOnePhysicalArgument(t *testing.T) {
	d := light()
	d.GetLight().ColorTempMireds = &registry.NumberBounds{Minimum: 100, Maximum: 500}
	d.GetLight().TransitionSeconds = &registry.NumberBounds{Minimum: 0, Maximum: 5}
	items, err := ProjectCapabilities(home(t), entity(t, "ent_001122"), []*registry.CapabilityDescription{d}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var c Capability
	for _, item := range items {
		if item.Trait() == Light {
			c = item
		}
	}
	on := true
	brightness, _ := apitypes.NewPercentage(0)
	color := int32(300)
	intent, err := apitypes.NewLightCommand(apitypes.LightInput{On: &on, Brightness: &brightness, ColorTemperatureMireds: &color})
	if err != nil {
		t.Fatal(err)
	}
	a, err := NativeArguments(c, intent)
	if err != nil {
		t.Fatal(err)
	}
	lightArgs := a.GetSetLight()
	if lightArgs == nil || lightArgs.On == nil || !*lightArgs.On || lightArgs.BrightnessPercent == nil || *lightArgs.BrightnessPercent != 0 || lightArgs.ColorTempMireds == nil || *lightArgs.ColorTempMireds != 300 {
		t.Fatal("atomic light fields lost")
	}
	source, _, _ := c.Source().Values()
	if source != contracts.LightSchema {
		t.Fatal("source binding changed")
	}
	low, _ := domain.NewCelsiusTemperature(0)
	high, _ := domain.NewCelsiusTemperature(20)
	r, _ := domain.NewRangeClimateSetpoint(low, high)
	mode := domain.ClimateModeAuto
	fan := domain.NewClimateFanTarget(true)
	thermal, _ := apitypes.NewThermalCommand(&r, &mode, &fan)
	slots, err := ProjectCapabilities(home(t), entity(t, "ent_001122"), []*registry.CapabilityDescription{thermostat()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range slots {
		if slot.Trait() == Thermal {
			args, err := NativeArguments(slot, thermal)
			if err != nil {
				t.Fatal(err)
			}
			x := args.GetSetClimate()
			if x == nil || x.HeatThresholdCelsius == nil || *x.HeatThresholdCelsius != 0 || x.CoolThresholdCelsius == nil || x.Mode == nil || x.FanTarget == nil {
				t.Fatal("thermal intention decomposed or lost")
			}
		}
	}
	if _, err := NativeArguments(c, thermal); err == nil {
		t.Fatal("cross-trait command admitted")
	}
}

func TestPublicDescriptorCannotImportPrivateHostVocabulary(t *testing.T) {
	seen := map[string]bool{}
	var walk func(protoreflect.FileDescriptor)
	walk = func(file protoreflect.FileDescriptor) {
		if seen[file.Path()] {
			return
		}
		seen[file.Path()] = true
		if string(file.Package()) != "home.api.v2" && len(file.Package()) >= 5 && string(file.Package())[:5] == "home." {
			t.Fatalf("private descriptor imported: %s", file.Path())
		}
		for i := 0; i < file.Imports().Len(); i++ {
			walk(file.Imports().Get(i).FileDescriptor)
		}
	}
	walk(api.File_home_api_v2_home_proto)
}

func TestNativeEnumConversionRejectsUnknownWithoutCoercion(t *testing.T) {
	if _, err := convertCoverState(controls.CoverState(991)); err == nil {
		t.Fatal("unknown cover state")
	}
	if _, err := convertWeatherCondition(environment.WeatherCondition(991)); err == nil {
		t.Fatal("unknown weather condition")
	}
	if _, err := convertHelperOperation(helpers.HelperOperation(991)); err == nil {
		t.Fatal("unknown helper operation")
	}
	// Even future wire data must not mutate a projected resource after return.
	d := &controls.CoverConstraints{ObservedStates: []controls.CoverState{controls.CoverState_COVER_STATE_CLOSED}}
	v, err := convertCoverConstraints(d)
	if err != nil {
		t.Fatal(err)
	}
	d.ObservedStates[0] = controls.CoverState_COVER_STATE_OPEN
	if v.ObservedStates[0] != api.CoverState_COVER_STATE_CLOSED {
		t.Fatal("mutable source retained")
	}
	_ = proto.CloneOf(v)
}

func TestTypedFamilyValuesPreservePresenceAndPhysicalMeaning(t *testing.T) {
	closed := controls.CoverState_COVER_STATE_CLOSED
	cover, err := convertCoverValue(&controls.CoverValue{State: &closed, PositionPercent: ptr(0.0), Obstructed: ptr(false)})
	if err != nil || cover.PositionPercent == nil || *cover.PositionPercent != 0 || cover.Obstructed == nil || *cover.Obstructed || cover.State == nil || *cover.State != api.CoverState_COVER_STATE_CLOSED {
		t.Fatal("cover zero/false lost", err)
	}
	count, err := convertHelperValue(&helpers.HelperValue{Value: &helpers.HelperValue_Counter{Counter: 0}, HelperRevision: 7})
	if err != nil || count.HelperRevision != 7 {
		t.Fatal(err)
	}
	if x, ok := count.Value.(*api.HelperValue_Counter); !ok || x.Counter != 0 {
		t.Fatal("counter zero lost")
	}
	text, err := convertHelperValue(&helpers.HelperValue{Value: &helpers.HelperValue_Text{Text: ""}})
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := text.Value.(*api.HelperValue_Text); !ok || x.Text != "" {
		t.Fatal("empty text lost")
	}
	event, err := convertSolarOccurrence(&solar.SolarOccurrence{Occurrence: &solar.SolarOccurrence_Absent{Absent: solar.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON}})
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := event.Occurrence.(*api.SolarOccurrence_Absent); !ok || x.Absent != api.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON {
		t.Fatal("computed absence became missing observation")
	}
	weather, err := convertWeatherValue(&environment.WeatherValue{Temperature: &environment.Temperature{Value: ptr(0.0), Unit: environment.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT}, HumidityPercent: ptr(0.0), Wind: &environment.Wind{Speed: ptr(0.0), Unit: environment.SpeedUnit_SPEED_UNIT_KNOTS, BearingDegrees: ptr(0.0)}})
	if err != nil || weather.Temperature.Value == nil || *weather.Temperature.Value != 0 || weather.Temperature.Unit != api.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT || weather.Wind.Unit != api.SpeedUnit_SPEED_UNIT_KNOTS || weather.Wind.Speed == nil || weather.HumidityPercent == nil {
		t.Fatal("weather units/zero lost", err)
	}
	presence, err := convertPresenceValue(&environment.PresenceValue{PersonId: "person-synthetic", Residence: environment.Residence_RESIDENCE_UNKNOWN, Confidence: environment.PresenceConfidence_PRESENCE_CONFIDENCE_UNKNOWN, SourceLabel: "Synthetic source"})
	if err != nil || presence.Residence != api.Residence_RESIDENCE_UNKNOWN || presence.Confidence != api.PresenceConfidence_PRESENCE_CONFIDENCE_UNKNOWN {
		t.Fatal("explicit unknown presence lost", err)
	}
	if presence.ProtoReflect().Descriptor().Fields().ByName("coordinates") != nil {
		t.Fatal("aggregate presence exposes details")
	}
}

func TestLongTraitDiscriminantStillFitsCanonicalResourceID(t *testing.T) {
	d := &registry.CapabilityDescription{SchemaId: contracts.PresenceDetailsSchema, Major: 1, Endpoint: "details", Constraints: &registry.CapabilityDescription_PresenceDetails{PresenceDetails: &environment.PresenceDetailsConstraints{}}}
	items, err := ProjectCapabilities(home(t), entity(t, "ent_001122"), []*registry.CapabilityDescription{d}, nil)
	if err != nil || len(items) != 1 {
		t.Fatal("presence details cannot be named", err)
	}
	if _, err := apitypes.ParseCapabilityName(items[0].Resource().Name); err != nil {
		t.Fatal(err)
	}
}

func TestTypedNilNativeFamilyCannotPanicOrAcquireATrait(t *testing.T) {
	var cover *registry.CapabilityDescription_Cover
	var helper *registry.CapabilityDescription_Helper
	var solarValue *registry.CapabilityDescription_Solar
	var fan *registry.CapabilityDescription_Fan
	for _, d := range []*registry.CapabilityDescription{nil, {Constraints: cover}, {Constraints: helper}, {Constraints: solarValue}, {Constraints: fan}} {
		if _, err := projectSource(d, nil); err == nil {
			t.Fatal("nil native family projected")
		}
	}
}

func TestObservedPowerDoesNotAcquireAnUndeclaredPowerCommand(t *testing.T) {
	d := light()
	d.GetLight().SetOn = false
	items, err := ProjectCapabilities(home(t), entity(t, "ent_001122"), []*registry.CapabilityDescription{d}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Trait() == Power {
			if _, err := NativeArguments(item, apitypes.PowerOff()); err == nil {
				t.Fatal("read-only power acquired dispatch")
			}
		}
		if item.Trait() == Light {
			off := false
			intent, _ := apitypes.NewLightCommand(apitypes.LightInput{On: &off})
			if _, err := NativeArguments(item, intent); err == nil {
				t.Fatal("atomic command bypassed read-only power")
			}
		}
	}
}

func TestKnownMediaCodecRequiresExactDescriptorAndKeepsReadOnlySourcesReadOnly(t *testing.T) {
	fingerprint, err := compiledMediaFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	declaration := func(schema, action string) *registry.CapabilityDescription {
		var message *string
		if action != "" {
			message = &action
		}
		return &registry.CapabilityDescription{SchemaId: schema, Major: 1, Endpoint: "player", DescriptorSha256: append([]byte(nil), fingerprint...), Constraints: &registry.CapabilityDescription_Extension{Extension: &registry.ExtensionConstraints{ActionMessage: message}}}
	}
	project := func(d *registry.CapabilityDescription) Capability {
		items, err := ProjectCapabilities(home(t), entity(t, "ent_001122"), []*registry.CapabilityDescription{d}, nil)
		if err != nil || len(items) != 1 {
			t.Fatal(err)
		}
		return items[0]
	}
	identity := entity(t, "ent_001122")
	publicName, err := EntityName(home(t), identity)
	if err != nil {
		t.Fatal(err)
	}
	nativeRoute := &nativeMedia.Route{EntityId: "ent_001122", CoordinatorEntityId: "ent_001122", Endpoint: "player", TopologyRevision: 1, ConfigRevision: 1, AdmissionEpoch: "11111111111111111111111111111111"}
	writable := &api.MediaPlaybackCapability{DataProvenance: api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC, Definition: &api.MediaPlaybackDefinition{Operations: []api.MediaOperation{api.MediaOperation_MEDIA_OPERATION_PLAY}}, ActionAccess: api.ActionAccess_ACTION_ACCESS_ALLOWED, Observation: &api.MediaPlaybackObservation{Route: &api.MediaRoute{Entity: publicName.String(), Coordinator: publicName.String(), Endpoint: "player", TopologyRevision: 1, ConfigRevision: 1, AdmissionEpoch: nativeRoute.AdmissionEpoch}}}
	original := project(declaration("home.media.v1", "home.media.v1.MediaAction"))
	projected, err := WithMediaPlayback(original, writable, nativeRoute)
	if err != nil || projected.Trait() != Media || projected.Resource().GetMediaPlayback() == nil || projected.Source() != original.Source() {
		t.Fatal("known media codec not projected", err)
	}
	if projected.Resource().Name == original.Resource().Name {
		t.Fatal("typed media retained opaque capability name")
	}
	full, err := projected.RetainedBytes()
	if err != nil {
		t.Fatal(err)
	}
	withoutRoute := projected
	withoutRoute.nativeMediaRoute = nil
	bare, err := withoutRoute.RetainedBytes()
	if err != nil || full <= bare {
		t.Fatal("private route escaped capture charging", err)
	}
	unknown := proto.CloneOf(writable)
	unknown.DataProvenance = api.MediaDataProvenance(991)
	if _, err := WithMediaPlayback(original, unknown, nativeRoute); err == nil {
		t.Fatal("unknown media data provenance admitted")
	}
	writable.Definition.Operations[0] = api.MediaOperation_MEDIA_OPERATION_STOP
	if projected.Resource().GetMediaPlayback().Definition.Operations[0] != api.MediaOperation_MEDIA_OPERATION_PLAY {
		t.Fatal("projected media retained mutable provider value")
	}
	readonly := project(declaration("androidtv.media.v1", ""))
	if _, err := WithMediaPlayback(readonly, writable, nativeRoute); err == nil {
		t.Fatal("read-only native source acquired media actions")
	}
	if _, err := WithMediaPlayback(readonly, &api.MediaPlaybackCapability{DataProvenance: api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC, ActionAccess: api.ActionAccess_ACTION_ACCESS_DENIED}, nil); err != nil {
		t.Fatal("read-only typed observation rejected", err)
	}
	bad := declaration("home.media.v1", "home.media.v1.MediaAction")
	bad.DescriptorSha256[0] ^= 1
	if _, err := WithMediaPlayback(project(bad), writable, nativeRoute); err == nil {
		t.Fatal("mismatched descriptor became actionable")
	}
	if _, err := WithMediaPlayback(project(declaration("example.media.v1", "home.media.v1.MediaAction")), writable, nativeRoute); err == nil {
		t.Fatal("unknown source became media")
	}
}
