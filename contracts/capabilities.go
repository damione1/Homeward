package contracts

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The schema identifiers are open. These constants name only codecs implemented by
// the MVP core. No device-kind enumeration is implied by this list.
const (
	SwitchSchema      = "core.switch.v1"
	LightSchema       = "core.light.v1"
	FanSchema         = "core.fan.v1"
	ClimateSchema     = "core.climate.v1"
	MeasurementSchema = "core.measurement.v1"
	BinarySchema      = "core.binary.v1"
)

var extensionSchema = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*\.v[1-9][0-9]*$`)

var (
	ErrInvalidCapability     = errors.New("invalid capability declaration")
	ErrUnknownExtensionCodec = errors.New("extension action codec is not registered")
	ErrUninitializedCatalog  = errors.New("capability catalogue is not initialized")
)

// ExtensionCodec supplies plugin-specific semantic validation after the protobuf
// descriptor has decoded an action. A missing codec makes the extension read-only.
type ExtensionCodec interface {
	Validate(proto.Message) error
}

type extensionRegistration struct {
	message protoreflect.MessageDescriptor
	codec   ExtensionCodec
	hash    [sha256.Size]byte
}

// CapabilityCatalog only publishes declarations admitted from a driver. Entries
// and returned descriptions are copied, so callers cannot mutate admitted limits.
// Entity aliases and permission checks belong to later routing tickets.
type CapabilityCatalog struct {
	mu         sync.RWMutex
	units      *domain.UnitCatalog
	instances  map[string]map[string]*registry.CapabilityDescription
	extensions map[string]extensionRegistration
}

func NewCapabilityCatalog(units *domain.UnitCatalog) (*CapabilityCatalog, error) {
	if units == nil {
		return nil, ErrUninitializedCatalog
	}
	if _, err := units.Parse("W"); err != nil {
		return nil, ErrUninitializedCatalog
	}
	return &CapabilityCatalog{units: units, instances: make(map[string]map[string]*registry.CapabilityDescription), extensions: make(map[string]extensionRegistration)}, nil
}

func (c *CapabilityCatalog) ready() bool {
	return c != nil && c.units != nil && c.instances != nil && c.extensions != nil
}

// Ready reports whether the catalog can be injected into a Host boundary.
func (c *CapabilityCatalog) Ready() bool { return c.ready() }

func capabilityKey(schema, endpoint string) string { return schema + "\x00" + endpoint }

// RegisterExtensionCodec binds a namespaced schema and digest to a descriptor
// message and plugin validator. Invalid or unknown descriptors never become a
// command path. The descriptor must be a complete FileDescriptorSet.
func (c *CapabilityCatalog) RegisterExtensionCodec(d *registry.CapabilityDescription, codec ExtensionCodec) error {
	if !c.ready() {
		return ErrUninitializedCatalog
	}
	if codec == nil || d == nil || (reflect.ValueOf(codec).Kind() == reflect.Pointer && reflect.ValueOf(codec).IsNil()) {
		return fmt.Errorf("%w: action extension needs a descriptor and codec", ErrInvalidCapability)
	}
	if err := c.validateDescription(d); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	if d.GetExtension() == nil || d.GetExtension().ActionMessage == nil {
		return fmt.Errorf("%w: action extension needs a descriptor and codec", ErrInvalidCapability)
	}
	message, err := extensionMessage(d)
	if err != nil {
		return err
	}
	var digest [sha256.Size]byte
	copy(digest[:], d.DescriptorSha256)
	key := capabilityKey(d.SchemaId, d.Endpoint)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.extensions[key]; exists {
		return errors.New("extension codec already registered")
	}
	c.extensions[key] = extensionRegistration{message: message, codec: codec, hash: digest}
	return nil
}

// RegisterDriverCapability is the sole advertisement entry point. A driver
// declares only the functions it actually implements, with instance-specific
// limits. The catalogue does not infer capabilities from device names.
func (c *CapabilityCatalog) RegisterDriverCapability(entity domain.EntityID, d *registry.CapabilityDescription) error {
	if !c.ready() {
		return ErrUninitializedCatalog
	}
	if d == nil {
		return fmt.Errorf("%w: description is required", ErrInvalidCapability)
	}
	id, err := entity.Value()
	if err != nil {
		return err
	}
	if err := c.validateDescription(d); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	key := capabilityKey(d.SchemaId, d.Endpoint)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ext := d.GetExtension(); ext != nil && ext.ActionMessage != nil {
		registered, ok := c.extensions[key]
		if !ok || !equalDigest(registered.hash, d.DescriptorSha256) {
			return ErrUnknownExtensionCodec
		}
	}
	if c.instances[id] == nil {
		c.instances[id] = make(map[string]*registry.CapabilityDescription)
	}
	if _, exists := c.instances[id][key]; exists {
		return errors.New("capability instance already registered")
	}
	c.instances[id][key] = proto.Clone(d).(*registry.CapabilityDescription)
	return nil
}

// AdmitDriverCapability checks a declaration against the same unit and
// extension-codec authority used by registration, without mutating the catalog.
// A Host can validate a discovery batch before asking its identity registry to
// bind source keys.
func (c *CapabilityCatalog) AdmitDriverCapability(d *registry.CapabilityDescription) error {
	if !c.ready() {
		return ErrUninitializedCatalog
	}
	if d == nil {
		return fmt.Errorf("%w: description is required", ErrInvalidCapability)
	}
	if err := c.validateDescription(d); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	if ext := d.GetExtension(); ext != nil && ext.ActionMessage != nil {
		key := capabilityKey(d.SchemaId, d.Endpoint)
		c.mu.RLock()
		registered, ok := c.extensions[key]
		c.mu.RUnlock()
		if !ok || !equalDigest(registered.hash, d.DescriptorSha256) {
			return ErrUnknownExtensionCodec
		}
	}
	return nil
}

// DriverCapabilityRegistration is a Host-resolved entity and one admitted
// declaration. A driver cannot supply the EntityID to the Host discovery API.
type DriverCapabilityRegistration struct {
	Entity      domain.EntityID
	Description *registry.CapabilityDescription
}

// ReplaceDriverCapabilitiesBatch atomically replaces all declarations for each
// entity in a discovery batch. Reconnection can refresh advertised limits, while
// a bad later declaration leaves every previous entity unchanged.
func (c *CapabilityCatalog) ReplaceDriverCapabilitiesBatch(registrations []DriverCapabilityRegistration) error {
	if !c.ready() || len(registrations) == 0 {
		return ErrUninitializedCatalog
	}
	staged := make(map[string]map[string]*registry.CapabilityDescription)
	for _, registration := range registrations {
		entity, err := registration.Entity.Value()
		if err != nil {
			return err
		}
		if err := c.AdmitDriverCapability(registration.Description); err != nil {
			return err
		}
		if staged[entity] == nil {
			staged[entity] = make(map[string]*registry.CapabilityDescription)
		}
		key := capabilityKey(registration.Description.SchemaId, registration.Description.Endpoint)
		if _, exists := staged[entity][key]; exists {
			return ErrInvalidCapability
		}
		staged[entity][key] = proto.Clone(registration.Description).(*registry.CapabilityDescription)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, descriptions := range staged {
		for key, description := range descriptions {
			if ext := description.GetExtension(); ext != nil && ext.ActionMessage != nil {
				registered, ok := c.extensions[key]
				if !ok || !equalDigest(registered.hash, description.DescriptorSha256) {
					return ErrUnknownExtensionCodec
				}
			}
		}
	}
	for entity, descriptions := range staged {
		c.instances[entity] = descriptions
	}
	return nil
}

func (c *CapabilityCatalog) Descriptions(entity domain.EntityID) ([]*registry.CapabilityDescription, error) {
	if !c.ready() {
		return nil, ErrUninitializedCatalog
	}
	id, err := entity.Value()
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*registry.CapabilityDescription, 0, len(c.instances[id]))
	for _, d := range c.instances[id] {
		out = append(out, proto.Clone(d).(*registry.CapabilityDescription))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SchemaId != out[j].SchemaId {
			return out[i].SchemaId < out[j].SchemaId
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validBounds(b *registry.NumberBounds) bool {
	return b != nil && finite(b.Minimum) && finite(b.Maximum) && b.Minimum <= b.Maximum &&
		(b.Step == nil || (finite(*b.Step) && *b.Step > 0))
}

func (c *CapabilityCatalog) validateDescription(d *registry.CapabilityDescription) error {
	if d != nil && d.Constraints != nil && reflect.ValueOf(d.Constraints).IsNil() {
		return fmt.Errorf("%w: nil constraints branch", ErrInvalidCapability)
	}
	if c == nil || d == nil || d.Major != 1 || d.Endpoint == "" || strings.TrimSpace(d.Endpoint) != d.Endpoint || d.DisplayName == "" {
		return errors.New("invalid capability identity")
	}
	if d.GetExtension() == nil && (len(d.DescriptorSet) != 0 || len(d.DescriptorSha256) != 0) {
		return errors.New("core capability cannot supply a plugin descriptor")
	}
	switch constraints := d.Constraints.(type) {
	case *registry.CapabilityDescription_Switch:
		if constraints == nil || d.SchemaId != SwitchSchema || constraints.Switch == nil {
			return errors.New("switch schema/constraints mismatch")
		}
	case *registry.CapabilityDescription_Light:
		if constraints == nil || d.SchemaId != LightSchema || constraints.Light == nil {
			return errors.New("light schema/constraints mismatch")
		}
		if b := constraints.Light.BrightnessPercent; b != nil && (!validBounds(b) || b.Minimum < 0 || b.Maximum > 100) {
			return errors.New("invalid brightness bounds")
		}
	case *registry.CapabilityDescription_Fan:
		if constraints == nil || d.SchemaId != FanSchema || constraints.Fan == nil || !uniqueOptions(constraints.Fan.SpeedOptions) {
			return errors.New("invalid fan constraints")
		}
	case *registry.CapabilityDescription_Climate:
		if constraints == nil || d.SchemaId != ClimateSchema || constraints.Climate == nil {
			return errors.New("climate schema/constraints mismatch")
		}
		if b := constraints.Climate.SetpointCelsius; b != nil && !validBounds(b) {
			return errors.New("invalid setpoint bounds")
		}
		if !validModes(constraints.Climate.ModeOptions) || !validActivities(constraints.Climate.ActivityOptions) {
			return errors.New("invalid climate options")
		}
	case *registry.CapabilityDescription_Measurement:
		if constraints == nil || d.SchemaId != MeasurementSchema || constraints.Measurement == nil {
			return errors.New("measurement schema/constraints mismatch")
		}
		m := constraints.Measurement
		if _, err := c.units.Parse(m.Unit); err != nil {
			return err
		}
		if m.Precision > 308 || (m.Value != nil && !validBounds(m.Value)) {
			return errors.New("invalid measurement constraints")
		}
		if _, err := measurementSemantic(m.Semantic); err != nil {
			return err
		}
		if _, err := measurementAggregation(m.Aggregation); err != nil {
			return err
		}
	case *registry.CapabilityDescription_Binary:
		if constraints == nil || d.SchemaId != BinarySchema || constraints.Binary == nil {
			return errors.New("binary schema/constraints mismatch")
		}
		if v := constraints.Binary.DeviceClass; v != nil && (strings.TrimSpace(*v) != *v || *v == "") {
			return errors.New("invalid binary class")
		}
	case *registry.CapabilityDescription_Extension:
		if constraints == nil || constraints.Extension == nil || !extensionSchema.MatchString(d.SchemaId) || strings.HasPrefix(d.SchemaId, "core.") {
			return errors.New("invalid extension schema")
		}
		if _, err := extensionMessage(d); err != nil {
			return err
		}
	default:
		return errors.New("capability constraints are required")
	}
	return nil
}

func uniqueOptions(options []string) bool {
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		if option == "" || strings.TrimSpace(option) != option || seen[option] {
			return false
		}
		seen[option] = true
	}
	return true
}

func validModes(options []registry.ClimateMode) bool {
	seen := map[registry.ClimateMode]bool{}
	for _, option := range options {
		switch option {
		case registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT, registry.ClimateMode_CLIMATE_MODE_COOL, registry.ClimateMode_CLIMATE_MODE_AUTO, registry.ClimateMode_CLIMATE_MODE_DRY, registry.ClimateMode_CLIMATE_MODE_FAN_ONLY:
		default:
			return false
		}
		if seen[option] {
			return false
		}
		seen[option] = true
	}
	return true
}

func validActivities(options []registry.ClimateActivity) bool {
	seen := map[registry.ClimateActivity]bool{}
	for _, option := range options {
		switch option {
		case registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE, registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING, registry.ClimateActivity_CLIMATE_ACTIVITY_COOLING, registry.ClimateActivity_CLIMATE_ACTIVITY_DRYING, registry.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY:
		default:
			return false
		}
		if seen[option] {
			return false
		}
		seen[option] = true
	}
	return true
}

// Explicit wire-to-domain conversion keeps the already delivered NHA-013
// numeric model authoritative for runtime admission. Generated proto enums
// are the transport authority; the descriptor coverage test catches additions.
func measurementSemantic(s registry.MeasurementSemantic) (domain.SemanticClass, error) {
	switch s {
	case registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT:
		return domain.SemanticMeasurement, nil
	case registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL:
		return domain.SemanticTotal, nil
	case registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING:
		return domain.SemanticTotalIncreasing, nil
	case registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_UNSPECIFIED:
		return domain.SemanticUnspecified, errors.New("measurement semantic is required")
	default:
		return domain.SemanticUnspecified, errors.New("unknown measurement semantic")
	}
}

func measurementAggregation(a registry.MeasurementAggregation) (domain.AggregationKind, error) {
	switch a {
	case registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT:
		return domain.AggregationCurrent, nil
	case registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM:
		return domain.AggregationSum, nil
	case registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN:
		return domain.AggregationMean, nil
	case registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_UNSPECIFIED:
		return domain.AggregationUnspecified, errors.New("measurement aggregation is required")
	default:
		return domain.AggregationUnspecified, errors.New("unknown measurement aggregation")
	}
}

// AdmitMeasurement applies the advertised per-instance unit, semantic class,
// precision and bounds to a wire observation before it enters the domain.
func (c *CapabilityCatalog) AdmitMeasurement(entity domain.EntityID, endpoint string, wire *state.Measurement) (domain.Measurement, error) {
	if !c.ready() {
		return domain.Measurement{}, ErrUninitializedCatalog
	}
	id, err := entity.Value()
	if err != nil {
		return domain.Measurement{}, err
	}
	c.mu.RLock()
	d := c.instances[id][capabilityKey(MeasurementSchema, endpoint)]
	c.mu.RUnlock()
	if d == nil || d.GetMeasurement() == nil {
		return domain.Measurement{}, errors.New("measurement capability is not advertised")
	}
	constraints := d.GetMeasurement()
	semantic, _ := measurementSemantic(constraints.Semantic)
	aggregation, _ := measurementAggregation(constraints.Aggregation)
	if wire == nil || wire.Unit != constraints.Unit || (constraints.Value != nil && !withinBounds(wire.Value, constraints.Value)) {
		return domain.Measurement{}, errors.New("measurement is outside advertised instance constraints")
	}
	return AdmitWireMeasurement(wire, c.units, int(constraints.Precision), semantic, aggregation)
}

// AdmitDomainMeasurement checks a driver-produced typed measurement against
// the same per-instance constraints as a wire observation. The comparison also
// rejects mismatched semantic class, aggregation, and decimal precision.
func (c *CapabilityCatalog) AdmitDomainMeasurement(entity domain.EntityID, endpoint string, measurement domain.Measurement) error {
	components, err := measurement.Components()
	if err != nil {
		return err
	}
	unit, err := components.Unit.Symbol()
	if err != nil {
		return err
	}
	admitted, err := c.AdmitMeasurement(entity, endpoint, &state.Measurement{Value: components.Value, Unit: unit})
	if err != nil {
		return err
	}
	if admitted != measurement {
		return ErrInvalidCapability
	}
	return nil
}

func extensionMessage(d *registry.CapabilityDescription) (protoreflect.MessageDescriptor, error) {
	if len(d.DescriptorSet) == 0 || len(d.DescriptorSha256) != sha256.Size {
		return nil, errors.New("extension descriptor and SHA-256 are required")
	}
	digest := sha256.Sum256(d.DescriptorSet)
	if !equalDigest(digest, d.DescriptorSha256) {
		return nil, errors.New("extension descriptor digest mismatch")
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(d.DescriptorSet, &set); err != nil || len(set.File) == 0 {
		return nil, errors.New("invalid extension descriptor set")
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("invalid extension descriptor: %w", err)
	}
	ext := d.GetExtension()
	if ext == nil || ext.ActionMessage == nil {
		return nil, nil
	}
	if *ext.ActionMessage == "" {
		return nil, errors.New("empty extension action message")
	}
	entry, err := files.FindDescriptorByName(protoreflect.FullName(*ext.ActionMessage))
	if err != nil {
		return nil, errors.New("extension action message missing from descriptor")
	}
	message, ok := entry.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, errors.New("extension action descriptor is not a message")
	}
	return message, nil
}

func equalDigest(digest [sha256.Size]byte, actual []byte) bool {
	if len(actual) != sha256.Size {
		return false
	}
	var diff byte
	for i := range digest {
		diff |= digest[i] ^ actual[i]
	}
	return diff == 0
}

// AdmitCapabilityCommand validates a command against the driver's advertised
// instance before any future permission check or dispatch. No physical action
// is performed here. Target aliases require resolution by a later router.
func (c *CapabilityCatalog) AdmitCapabilityCommand(cmd *actions.CommandRequest, epoch string, revision uint64) *actions.ContractError {
	if err := validateEnvelope(cmd, epoch, revision); err != nil {
		return err
	}
	if !c.ready() {
		return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id", "capability catalogue is unavailable")
	}
	entityRef, ok := cmd.Target.Reference.(*registry.EntityRef_EntityId)
	if !ok {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target", "entity alias must be resolved before capability admission")
	}
	entity, err := domain.ParseEntityID(entityRef.EntityId)
	if err != nil {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target", "invalid entity")
	}
	id, _ := entity.Value()
	key := capabilityKey(cmd.CapabilitySchemaId, cmd.Endpoint)
	c.mu.RLock()
	d := c.instances[id][key]
	extension, hasCodec := c.extensions[key]
	c.mu.RUnlock()
	if d == nil {
		return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id", "capability is not advertised by this driver")
	}
	if cmd.Arguments == nil {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments", "action arguments are required")
	}
	if cmd.Arguments.Arguments != nil && reflect.ValueOf(cmd.Arguments.Arguments).IsNil() {
		return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments", "nil action branch")
	}
	switch constraints := d.Constraints.(type) {
	case *registry.CapabilityDescription_Switch:
		if arg := cmd.Arguments.GetSetSwitch(); arg != nil && constraints.Switch.SetOn {
			return nil
		}
	case *registry.CapabilityDescription_Light:
		arg := cmd.Arguments.GetSetLight()
		if arg == nil || (arg.On == nil && arg.BrightnessPercent == nil) {
			break
		}
		if arg.On != nil && !constraints.Light.SetOn {
			break
		}
		if arg.BrightnessPercent != nil && !withinBounds(*arg.BrightnessPercent, constraints.Light.BrightnessPercent) {
			break
		}
		return nil
	case *registry.CapabilityDescription_Fan:
		arg := cmd.Arguments.GetSetFan()
		if arg == nil || (arg.On == nil && arg.Speed == nil) {
			break
		}
		if arg.On != nil && !constraints.Fan.SetOn {
			break
		}
		if arg.Speed != nil && !contains(constraints.Fan.SpeedOptions, *arg.Speed) {
			break
		}
		return nil
	case *registry.CapabilityDescription_Climate:
		arg := cmd.Arguments.GetSetClimate()
		if arg == nil || (arg.SetpointCelsius == nil && arg.Mode == nil) {
			break
		}
		if arg.SetpointCelsius != nil && !withinBounds(*arg.SetpointCelsius, constraints.Climate.SetpointCelsius) {
			break
		}
		if arg.Mode != nil && !contains(constraints.Climate.ModeOptions, *arg.Mode) {
			break
		}
		return nil
	case *registry.CapabilityDescription_Extension:
		arg := cmd.Arguments.GetExtension()
		if arg == nil {
			break
		}
		if !hasCodec || constraints.Extension.ActionMessage == nil {
			return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id", "extension action codec is unknown")
		}
		if arg.SchemaId != d.SchemaId || arg.Major != d.Major || !equalDigest(extension.hash, arg.DescriptorSha256) {
			return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "arguments.extension", "extension action schema or digest differs")
		}
		message := dynamicpb.NewMessage(extension.message)
		if err := proto.Unmarshal(arg.ProtobufPayload, message); err != nil || !knownFieldsOnly(message) {
			return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments.extension.protobuf_payload", "extension payload does not match descriptor")
		}
		if err := extension.codec.Validate(message); err != nil {
			return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments.extension.protobuf_payload", err.Error())
		}
		return nil
	case *registry.CapabilityDescription_Measurement, *registry.CapabilityDescription_Binary:
		return commandError(actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY, "capability_schema_id", "capability is observation only")
	}
	return commandError(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "arguments", "action is outside advertised instance constraints")
}

func knownFieldsOnly(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return false
	}
	valid := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsList() && field.Kind() == protoreflect.MessageKind {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if !knownFieldsOnly(list.Get(i).Message()) {
					valid = false
					return false
				}
			}
		} else if field.IsMap() && field.MapValue().Kind() == protoreflect.MessageKind {
			value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
				valid = knownFieldsOnly(item.Message())
				return valid
			})
		} else if field.Kind() == protoreflect.MessageKind {
			valid = knownFieldsOnly(value.Message())
		}
		return valid
	})
	return valid
}

func contains[T comparable](values []T, wanted T) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func withinBounds(value float64, bounds *registry.NumberBounds) bool {
	if !finite(value) || bounds == nil || value < bounds.Minimum || value > bounds.Maximum {
		return false
	}
	if bounds.Step == nil {
		return true
	}
	steps := (value - bounds.Minimum) / *bounds.Step
	return math.Abs(steps-math.Round(steps)) <= 1e-9
}
