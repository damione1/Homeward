package contracts

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func nha206Descriptor(t *testing.T, d *registry.CapabilityDescription, mutate func(*descriptorpb.FileDescriptorSet)) *registry.CapabilityDescription {
	t.Helper()
	result := proto.Clone(d).(*registry.CapabilityDescription)
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(result.DescriptorSet, set); err != nil {
		t.Fatal(err)
	}
	mutate(set)
	var err error
	result.DescriptorSet, err = proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(result.DescriptorSet)
	result.DescriptorSha256 = hash[:]
	return result
}

func TestNHA206CompilationDescriptorProjectionPreservesLiveAdmission(t *testing.T) {
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	current := testNHA014Extension(t)
	if err := catalog.RegisterExtensionCodec(current, testNHA014ExtensionCodec{}); err != nil {
		t.Fatal(err)
	}
	historical := nha206Descriptor(t, current, func(set *descriptorpb.FileDescriptorSet) {
		set.File[0].Service = append(set.File[0].Service, &descriptorpb.ServiceDescriptorProto{Name: proto.String("RemovedPublicService"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Read"), InputType: proto.String("." + current.GetExtension().GetActionMessage()), OutputType: proto.String("." + current.GetExtension().GetActionMessage())}}})
	})
	original := proto.Clone(historical)
	if bytes.Equal(current.DescriptorSha256, historical.DescriptorSha256) {
		t.Fatal("fixture did not change descriptor packaging")
	}
	for i := 0; i < 2; i++ {
		projected, err := catalog.NormalizeCompilationCapability(historical)
		if err != nil || !proto.Equal(projected, current) {
			t.Fatalf("same action vocabulary not projected: %v", err)
		}
		if !proto.Equal(historical, original) {
			t.Fatal("sealed input changed")
		}
		if !errors.Is(catalog.AdmitDriverCapability(historical), ErrUnknownExtensionCodec) {
			t.Fatal("live admission accepted historical digest")
		}
		if !errors.Is(catalog.RegisterDriverCapability(testNHA014Entity(t), historical), ErrUnknownExtensionCodec) {
			t.Fatal("live advertisement accepted historical digest")
		}
		projected.DescriptorSet[0] ^= 1
	}
	if got, err := catalog.Descriptions(testNHA014Entity(t)); err != nil || len(got) != 0 {
		t.Fatal("projection populated live catalogue", err)
	}
	for name, mutate := range map[string]func(*registry.CapabilityDescription){
		"forged digest":    func(d *registry.CapabilityDescription) { d.DescriptorSha256[0] ^= 1 },
		"unknown schema":   func(d *registry.CapabilityDescription) { d.SchemaId = "vendor.unknown.v1" },
		"unknown endpoint": func(d *registry.CapabilityDescription) { d.Endpoint = "other" },
		"corrupt descriptor": func(d *registry.CapabilityDescription) {
			d.DescriptorSet = []byte{255}
			h := sha256.Sum256(d.DescriptorSet)
			d.DescriptorSha256 = h[:]
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := proto.Clone(historical).(*registry.CapabilityDescription)
			mutate(d)
			if _, err := catalog.NormalizeCompilationCapability(d); err == nil {
				t.Fatal("invalid historical declaration admitted")
			}
		})
	}
	changed := nha206Descriptor(t, historical, func(set *descriptorpb.FileDescriptorSet) {
		set.File[0].MessageType[0].Field[0].Number = proto.Int32(42)
	})
	if _, err := catalog.NormalizeCompilationCapability(changed); err == nil {
		t.Fatal("changed action field admitted")
	}
}

func TestNHA206CompilationChecksTransitiveTypes(t *testing.T) {
	base := testNHA014Extension(t)
	current := nha206Descriptor(t, base, func(set *descriptorpb.FileDescriptorSet) {
		f := set.File[0]
		f.EnumType = append(f.EnumType, &descriptorpb.EnumDescriptorProto{Name: proto.String("Choice"), Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("CHOICE_UNSPECIFIED"), Number: proto.Int32(0)}, {Name: proto.String("CHOICE_ONE"), Number: proto.Int32(1)}}})
		f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("Nested"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("choice"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String("." + f.GetPackage() + ".Choice")}}})
		f.MessageType[0].Field = append(f.MessageType[0].Field, &descriptorpb.FieldDescriptorProto{Name: proto.String("nested"), Number: proto.Int32(20), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String("." + f.GetPackage() + ".Nested")})
	})
	catalog, _ := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err := catalog.RegisterExtensionCodec(current, testNHA014ExtensionCodec{}); err != nil {
		t.Fatal(err)
	}
	otherAction := proto.Clone(current).(*registry.CapabilityDescription)
	var descriptor descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(current.DescriptorSet, &descriptor); err != nil {
		t.Fatal(err)
	}
	otherAction.GetExtension().ActionMessage = proto.String(descriptor.File[0].GetPackage() + ".Nested")
	if _, err := catalog.NormalizeCompilationCapability(otherAction); err == nil {
		t.Fatal("different action identity admitted under exact package digest")
	}
	for name, mutate := range map[string]func(*descriptorpb.FileDescriptorSet){
		"nested field": func(s *descriptorpb.FileDescriptorSet) { s.File[0].MessageType[1].Field[0].Number = proto.Int32(2) },
		"enum number":  func(s *descriptorpb.FileDescriptorSet) { s.File[0].EnumType[0].Value[1].Number = proto.Int32(2) },
		"file options": func(s *descriptorpb.FileDescriptorSet) {
			s.File[0].Options = &descriptorpb.FileOptions{Deprecated: proto.Bool(true)}
		},
		"unknown action field": func(s *descriptorpb.FileDescriptorSet) {
			s.File[0].MessageType[0].Field[0].ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 19000, protowire.VarintType), 1))
		},
		"unknown action": func(s *descriptorpb.FileDescriptorSet) {
			s.File[0].MessageType[0].ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 19000, protowire.VarintType), 1))
		},
		"unknown file": func(s *descriptorpb.FileDescriptorSet) {
			s.File[0].ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 19000, protowire.VarintType), 1))
		},
		"editions": func(s *descriptorpb.FileDescriptorSet) {
			s.File[0].Syntax = proto.String("editions")
			s.File[0].Edition = descriptorpb.Edition_EDITION_2023.Enum()
		},
		"duplicate file": func(s *descriptorpb.FileDescriptorSet) {
			s.File = append(s.File, proto.Clone(s.File[0]).(*descriptorpb.FileDescriptorProto))
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := nha206Descriptor(t, current, mutate)
			if _, err := catalog.NormalizeCompilationCapability(changed); err == nil {
				t.Fatal("different transitive semantics admitted")
			}
		})
	}
}

func TestNHA206CompilationRejectsImportedClosedEnum(t *testing.T) {
	base := testNHA014Extension(t)
	current := nha206Descriptor(t, base, func(s *descriptorpb.FileDescriptorSet) {
		f := s.File[0]
		f.Dependency = append(f.Dependency, "choice.proto")
		s.File = append(s.File, &descriptorpb.FileDescriptorProto{Name: proto.String("choice.proto"), Package: proto.String("synthetic.choice"), Syntax: proto.String("proto3"), EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("Choice"), Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("CHOICE_UNSPECIFIED"), Number: proto.Int32(0)}}}}})
		f.MessageType[0].Field = append(f.MessageType[0].Field, &descriptorpb.FieldDescriptorProto{Name: proto.String("choice"), Number: proto.Int32(20), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".synthetic.choice.Choice")})
	})
	catalog, _ := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err := catalog.RegisterExtensionCodec(current, testNHA014ExtensionCodec{}); err != nil {
		t.Fatal(err)
	}
	changed := nha206Descriptor(t, current, func(s *descriptorpb.FileDescriptorSet) { s.File[len(s.File)-1].Syntax = proto.String("proto2") })
	if _, err := catalog.NormalizeCompilationCapability(changed); err == nil {
		t.Fatal("imported enum openness changed")
	}
}
