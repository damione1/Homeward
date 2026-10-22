// Package configcompilation owns detached historical inputs to compilation.
// Context is a value codec, never an identity, capability or dispatch authority.
package configcompilation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"sort"
	"unicode/utf8"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	FormatVersion            = 1
	MaxContextBytes          = 8 << 20
	MaxIdentities            = 4096
	MaxAliasesPerIdentity    = 256
	MaxCapabilitiesPerEntity = 32
	MaxCapabilityBytes       = 256 << 10
	MaxIdentityTextBytes     = 256
)

var ErrInvalidContext = errors.New("invalid configuration compilation context")

// Identity is a DTO copied from an admitted registry projection. Opaque Host
// IDs are intentionally not constrained to UUIDs: SQL registry IDs use dev_ and
// ent_ prefixes, and domain IDs remain open identities.
type Identity struct {
	Device       domain.DeviceID
	Entity       domain.EntityID
	Instance     domain.IntegrationInstanceID
	SourceDevice string
	SourceEntity string
	Original     domain.HAAlias
	Current      domain.HAAlias
	Aliases      []domain.HAAlias
	Retired      bool
}

type Context struct {
	valid        bool
	revision     string
	manifest     [32]byte
	identities   []Identity
	capabilities map[domain.EntityID][]*wireregistry.CapabilityDescription
	encoded      []byte
	digest       [32]byte
}

type wireContext struct {
	Version          int                `json:"version"`
	ManifestRevision string             `json:"manifest_revision"`
	ManifestSHA256   string             `json:"manifest_sha256"`
	Identities       []wireIdentity     `json:"identities"`
	Capabilities     []wireCapabilities `json:"capabilities"`
}
type wireIdentity struct {
	Device       string   `json:"device_id"`
	Entity       string   `json:"entity_id"`
	Instance     string   `json:"instance_id"`
	SourceDevice string   `json:"source_device"`
	SourceEntity string   `json:"source_entity"`
	Original     string   `json:"original_alias"`
	Current      string   `json:"current_alias"`
	Aliases      []string `json:"aliases"`
	Retired      *bool    `json:"retired"`
}
type wireCapabilities struct {
	Entity       string   `json:"entity_id"`
	Descriptions [][]byte `json:"descriptions"`
}

// NewContext checks the bounded representation and referential consistency.
// Capability semantics must still be readmitted by the existing capability
// catalogue when compiling. This codec grants no live support or identity.
func NewContext(manifestRevision string, manifestDigest [32]byte, identities []Identity, capabilities map[domain.EntityID][]*wireregistry.CapabilityDescription) (Context, error) {
	if _, err := scope.ParseRevision(manifestRevision); err != nil || manifestDigest == ([32]byte{}) || len(identities) > MaxIdentities || len(capabilities) > MaxIdentities {
		return Context{}, ErrInvalidContext
	}
	c := Context{revision: manifestRevision, manifest: manifestDigest, identities: make([]Identity, 0, len(identities)), capabilities: make(map[domain.EntityID][]*wireregistry.CapabilityDescription, len(capabilities))}
	wire := wireContext{Version: FormatVersion, ManifestRevision: manifestRevision, ManifestSHA256: hex.EncodeToString(manifestDigest[:]), Identities: make([]wireIdentity, 0, len(identities)), Capabilities: make([]wireCapabilities, 0, len(capabilities))}
	entities := make(map[domain.EntityID]bool, len(identities))
	aliases := make(map[domain.HAAlias]struct{})
	type sourceKey struct {
		instance       domain.IntegrationInstanceID
		device, entity string
	}
	type deviceKey struct {
		instance domain.IntegrationInstanceID
		source   string
	}
	sources := make(map[sourceKey]struct{}, len(identities))
	devices := make(map[deviceKey]domain.DeviceID)
	charge := 512
	for _, identity := range identities {
		device, deviceErr := identity.Device.Value()
		entity, entityErr := identity.Entity.Value()
		instance, instanceErr := identity.Instance.Value()
		original, originalErr := identity.Original.Value()
		current, currentErr := identity.Current.Value()
		if deviceErr != nil || entityErr != nil || instanceErr != nil || originalErr != nil || currentErr != nil || !boundedText(device) || !boundedText(entity) || !boundedText(instance) || !boundedText(identity.SourceDevice) || !boundedText(identity.SourceEntity) || len(identity.Aliases) < 1 || len(identity.Aliases) > MaxAliasesPerIdentity {
			return Context{}, ErrInvalidContext
		}
		if _, duplicate := entities[identity.Entity]; duplicate {
			return Context{}, ErrInvalidContext
		}
		entities[identity.Entity] = identity.Retired
		source := sourceKey{identity.Instance, identity.SourceDevice, identity.SourceEntity}
		if _, duplicate := sources[source]; duplicate {
			return Context{}, ErrInvalidContext
		}
		sources[source] = struct{}{}
		deviceSource := deviceKey{identity.Instance, identity.SourceDevice}
		if previous, found := devices[deviceSource]; found && previous != identity.Device {
			return Context{}, ErrInvalidContext
		}
		devices[deviceSource] = identity.Device
		item := wireIdentity{Device: device, Entity: entity, Instance: instance, SourceDevice: identity.SourceDevice, SourceEntity: identity.SourceEntity, Original: original, Current: current, Aliases: make([]string, 0, len(identity.Aliases))}
		retired := identity.Retired
		item.Retired = &retired
		foundOriginal, foundCurrent := false, false
		charge += 256 + 6*(len(device)+len(entity)+len(instance)+len(original)+len(current)+len(identity.SourceDevice)+len(identity.SourceEntity))
		for _, alias := range identity.Aliases {
			label, err := alias.Value()
			if err != nil {
				return Context{}, ErrInvalidContext
			}
			if _, duplicate := aliases[alias]; duplicate {
				return Context{}, ErrInvalidContext
			}
			aliases[alias] = struct{}{}
			foundOriginal = foundOriginal || alias == identity.Original
			foundCurrent = foundCurrent || alias == identity.Current
			charge += 6*len(label) + 8
			if charge > MaxContextBytes {
				return Context{}, ErrInvalidContext
			}
			item.Aliases = append(item.Aliases, label)
		}
		if !foundOriginal || !foundCurrent {
			return Context{}, ErrInvalidContext
		}
		sort.Strings(item.Aliases)
		wire.Identities = append(wire.Identities, item)
	}
	sort.Slice(wire.Identities, func(i, j int) bool { return wire.Identities[i].Entity < wire.Identities[j].Entity })
	for entity, descriptions := range capabilities {
		retired, known := entities[entity]
		if !known || retired || len(descriptions) > MaxCapabilitiesPerEntity {
			return Context{}, ErrInvalidContext
		}
		label, _ := entity.Value()
		item := wireCapabilities{Entity: label, Descriptions: make([][]byte, 0, len(descriptions))}
		type declarationKey struct{ schema, endpoint string }
		seen := make(map[declarationKey]struct{}, len(descriptions))
		ordered := append([]*wireregistry.CapabilityDescription(nil), descriptions...)
		for _, description := range ordered {
			if !validDescriptionRepresentation(description) {
				return Context{}, ErrInvalidContext
			}
			key := declarationKey{description.SchemaId, description.Endpoint}
			if _, duplicate := seen[key]; duplicate {
				return Context{}, ErrInvalidContext
			}
			seen[key] = struct{}{}
		}
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].SchemaId != ordered[j].SchemaId {
				return ordered[i].SchemaId < ordered[j].SchemaId
			}
			return ordered[i].Endpoint < ordered[j].Endpoint
		})
		for _, description := range ordered {
			encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(description)
			if err != nil || len(encoded) == 0 || len(encoded) > MaxCapabilityBytes {
				return Context{}, ErrInvalidContext
			}
			charge += 2*len(encoded) + 64
			if charge > MaxContextBytes {
				return Context{}, ErrInvalidContext
			}
			item.Descriptions = append(item.Descriptions, encoded)
		}
		wire.Capabilities = append(wire.Capabilities, item)
	}
	sort.Slice(wire.Capabilities, func(i, j int) bool { return wire.Capabilities[i].Entity < wire.Capabilities[j].Entity })
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > MaxContextBytes {
		return Context{}, ErrInvalidContext
	}
	// Reconstruct the owned DTOs from the canonical wire, so no caller slice or
	// protobuf remains reachable through this immutable value.
	for _, item := range wire.Identities {
		identity, err := identityFromWire(item)
		if err != nil {
			return Context{}, err
		}
		c.identities = append(c.identities, identity)
	}
	for _, item := range wire.Capabilities {
		entity, _ := domain.ParseEntityID(item.Entity)
		c.capabilities[entity] = make([]*wireregistry.CapabilityDescription, 0, len(item.Descriptions))
		for _, encoded := range item.Descriptions {
			description := new(wireregistry.CapabilityDescription)
			if err := proto.Unmarshal(encoded, description); err != nil {
				return Context{}, ErrInvalidContext
			}
			c.capabilities[entity] = append(c.capabilities[entity], description)
		}
	}
	c.valid, c.encoded, c.digest = true, encoded, sha256.Sum256(encoded)
	return c, nil
}

func ParseContext(raw []byte) (Context, error) {
	if len(raw) == 0 || len(raw) > MaxContextBytes || !utf8.Valid(raw) || !uniqueJSONFields(raw) {
		return Context{}, ErrInvalidContext
	}
	var wire wireContext
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Context{}, ErrInvalidContext
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Context{}, ErrInvalidContext
	}
	if wire.Version != FormatVersion || wire.Identities == nil || wire.Capabilities == nil || len(wire.Identities) > MaxIdentities || len(wire.Capabilities) > MaxIdentities {
		return Context{}, ErrInvalidContext
	}
	manifestBytes, err := hex.DecodeString(wire.ManifestSHA256)
	if err != nil || len(manifestBytes) != 32 || hex.EncodeToString(manifestBytes) != wire.ManifestSHA256 {
		return Context{}, ErrInvalidContext
	}
	var manifest [32]byte
	copy(manifest[:], manifestBytes)
	identities := make([]Identity, 0, len(wire.Identities))
	for _, item := range wire.Identities {
		identity, err := identityFromWire(item)
		if err != nil {
			return Context{}, err
		}
		identities = append(identities, identity)
	}
	capabilities := make(map[domain.EntityID][]*wireregistry.CapabilityDescription, len(wire.Capabilities))
	for _, item := range wire.Capabilities {
		entity, err := domain.ParseEntityID(item.Entity)
		if err != nil || item.Descriptions == nil || len(item.Descriptions) > MaxCapabilitiesPerEntity {
			return Context{}, ErrInvalidContext
		}
		if _, duplicate := capabilities[entity]; duplicate {
			return Context{}, ErrInvalidContext
		}
		capabilities[entity] = make([]*wireregistry.CapabilityDescription, 0, len(item.Descriptions))
		for _, encoded := range item.Descriptions {
			if len(encoded) == 0 || len(encoded) > MaxCapabilityBytes {
				return Context{}, ErrInvalidContext
			}
			description := new(wireregistry.CapabilityDescription)
			if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(encoded, description); err != nil || !validDescriptionRepresentation(description) {
				return Context{}, ErrInvalidContext
			}
			canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(description)
			if err != nil || !bytes.Equal(canonical, encoded) {
				return Context{}, ErrInvalidContext
			}
			capabilities[entity] = append(capabilities[entity], description)
		}
	}
	return NewContext(wire.ManifestRevision, manifest, identities, capabilities)
}

func identityFromWire(item wireIdentity) (Identity, error) {
	var identity Identity
	var err error
	if item.Retired == nil || len(item.Aliases) < 1 || len(item.Aliases) > MaxAliasesPerIdentity {
		return Identity{}, ErrInvalidContext
	}
	if identity.Device, err = domain.ParseDeviceID(item.Device); err != nil {
		return Identity{}, ErrInvalidContext
	}
	if identity.Entity, err = domain.ParseEntityID(item.Entity); err != nil {
		return Identity{}, ErrInvalidContext
	}
	if identity.Instance, err = domain.ParseIntegrationInstanceID(item.Instance); err != nil {
		return Identity{}, ErrInvalidContext
	}
	if identity.Original, err = domain.ParseHAAlias(item.Original); err != nil {
		return Identity{}, ErrInvalidContext
	}
	if identity.Current, err = domain.ParseHAAlias(item.Current); err != nil {
		return Identity{}, ErrInvalidContext
	}
	identity.SourceDevice, identity.SourceEntity, identity.Retired = item.SourceDevice, item.SourceEntity, *item.Retired
	identity.Aliases = make([]domain.HAAlias, 0, len(item.Aliases))
	for _, label := range item.Aliases {
		alias, err := domain.ParseHAAlias(label)
		if err != nil {
			return Identity{}, ErrInvalidContext
		}
		identity.Aliases = append(identity.Aliases, alias)
	}
	return identity, nil
}

func boundedText(value string) bool {
	return value != "" && len(value) <= MaxIdentityTextBytes && utf8.ValidString(value)
}

func validDescriptionRepresentation(description *wireregistry.CapabilityDescription) bool {
	if description == nil || !boundedText(description.SchemaId) || !boundedText(description.Endpoint) || !boundedText(description.DisplayName) || description.Major != 1 {
		return false
	}
	message := description.ProtoReflect()
	constraints := message.Descriptor().Oneofs().ByName("constraints")
	return constraints != nil && message.WhichOneof(constraints) != nil && knownProto(message) && proto.Size(description) <= MaxCapabilityBytes
}

func knownProto(message protoreflect.Message) bool {
	if !message.IsValid() || len(message.GetUnknown()) != 0 {
		return false
	}
	valid := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		check := func(value protoreflect.Value) bool {
			if field.Kind() == protoreflect.MessageKind {
				return knownProto(value.Message())
			}
			if field.Kind() == protoreflect.EnumKind {
				return field.Enum().Values().ByNumber(value.Enum()) != nil
			}
			if field.Kind() == protoreflect.StringKind {
				return utf8.ValidString(value.String())
			}
			if field.Kind() == protoreflect.FloatKind || field.Kind() == protoreflect.DoubleKind {
				return !math.IsNaN(value.Float()) && !math.IsInf(value.Float(), 0)
			}
			return true
		}
		if field.IsMap() {
			valid = false // No map occurs in CapabilityDescription's v1 tree.
		} else if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if !check(list.Get(i)) {
					valid = false
					break
				}
			}
		} else {
			valid = check(value)
		}
		return valid
	})
	return valid
}

func uniqueJSONFields(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// encoding/json otherwise accepts differently cased struct keys. Derive
	// the exact wire spelling from the one struct declaration before decoding.
	names := make(map[string]struct{})
	for _, shape := range []reflect.Type{reflect.TypeOf(wireContext{}), reflect.TypeOf(wireIdentity{}), reflect.TypeOf(wireCapabilities{})} {
		for i := 0; i < shape.NumField(); i++ {
			names[shape.Field(i).Tag.Get("json")] = struct{}{}
		}
	}
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return true
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok {
					return false
				}
				if _, known := names[name]; !known {
					return false
				}
				if _, duplicate := seen[name]; duplicate {
					return false
				}
				seen[name] = struct{}{}
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func (c Context) Valid() bool              { return c.valid }
func (c Context) Bytes() []byte            { return append([]byte(nil), c.encoded...) }
func (c Context) SHA256() [32]byte         { return c.digest }
func (c Context) ManifestRevision() string { return c.revision }
func (c Context) ManifestSHA256() [32]byte { return c.manifest }
func (c Context) Identities() []Identity {
	if !c.valid {
		return nil
	}
	result := make([]Identity, len(c.identities))
	for i, identity := range c.identities {
		identity.Aliases = append([]domain.HAAlias(nil), identity.Aliases...)
		result[i] = identity
	}
	return result
}
func (c Context) Capabilities() map[domain.EntityID][]*wireregistry.CapabilityDescription {
	if !c.valid {
		return nil
	}
	result := make(map[domain.EntityID][]*wireregistry.CapabilityDescription, len(c.capabilities))
	for entity, descriptions := range c.capabilities {
		result[entity] = make([]*wireregistry.CapabilityDescription, 0, len(descriptions))
		for _, description := range descriptions {
			result[entity] = append(result[entity], proto.Clone(description).(*wireregistry.CapabilityDescription))
		}
	}
	return result
}
