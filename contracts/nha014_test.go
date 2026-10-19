package contracts

import (
	"crypto/sha256"
	"errors"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func testNHA014Entity(t *testing.T) domain.EntityID {
	t.Helper()
	id, err := domain.ParseEntityID("Host:synthetic/lamp")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testNHA014Descriptions(t *testing.T) []*registry.CapabilityDescription {
	t.Helper()
	view := new(state.OpenViewResponse)
	roundtrip(t, "capability-instances", view)
	if len(view.Capabilities) != 6 {
		t.Fatalf("got %d capability families", len(view.Capabilities))
	}
	return view.Capabilities
}

func testNHA014Catalog(t *testing.T) (*CapabilityCatalog, domain.EntityID) {
	t.Helper()
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	entity := testNHA014Entity(t)
	for _, d := range testNHA014Descriptions(t) {
		if err := catalog.RegisterDriverCapability(entity, d); err != nil {
			t.Fatalf("%s: %v", d.SchemaId, err)
		}
	}
	return catalog, entity
}

func testNHA014Command(schema, endpoint string, args *actions.ActionArgs) *actions.CommandRequest {
	return &actions.CommandRequest{
		CommandId: "synthetic", AdmissionEpoch: "epoch-a", ConfigRevision: 1,
		Target:             &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: "Host:synthetic/lamp"}},
		CapabilitySchemaId: schema, Endpoint: endpoint, Arguments: args,
	}
}

func TestNHA014CoreAdmissionAndGolden(t *testing.T) {
	catalog, entity := testNHA014Catalog(t)
	light := new(actions.CommandRequest)
	roundtrip(t, "light-command", light)
	if light.GetArguments().GetSetLight().On == nil || *light.GetArguments().GetSetLight().On ||
		light.GetArguments().GetSetLight().BrightnessPercent == nil || *light.GetArguments().GetSetLight().BrightnessPercent != 0 {
		t.Fatal("explicit false/zero presence lost")
	}
	if err := catalog.AdmitCapabilityCommand(light, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	switchCommand := testNHA014Command(SwitchSchema, "relay", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: false}}})
	if err := catalog.AdmitCapabilityCommand(switchCommand, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	brightness := 101.0
	light.Arguments.GetSetLight().BrightnessPercent = &brightness
	if err := catalog.AdmitCapabilityCommand(light, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || err.FieldPath != "arguments" {
		t.Fatalf("101%% admitted: %v", err)
	}
	brightness = 50.5
	if err := catalog.AdmitCapabilityCommand(light, "epoch-a", 1); err == nil {
		t.Fatal("fractional brightness outside step admitted")
	}
	brightness = 50
	if err := catalog.AdmitCapabilityCommand(light, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	light.Arguments.GetSetLight().On = nil
	light.Arguments.GetSetLight().BrightnessPercent = nil
	if err := catalog.AdmitCapabilityCommand(light, "epoch-a", 1); err == nil {
		t.Fatal("empty light action admitted")
	}

	speed := "high"
	fan := testNHA014Command(FanSchema, "fan", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &speed}}})
	if err := catalog.AdmitCapabilityCommand(fan, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	speed = "turbo"
	if err := catalog.AdmitCapabilityCommand(fan, "epoch-a", 1); err == nil {
		t.Fatal("unsupported fan speed admitted")
	}
	on := false
	fan.Arguments.GetSetFan().Speed = nil
	fan.Arguments.GetSetFan().On = &on
	if err := catalog.AdmitCapabilityCommand(fan, "epoch-a", 1); err == nil {
		t.Fatal("driver did not advertise fan set_on")
	}

	setpoint := 16.5
	climate := testNHA014Command(ClimateSchema, "climate", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint}}})
	if err := catalog.AdmitCapabilityCommand(climate, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	setpoint = 31
	if err := catalog.AdmitCapabilityCommand(climate, "epoch-a", 1); err == nil {
		t.Fatal("setpoint above maximum admitted")
	}
	climate.Arguments.GetSetClimate().SetpointCelsius = nil
	mode := registry.ClimateMode_CLIMATE_MODE_COOL
	climate.Arguments.GetSetClimate().Mode = &mode
	if err := catalog.AdmitCapabilityCommand(climate, "epoch-a", 1); err == nil {
		t.Fatal("unadvertised climate mode admitted")
	}
	mode = registry.ClimateMode_CLIMATE_MODE_HEAT
	if err := catalog.AdmitCapabilityCommand(climate, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}

	for _, schema := range []struct{ id, endpoint string }{{MeasurementSchema, "power"}, {BinarySchema, "contact"}} {
		cmd := testNHA014Command(schema.id, schema.endpoint, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}})
		if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY {
			t.Fatalf("read-only action admitted: %v", err)
		}
	}
	measurement, err := catalog.AdmitMeasurement(entity, "power", &state.Measurement{Value: 100, Unit: "W"})
	if err != nil {
		t.Fatal(err)
	}
	components, err := measurement.Components()
	if err != nil || components.Precision != 1 || components.Semantic != domain.SemanticMeasurement || components.Aggregation != domain.AggregationCurrent {
		t.Fatalf("measurement metadata lost: %+v %v", components, err)
	}
	if _, err := catalog.AdmitMeasurement(entity, "power", &state.Measurement{Value: 2001, Unit: "W"}); err == nil {
		t.Fatal("measurement above range admitted")
	}
	if _, err := catalog.AdmitMeasurement(entity, "power", &state.Measurement{Value: 0, Unit: "kW"}); err == nil {
		t.Fatal("unadvertised source unit admitted")
	}
	decimal := "1.01"
	if _, err := catalog.AdmitMeasurement(entity, "power", &state.Measurement{Unit: "W", DecimalValue: &decimal}); err == nil {
		t.Fatal("exact decimal silently coerced")
	}

	copyOfDescriptions, err := catalog.Descriptions(entity)
	if err != nil || len(copyOfDescriptions) != 6 {
		t.Fatalf("advertised subset: %v %v", len(copyOfDescriptions), err)
	}
	for i := 1; i < len(copyOfDescriptions); i++ {
		if copyOfDescriptions[i-1].SchemaId > copyOfDescriptions[i].SchemaId {
			t.Fatal("advertisement order is unstable")
		}
	}
	for _, d := range copyOfDescriptions {
		if d.SchemaId == BinarySchema && d.GetBinary().DeviceClass != nil {
			t.Fatal("absent binary class became present")
		}
		if d.SchemaId == MeasurementSchema && d.GetMeasurement().Value.Step != nil {
			t.Fatal("absent measurement step became present")
		}
		if d.SchemaId == FanSchema && d.GetFan().SetOn {
			t.Fatal("unadvertised fan action became present")
		}
	}
	for _, d := range copyOfDescriptions {
		if d.SchemaId == LightSchema {
			d.GetLight().BrightnessPercent.Maximum = 101
		}
	}
	brightness = 100
	light.Arguments.GetSetLight().BrightnessPercent = &brightness
	if err := catalog.AdmitCapabilityCommand(light, "epoch-a", 1); err != nil {
		t.Fatal("catalogue mutated through returned description", err)
	}
}

func TestNHA014TypedNilActionBranches(t *testing.T) {
	catalog, _ := testNHA014Catalog(t)
	commands := []*actions.CommandRequest{
		testNHA014Command(SwitchSchema, "relay", &actions.ActionArgs{Arguments: (*actions.ActionArgs_SetSwitch)(nil)}),
		testNHA014Command(LightSchema, "lamp", &actions.ActionArgs{Arguments: (*actions.ActionArgs_SetLight)(nil)}),
		testNHA014Command(FanSchema, "fan", &actions.ActionArgs{Arguments: (*actions.ActionArgs_SetFan)(nil)}),
		testNHA014Command(ClimateSchema, "climate", &actions.ActionArgs{Arguments: (*actions.ActionArgs_SetClimate)(nil)}),
	}
	for _, cmd := range commands {
		if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || err.FieldPath != "arguments" {
			t.Fatalf("typed nil %s: %v", cmd.CapabilitySchemaId, err)
		}
	}
}

func TestNHA014InvalidDeclarations(t *testing.T) {
	if _, err := NewCapabilityCatalog(&domain.UnitCatalog{}); !errors.Is(err, ErrUninitializedCatalog) {
		t.Fatalf("zero unit catalogue: %v", err)
	}
	zeroCatalog := new(CapabilityCatalog)
	if err := zeroCatalog.RegisterDriverCapability(testNHA014Entity(t), testNHA014Descriptions(t)[0]); !errors.Is(err, ErrUninitializedCatalog) {
		t.Fatalf("zero catalogue registration: %v", err)
	}
	if err := zeroCatalog.RegisterExtensionCodec(testNHA014Extension(t), testNHA014ExtensionCodec{}); !errors.Is(err, ErrUninitializedCatalog) {
		t.Fatalf("zero catalogue codec: %v", err)
	}
	if _, err := zeroCatalog.Descriptions(testNHA014Entity(t)); !errors.Is(err, ErrUninitializedCatalog) {
		t.Fatalf("zero catalogue listing: %v", err)
	}
	catalog, entity := testNHA014Catalog(t)
	var zero domain.EntityID
	if err := catalog.RegisterDriverCapability(zero, testNHA014Descriptions(t)[0]); err == nil {
		t.Fatal("zero entity admitted")
	}
	if _, err := catalog.Descriptions(zero); err == nil {
		t.Fatal("zero entity listed")
	}
	for _, mutate := range []func(*registry.CapabilityDescription){
		func(d *registry.CapabilityDescription) { d.Major = 0 },
		func(d *registry.CapabilityDescription) { d.Constraints = nil },
		func(d *registry.CapabilityDescription) { d.GetLight().BrightnessPercent.Maximum = 101 },
		func(d *registry.CapabilityDescription) {
			d.GetLight().BrightnessPercent.Step = nil
			d.GetLight().BrightnessPercent.Minimum = 101
		},
		func(d *registry.CapabilityDescription) { d.DescriptorSha256 = []byte{1} },
	} {
		d := proto.Clone(testNHA014Descriptions(t)[1]).(*registry.CapabilityDescription)
		mutate(d)
		fresh, err := NewCapabilityCatalog(domain.NewUnitCatalog())
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.RegisterDriverCapability(entity, d); !errors.Is(err, ErrInvalidCapability) {
			t.Fatalf("invalid declaration did not yield typed error: %v", err)
		}
	}
	for _, nilBranch := range []struct {
		schema          string
		makeDescription func() *registry.CapabilityDescription
	}{
		{SwitchSchema, func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Switch)(nil)}
		}},
		{LightSchema, func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Light)(nil)}
		}},
		{FanSchema, func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Fan)(nil)}
		}},
		{ClimateSchema, func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Climate)(nil)}
		}},
		{MeasurementSchema, func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Measurement)(nil)}
		}},
		{BinarySchema, func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Binary)(nil)}
		}},
		{"vendor.example.v1", func() *registry.CapabilityDescription {
			return &registry.CapabilityDescription{Constraints: (*registry.CapabilityDescription_Extension)(nil)}
		}},
	} {
		fresh, err := NewCapabilityCatalog(domain.NewUnitCatalog())
		if err != nil {
			t.Fatal(err)
		}
		d := nilBranch.makeDescription()
		d.SchemaId, d.Major, d.DisplayName, d.Endpoint = nilBranch.schema, 1, "Test", "lamp"
		if err := fresh.RegisterDriverCapability(entity, d); !errors.Is(err, ErrInvalidCapability) {
			t.Fatalf("typed nil %s: %v", nilBranch.schema, err)
		}
		if nilBranch.schema == "vendor.example.v1" {
			if err := fresh.RegisterExtensionCodec(d, testNHA014ExtensionCodec{}); !errors.Is(err, ErrInvalidCapability) {
				t.Fatalf("typed nil extension codec: %v", err)
			}
		}
	}
	for _, d := range testNHA014Descriptions(t) {
		if d.SchemaId == FanSchema {
			d.GetFan().SpeedOptions = []string{"low", "low"}
		}
		if d.SchemaId == ClimateSchema {
			d.GetClimate().ModeOptions = append(d.GetClimate().ModeOptions, registry.ClimateMode(999))
		}
		if d.SchemaId == MeasurementSchema {
			d.GetMeasurement().Semantic = registry.MeasurementSemantic(999)
		}
		if d.SchemaId == BinarySchema {
			d.GetBinary().DeviceClass = proto.String("")
		}
		if d.SchemaId == SwitchSchema || d.SchemaId == LightSchema {
			continue
		}
		fresh, err := NewCapabilityCatalog(domain.NewUnitCatalog())
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.RegisterDriverCapability(entity, d); err == nil {
			t.Fatalf("invalid %s declared", d.SchemaId)
		}
	}
}

type testNHA014ExtensionCodec struct{}

func (testNHA014ExtensionCodec) Validate(m proto.Message) error {
	field := m.ProtoReflect().Descriptor().Fields().ByName("level")
	if m.ProtoReflect().Get(field).Int() > 10 {
		return errors.New("level exceeds plugin limit")
	}
	return nil
}

func testNHA014Extension(t *testing.T) *registry.CapabilityDescription {
	t.Helper()
	d := new(registry.CapabilityDescription)
	roundtrip(t, "extension-description", d)
	return d
}

func TestNHA014ExtensionDescriptorAndRejection(t *testing.T) {
	catalog, entity := testNHA014Catalog(t)
	unknown := testNHA014Command("vendor.unknown.v1", "lamp", &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: "vendor.unknown.v1", Major: 1}}})
	if err := catalog.AdmitCapabilityCommand(unknown, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY || err.FieldPath != "capability_schema_id" {
		t.Fatalf("unknown extension not structurally rejected: %v", err)
	}
	d := testNHA014Extension(t)
	bad := proto.Clone(d).(*registry.CapabilityDescription)
	bad.DescriptorSha256[0] ^= 1
	if err := catalog.RegisterDriverCapability(entity, bad); err == nil {
		t.Fatal("bad digest advertised")
	}
	bad = proto.Clone(d).(*registry.CapabilityDescription)
	missing := "vendor.example.v1.NoSuchAction"
	bad.GetExtension().ActionMessage = &missing
	if err := catalog.RegisterDriverCapability(entity, bad); err == nil {
		t.Fatal("missing action descriptor advertised")
	}
	if err := catalog.RegisterDriverCapability(entity, d); !errors.Is(err, ErrUnknownExtensionCodec) {
		t.Fatalf("action advertised before codec registration: %v", err)
	}
	if err := catalog.RegisterExtensionCodec(d, testNHA014ExtensionCodec{}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RegisterDriverCapability(entity, d); err != nil {
		t.Fatal(err)
	}
	typedNil := testNHA014Command(d.SchemaId, d.Endpoint, &actions.ActionArgs{Arguments: (*actions.ActionArgs_Extension)(nil)})
	if err := catalog.AdmitCapabilityCommand(typedNil, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("typed nil extension action: %v", err)
	}
	for _, desc := range mustDescriptions(t, catalog, entity) {
		if desc.SchemaId == d.SchemaId && (!proto.Equal(desc, d) || len(desc.DescriptorSet) == 0) {
			t.Fatal("extension descriptor lost")
		}
	}
	arg := &actions.ExtensionPayload{SchemaId: d.SchemaId, Major: 1, DescriptorSha256: d.DescriptorSha256, ProtobufPayload: []byte{0x08, 0x05}}
	cmd := testNHA014Command(d.SchemaId, d.Endpoint, &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: arg}})
	if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	arg.ProtobufPayload = []byte{0x08, 0x0b}
	if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("plugin semantic limit bypassed: %v", err)
	}
	arg.ProtobufPayload = []byte{0x10, 0x01}
	if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil {
		t.Fatal("unknown extension payload field admitted")
	}
	arg.DescriptorSha256 = make([]byte, sha256.Size)
	if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY {
		t.Fatal("bad action digest admitted")
	}
}

func mustDescriptions(t *testing.T, c *CapabilityCatalog, id domain.EntityID) []*registry.CapabilityDescription {
	t.Helper()
	d, err := c.Descriptions(id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNHA014EnumAndOneofCoverage(t *testing.T) {
	for _, item := range []struct {
		descriptor protoreflect.EnumDescriptor
		expected   int
	}{
		{registry.ClimateMode(0).Descriptor(), 7},
		{registry.ClimateActivity(0).Descriptor(), 6},
		{registry.MeasurementSemantic(0).Descriptor(), 4},
		{registry.MeasurementAggregation(0).Descriptor(), 4},
	} {
		if item.descriptor.Values().Len() != item.expected {
			t.Fatalf("%s changed; review exhaustive admission", item.descriptor.FullName())
		}
	}
	for _, semantic := range []registry.MeasurementSemantic{registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING} {
		if _, err := measurementSemantic(semantic); err != nil {
			t.Fatal(err)
		}
	}
	for _, aggregation := range []registry.MeasurementAggregation{registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT, registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM, registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN} {
		if _, err := measurementAggregation(aggregation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := measurementSemantic(0); err == nil {
		t.Fatal("missing semantic admitted")
	}
	if _, err := measurementAggregation(0); err == nil {
		t.Fatal("missing aggregation admitted")
	}
	fields := (&registry.CapabilityDescription{}).ProtoReflect().Descriptor().Oneofs().ByName("constraints").Fields()
	for _, name := range []protoreflect.Name{"switch", "light", "fan", "climate", "measurement", "binary", "extension", "helper"} {
		if fields.ByName(name) == nil {
			t.Fatalf("missing %s admission branch", name)
		}
	}
	if fields.Len() != 8 || fields.ByName("helper").Number() != 14 {
		t.Fatal("new constraint branch needs explicit admission review")
	}
}
