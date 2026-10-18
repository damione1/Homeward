package contracts

import (
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

type nha022MutatingCodec struct {
	catalog  *CapabilityCatalog
	entity   domain.EntityID
	original *actions.CommandRequest
}

func (c *nha022MutatingCodec) Validate(proto.Message) error {
	if _, err := c.catalog.Descriptions(c.entity); err != nil {
		return err
	}
	c.original.Arguments.GetExtension().SchemaId = "vendor.mutated.v1"
	return nil
}

func TestNHA022PreparedCapabilityIsImmutableAndFenced(t *testing.T) {
	catalog, entity := testNHA014Catalog(t)
	description := testNHA014Extension(t)
	arg := &actions.ExtensionPayload{SchemaId: description.SchemaId, Major: 1, DescriptorSha256: description.DescriptorSha256, ProtobufPayload: []byte{0x08, 0x05}}
	command := testNHA014Command(description.SchemaId, description.Endpoint, &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: arg}})
	codec := &nha022MutatingCodec{catalog: catalog, entity: entity, original: command}
	if err := catalog.RegisterExtensionCodec(description, codec); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RegisterDriverCapability(entity, description); err != nil {
		t.Fatal(err)
	}
	prepared, contractErr := catalog.PrepareCapabilityCommand(command)
	if contractErr != nil {
		t.Fatal(contractErr)
	}
	called := false
	contractErr, err := catalog.CommitPreparedCapabilityCommand(prepared, "epoch-a", 1, func(copy *registry.CapabilityDescription) error {
		called = true
		copy.SchemaId = "vendor.mutated.v1"
		copy.DescriptorSha256[0] ^= 1
		return nil
	})
	if err != nil || contractErr != nil || !called {
		t.Fatalf("prepared command changed with caller: %v %v", contractErr, err)
	}
	listed, err := catalog.Descriptions(entity)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range listed {
		if d.SchemaId == description.SchemaId {
			found = true
			if !proto.Equal(d, description) {
				t.Fatal("callback mutated authoritative catalogue")
			}
		}
	}
	if !found {
		t.Fatal("extension disappeared")
	}
	switchCommand := testNHA014Command(SwitchSchema, "relay", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}})
	prepared, contractErr = catalog.PrepareCapabilityCommand(switchCommand)
	if contractErr != nil {
		t.Fatal(contractErr)
	}
	var switchDescription *registry.CapabilityDescription
	for _, d := range listed {
		if d.SchemaId == SwitchSchema {
			switchDescription = d
			break
		}
	}
	if switchDescription == nil {
		t.Fatal("switch missing")
	}
	if err := catalog.ReplaceDriverCapabilitiesBatch([]DriverCapabilityRegistration{{Entity: entity, Description: switchDescription}}); err != nil {
		t.Fatal(err)
	}
	called = false
	contractErr, err = catalog.CommitPreparedCapabilityCommand(prepared, "epoch-a", 1, func(*registry.CapabilityDescription) error { called = true; return nil })
	if err != nil || contractErr == nil || called {
		t.Fatalf("stale prepared capability admitted: %v %v", contractErr, err)
	}
}
