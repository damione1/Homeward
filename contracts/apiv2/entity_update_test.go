package apiv2

import (
	"errors"
	"testing"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestEntityUpdateMaskPresenceAndReadOnly(t *testing.T) {
	for _, test := range []struct {
		name     string
		mask     *fieldmaskpb.FieldMask
		label    string
		selected bool
	}{
		{"omitted-empty", nil, "", false},
		{"omitted-populated", nil, "Synthetic", true},
		{"present-empty", &fieldmaskpb.FieldMask{}, "Synthetic", false},
		{"explicit-clear", &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}, "", true},
		{"replace-clear", &fieldmaskpb.FieldMask{Paths: []string{"*"}}, "", true},
		{"readonly-ignored", &fieldmaskpb.FieldMask{Paths: []string{"create_time", "update_time", "name", "etag"}}, "Synthetic", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &wire.UpdateEntityRequest{Entity: &wire.Entity{Name: "homes/synthetic/entities/entity-a", Etag: "opaque", DisplayName: test.label, CreateTime: &timestamppb.Timestamp{Seconds: 1 << 62}}, UpdateMask: test.mask}
			update, err := ParseEntityUpdate(r)
			if err != nil {
				t.Fatal(err)
			}
			label, selected := update.Patch().DisplayName()
			if selected != test.selected || selected && label.String() != test.label {
				t.Fatal("field-mask presence changed intent")
			}
		})
	}
}

func TestEntityUpdateRejectsUnknownIntentAndReportsOwnDescriptor(t *testing.T) {
	for _, paths := range [][]string{{"displayName"}, {"state"}, {"*", "display_name"}, {"capabilities.power"}} {
		r := &wire.UpdateEntityRequest{Entity: &wire.Entity{Name: "homes/synthetic/entities/entity-a", Etag: "opaque"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}
		_, err := ParseEntityUpdate(r)
		var violation *Violation
		if !errors.As(err, &violation) || violation.Field() != "update_mask" || violation.Descriptor().FullName() != "home.api.v2.UpdateEntityRequest" {
			t.Fatalf("wrong structured diagnostic: %v", err)
		}
	}
	r := &wire.UpdateEntityRequest{Entity: &wire.Entity{Name: "homes/synthetic/entities/entity-a", Etag: "opaque"}}
	r.Entity.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
	if _, err := ParseEntityUpdate(r); err == nil {
		t.Fatal("unknown mutable entity field silently accepted")
	}
}
