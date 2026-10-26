package contracts

import (
	"bytes"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"testing"
)

func TestCommandAdmissionBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		change func(*actions.CommandRequest)
		code   actions.ErrorCode
		path   string
	}{
		{"missing command ID", func(c *actions.CommandRequest) { c.CommandId = "" }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command_id"},
		{"missing epoch", func(c *actions.CommandRequest) { c.AdmissionEpoch = "" }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "admission_epoch"},
		{"missing target", func(c *actions.CommandRequest) { c.Target = nil }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"},
		{"typed nil target", func(c *actions.CommandRequest) { c.Target.Reference = (*registry.EntityRef_EntityId)(nil) }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"},
		{"unknown target branch", func(c *actions.CommandRequest) {
			wire := []byte{0x9a, 0x06, 0}
			c.Target = new(registry.EntityRef)
			if err := proto.Unmarshal(wire, c.Target); err != nil {
				t.Fatal(err)
			}
			if c.Target.Reference != nil || len(c.Target.ProtoReflect().GetUnknown()) == 0 {
				t.Fatal("future target branch is no longer unknown")
			}
			encoded, err := proto.Marshal(c.Target)
			if err != nil || !bytes.Equal(encoded, wire) {
				t.Fatalf("unknown target branch did not round-trip: %v", err)
			}
		}, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"},
		{"invalid UTF8 identity", func(c *actions.CommandRequest) {
			c.Target.Reference = &registry.EntityRef_EntityId{EntityId: string([]byte{0xff})}
		}, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"},
		{"missing args", func(c *actions.CommandRequest) { c.Arguments = nil }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments"},
		{"absent args branch", func(c *actions.CommandRequest) { c.Arguments = &actions.ActionArgs{} }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments"},
		{"typed nil args", func(c *actions.CommandRequest) { c.Arguments.Arguments = (*actions.ActionArgs_SetSwitch)(nil) }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments"},
		{"nil switch payload", func(c *actions.CommandRequest) { c.Arguments.Arguments = &actions.ActionArgs_SetSwitch{} }, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments"},
		{"extension for switch", func(c *actions.CommandRequest) {
			c.Arguments.Arguments = &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{}}
		}, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments"},
		{"unknown args branch", func(c *actions.CommandRequest) {
			wire := []byte{0x9a, 0x06, 0}
			c.Arguments = &actions.ActionArgs{}
			if err := proto.Unmarshal(wire, c.Arguments); err != nil {
				t.Fatal(err)
			}
			if c.Arguments.Arguments != nil || len(c.Arguments.ProtoReflect().GetUnknown()) == 0 {
				t.Fatal("future arguments branch is no longer unknown")
			}
			encoded, err := proto.Marshal(c.Arguments)
			if err != nil || !bytes.Equal(encoded, wire) {
				t.Fatalf("unknown arguments branch did not round-trip: %v", err)
			}
		}, actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments"},
		{"missing schema", func(c *actions.CommandRequest) { c.CapabilitySchemaId = "" }, actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := new(actions.CommandRequest)
			if err := proto.Unmarshal(golden(t, "command"), c); err != nil {
				t.Fatal(err)
			}
			tt.change(c)
			err := ValidateCommand(c, "epoch-a", c.ConfigRevision)
			if err == nil || err.Code != tt.code || err.FieldPath != tt.path {
				t.Fatalf("rejection = %v", err)
			}
		})
	}
	c := new(actions.CommandRequest)
	if err := proto.Unmarshal(golden(t, "command"), c); err != nil {
		t.Fatal(err)
	}
	c.AdmissionEpoch = ""
	if err := ValidateCommand(c, "", c.ConfigRevision); err == nil || err.FieldPath != "admission_epoch" {
		t.Fatal("two missing epochs admitted")
	}
	if err := ValidateCommand(nil, "epoch-a", 0); err == nil || err.FieldPath != "command_id" {
		t.Fatal("nil command admitted")
	}
	for _, ref := range []*registry.EntityRef{{Reference: &registry.EntityRef_EntityId{EntityId: "Host:opaque/É-42"}}, {Reference: &registry.EntityRef_HaAlias{HaAlias: "switch.synthetic"}}} {
		c.Target = ref
		c.AdmissionEpoch = "epoch-a"
		c.ConfigRevision = 0
		if err := ValidateCommand(c, "epoch-a", 0); err != nil {
			t.Fatalf("valid reference/explicit zero revision/switch off rejected: %v", err)
		}
	}
}

// Go type switches do not prove exhaustiveness. Tie the handwritten admission
// cases to generated descriptors: an added wire branch must get an explicit review.
func TestAdmissionOneofCoverage(t *testing.T) {
	tests := []struct {
		message proto.Message
		oneof   protoreflect.Name
		names   []protoreflect.Name
	}{
		{&registry.EntityRef{}, "reference", []protoreflect.Name{"entity_id", "ha_alias"}},
		{&actions.ActionArgs{}, "arguments", []protoreflect.Name{"set_switch", "extension", "set_light", "set_fan", "set_climate", "helper", "cover", "lock", "alarm"}},
	}
	for _, tt := range tests {
		fields := tt.message.ProtoReflect().Descriptor().Oneofs().ByName(tt.oneof).Fields()
		if fields.Len() != len(tt.names) {
			t.Fatalf("%s: admission must explicitly handle new variants", tt.oneof)
		}
		for _, name := range tt.names {
			if fields.ByName(name) == nil {
				t.Fatalf("%s: variant %s changed", tt.oneof, name)
			}
		}
	}
}
