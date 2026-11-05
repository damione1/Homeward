package contracts

import (
	"fmt"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// NormalizeCompilationCapability projects a sealed historical compiler input
// onto a currently registered native codec. Removing unrelated RPCs or messages
// from its descriptor file must not invalidate unchanged action vocabulary.
// The original declaration and its persisted bytes/hash remain untouched.
// This does not register a capability, decode an action, or grant Host authority;
// live driver admission and dispatch still require the exact current digest.
func (c *CapabilityCatalog) NormalizeCompilationCapability(d *registry.CapabilityDescription) (*registry.CapabilityDescription, error) {
	if !c.ready() {
		return nil, ErrUninitializedCatalog
	}
	if d == nil {
		return nil, ErrInvalidCapability
	}
	if err := c.validateDescription(d); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	result := proto.Clone(d).(*registry.CapabilityDescription)
	if ext := d.GetExtension(); ext != nil && ext.ActionMessage != nil {
		var source descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(d.DescriptorSet, &source); err != nil || !knownCompilationDescriptor(source.ProtoReflect()) {
			return nil, ErrInvalidCapability
		}
		c.mu.RLock()
		registered, ok := c.extensions[capabilityKey(d.SchemaId, d.Endpoint)]
		c.mu.RUnlock()
		if !ok || d.GetExtension().GetActionMessage() != string(registered.message.FullName()) {
			return nil, ErrUnknownExtensionCodec
		}
		if equalDigest(registered.hash, d.DescriptorSha256) {
			return result, nil
		}
		message, err := extensionMessage(d)
		if err != nil || !sameCompilationAction(message, registered.message) {
			return nil, ErrUnknownExtensionCodec
		}
		result.DescriptorSet = append([]byte(nil), registered.descriptor...)
		result.DescriptorSha256 = append([]byte(nil), registered.hash[:]...)
	}
	if err := c.AdmitDriverCapability(result); err != nil {
		return nil, err
	}
	return result, nil
}

// protodesc reconstructs descriptor messages and drops raw unknown fields.
// Reject these before reflection so future semantics cannot disappear silently.
func knownCompilationDescriptor(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return false
	}
	valid := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if !knownCompilationDescriptor(list.Get(i).Message()) {
					valid = false
					return false
				}
			}
		} else if !knownCompilationDescriptor(value.Message()) {
			valid = false
			return false
		}
		return true
	})
	return valid
}

// Compare the complete transitive action type graph, including options, unknown
// fields, nested declarations, oneofs, enum values, reserved ranges and presence.
// Only unrelated file declarations/services and source locations are excluded.
// Non-exact projection is limited to proto3; inherited Editions features and
// proto2 extension semantics require a separately qualified conversion.
func sameCompilationAction(left, right protoreflect.MessageDescriptor) bool {
	if left == nil || right == nil {
		return false
	}
	type pair struct {
		left, right protoreflect.MessageDescriptor
	}
	pending := []pair{{left, right}}
	seen := map[protoreflect.FullName]bool{}
	for len(pending) > 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		a, b := item.left, item.right
		if a.FullName() != b.FullName() {
			return false
		}
		if seen[a.FullName()] {
			continue
		}
		if len(seen) >= 4096 || a.ParentFile().Syntax() != protoreflect.Proto3 || b.ParentFile().Syntax() != protoreflect.Proto3 ||
			protodesc.ToFileDescriptorProto(a.ParentFile()).GetEdition() != protodesc.ToFileDescriptorProto(b.ParentFile()).GetEdition() ||
			!proto.Equal(a.ParentFile().Options(), b.ParentFile().Options()) ||
			!proto.Equal(protodesc.ToDescriptorProto(a), protodesc.ToDescriptorProto(b)) {
			return false
		}
		seen[a.FullName()] = true
		for i := 0; i < a.Fields().Len(); i++ {
			x, y := a.Fields().Get(i), b.Fields().Get(i)
			if x.Message() != nil {
				if y.Message() == nil {
					return false
				}
				pending = append(pending, pair{x.Message(), y.Message()})
			}
			if x.Enum() != nil {
				if y.Enum() == nil || x.Enum().FullName() != y.Enum().FullName() ||
					x.Enum().IsClosed() != y.Enum().IsClosed() ||
					x.Enum().ParentFile().Syntax() != protoreflect.Proto3 || y.Enum().ParentFile().Syntax() != protoreflect.Proto3 ||
					protodesc.ToFileDescriptorProto(x.Enum().ParentFile()).GetEdition() != protodesc.ToFileDescriptorProto(y.Enum().ParentFile()).GetEdition() ||
					!proto.Equal(x.Enum().ParentFile().Options(), y.Enum().ParentFile().Options()) ||
					!proto.Equal(protodesc.ToEnumDescriptorProto(x.Enum()), protodesc.ToEnumDescriptorProto(y.Enum())) {
					return false
				}
			}
		}
	}
	return true
}
