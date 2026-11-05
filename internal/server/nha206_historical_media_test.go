package server

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/damione1/Homeward/contracts"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sonos"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestNHA206PersistedMediaCompilationWithoutLiveAuthority(t *testing.T) {
	set := new(descriptorpb.FileDescriptorSet)
	seen := map[string]bool{}
	var add func(protoreflect.FileDescriptor)
	add = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		for i := 0; i < f.Imports().Len(); i++ {
			add(f.Imports().Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
	}
	add(media.File_home_media_v1_media_proto)
	// Synthetic retired public surface: native MediaAction itself is unchanged.
	last := set.File[len(set.File)-1]
	last.Service = append(last.Service, &descriptorpb.ServiceDescriptorProto{Name: proto.String("RemovedSyntheticPublicService"), Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Read"), InputType: proto.String(".home.media.v1.MediaAction"), OutputType: proto.String(".home.media.v1.MediaAction")}}})
	descriptor, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(descriptor)
	description := &wire.CapabilityDescription{SchemaId: sonos.Schema, Major: 1, Endpoint: sonos.Endpoint, DisplayName: "Synthetic speaker", DescriptorSet: descriptor, DescriptorSha256: hash[:], Constraints: &wire.CapabilityDescription_Extension{Extension: &wire.ExtensionConstraints{ActionMessage: proto.String("home.media.v1.MediaAction")}}}
	device, _ := domain.ParseDeviceID("dev_synthetic_speaker")
	entity, _ := domain.ParseEntityID("ent_synthetic_speaker")
	instance, _ := domain.ParseIntegrationInstanceID("synthetic_sonos")
	alias, _ := domain.ParseHAAlias("media_player.synthetic_speaker")
	revision, _ := scope.BaselineV1().Value()
	manifest := sha256.Sum256([]byte("synthetic installation manifest"))
	input, err := configcompilation.NewContext(revision, manifest, []configcompilation.Identity{{Device: device, Entity: entity, Instance: instance, SourceDevice: "synthetic-device", SourceEntity: "synthetic-player", Original: alias, Current: alias, Aliases: []domain.HAAlias{alias}}}, map[domain.EntityID][]*wire.CapabilityDescription{entity: {description}})
	if err != nil {
		t.Fatal(err)
	}
	before, seal := input.Bytes(), input.SHA256()
	for i := 0; i < 2; i++ {
		decoded, err := configcompilation.ParseContext(before)
		if err != nil {
			t.Fatal(err)
		}
		units, err := newRunUnitCatalog(true)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := contracts.NewCapabilityCatalog(units)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newNativeSonosRuntime(catalog); err != nil {
			t.Fatal(err)
		}
		stored := decoded.Capabilities()[entity]
		if err := catalog.AdmitDriverCapability(stored[0]); err == nil {
			t.Fatal("historical package accepted for live dispatch")
		}
		supports, err := historicalSourceSemanticSupports(catalog, stored)
		if err != nil {
			t.Fatalf("persisted native compilation failed: %v", err)
		}
		if len(supports) != 0 {
			t.Fatal("native extension supplied new HA execution support")
		}
		if !bytes.Equal(decoded.Bytes(), before) || decoded.SHA256() != seal || !proto.Equal(stored[0], description) {
			t.Fatal("sealed context or detached input mutated")
		}
		if entries, err := catalog.Descriptions(entity); err != nil || len(entries) != 0 {
			t.Fatal("historical compilation granted live authority", err)
		}
	}
	corrupt := append([]byte(nil), before...)
	corrupt[len(corrupt)-1] = '!'
	if _, err := configcompilation.ParseContext(corrupt); err == nil {
		t.Fatal("corrupt context accepted")
	}
}
