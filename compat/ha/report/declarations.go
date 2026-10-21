package report

import (
	"strings"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/contracts"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// Conversion is an explicit documentary configuration assertion. It never
// qualifies the corresponding private Host binding or negotiated worker.
type Conversion uint8

const (
	ConversionUnspecified Conversion = iota
	NativeOnly
	MQTTHA254
	MQTTHA255
	CasetaHALight100
	CasetaHAFan4
)

type Declaration struct {
	id          domain.HAAlias
	description *registryv1.CapabilityDescription
	conversion  Conversion
}

func NewDeclaration(id domain.HAAlias, description *registryv1.CapabilityDescription, conversion Conversion) (Declaration, error) {
	alias, err := id.Value()
	if err != nil || description == nil || conversion <= ConversionUnspecified || conversion > CasetaHAFan4 {
		return Declaration{}, ErrInvalidInput
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil || catalog.AdmitDriverCapability(description) != nil {
		return Declaration{}, ErrInvalidInput
	}
	valid := false
	switch description.SchemaId {
	case contracts.LightSchema:
		valid = strings.HasPrefix(alias, "light.") && (conversion == MQTTHA254 || conversion == MQTTHA255 || conversion == CasetaHALight100 || conversion == NativeOnly)
	case contracts.FanSchema:
		valid = strings.HasPrefix(alias, "fan.") && (conversion == CasetaHAFan4 || conversion == NativeOnly)
	case contracts.SwitchSchema:
		valid = strings.HasPrefix(alias, "switch.") && conversion == NativeOnly
	case contracts.ClimateSchema:
		valid = strings.HasPrefix(alias, "climate.") && conversion == NativeOnly
	default:
		valid = conversion == NativeOnly
	}
	if !valid {
		return Declaration{}, ErrInvalidInput
	}
	key, err := integrations.NewSourceKey("report-declaration", alias)
	if err != nil {
		return Declaration{}, ErrInvalidInput
	}
	candidate, err := integrations.NewCandidate(key, []*registryv1.CapabilityDescription{description})
	if err != nil {
		return Declaration{}, ErrInvalidInput
	}
	switch conversion {
	case CasetaHALight100:
		_, err = candidate.WithCasetaHA100V1(id)
	case CasetaHAFan4:
		_, err = candidate.WithCasetaHAFan4V1(id)
	case MQTTHA254, MQTTHA255:
		scale := uint32(254)
		if conversion == MQTTHA255 {
			scale = 255
		}
		encoded, encodeErr := proto.Marshal(description)
		if encodeErr != nil {
			return Declaration{}, ErrInvalidInput
		}
		encoding, encodingErr := integrations.NewMQTTLightEncoding(scale, [32]byte(nha047.Hash(encoded)), false)
		if encodingErr != nil {
			return Declaration{}, ErrInvalidInput
		}
		_, err = candidate.WithLightEncoding(encoding)
	case NativeOnly:
	case ConversionUnspecified:
		err = ErrInvalidInput
	default:
		err = ErrInvalidInput
	}
	if err != nil {
		return Declaration{}, ErrInvalidInput
	}
	return Declaration{id, proto.Clone(description).(*registryv1.CapabilityDescription), conversion}, nil
}

func (d Declaration) ID() domain.HAAlias { return d.id }
func (d Declaration) Description() *registryv1.CapabilityDescription {
	if d.description == nil {
		return nil
	}
	return proto.Clone(d.description).(*registryv1.CapabilityDescription)
}
func (d Declaration) Conversion() Conversion { return d.conversion }

func admitsBounds(bounds *registryv1.NumberBounds, value float64) bool {
	return contracts.WithinNumberBounds(value, bounds)
}
