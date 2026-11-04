// Package apiresources projects admitted registry/state values into the current
// public resource contract. It owns no state, permissions, configuration or I/O.
// Callers must supply the authorized, coherent cut from view/sequencer.
package apiresources

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	nativeMedia "github.com/damione1/Homeward/gen/home/media/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

var ErrProjection = errors.New("invalid resource projection")
var ErrUnsupportedTrait = errors.New("source trait has no public projection")

// Trait is an operational discriminator, never inferred from a resource label.
// Go's open integers require validation; a zero binding never grants dispatch.
type Trait uint8

const (
	Power Trait = iota + 1
	Level
	Thermal
	Temperature
	Humidity
	Measurement
	Binary
	Light
	Fan
	Cover
	Lock
	Alarm
	Helper
	Solar
	SolarEvent
	Weather
	Presence
	PresenceDetails
	Opaque
	Media
)

func (t Trait) id() string {
	switch t {
	case Power:
		return "power"
	case Level:
		return "level"
	case Thermal:
		return "thermal"
	case Temperature:
		return "temperature"
	case Humidity:
		return "humidity"
	case Measurement:
		return "measurement"
	case Binary:
		return "binary"
	case Light:
		return "light"
	case Fan:
		return "fan"
	case Cover:
		return "cover"
	case Lock:
		return "lock"
	case Alarm:
		return "alarm"
	case Helper:
		return "helper"
	case Solar:
		return "solar"
	case SolarEvent:
		return "solar-event"
	case Weather:
		return "weather"
	case Presence:
		return "presence"
	case PresenceDetails:
		return "presence-data"
	case Opaque:
		return "opaque"
	case Media:
		return "media"
	default:
		return ""
	}
}

// Capability retains the admitted native source slot alongside its public
// trait. Dispatch must resolve this binding from the current authorized cut;
// a client cannot nominate another schema/endpoint or a provider-specific route.
type Capability struct {
	source           authorization.CapabilitySlot
	trait            Trait
	value            *api.Capability
	actionMessage    string
	nativeMediaRoute *nativeMedia.Route
}

func (c Capability) Source() authorization.CapabilitySlot { return c.source }
func (c Capability) Trait() Trait                         { return c.trait }
func (c Capability) Resource() *api.Capability            { return proto.CloneOf(c.value) }

func EntityName(home apitypes.HomeName, id domain.EntityID) (apitypes.EntityName, error) {
	name, err := apitypes.EntityNameForIdentity(home, id)
	if err != nil {
		return apitypes.EntityName{}, ErrProjection
	}
	return name, nil
}

// The system-assigned slot ID is independent of order, labels and current
// values. Length prefixes keep endpoint/schema boundaries unambiguous. A
// truncated 192-bit digest fits AIP resource IDs; collisions/duplicates within
// an entity are rejected rather than silently selecting a different route.
func capabilityName(entity apitypes.EntityName, slot authorization.CapabilitySlot, trait Trait) (apitypes.CapabilityName, error) {
	schema, endpoint, err := slot.Values()
	if err != nil || !entity.Valid() || trait.id() == "" {
		return apitypes.CapabilityName{}, ErrProjection
	}
	h := sha256.New()
	var size [8]byte
	for _, text := range []string{schema, endpoint} {
		binary.BigEndian.PutUint64(size[:], uint64(len(text)))
		h.Write(size[:])
		h.Write([]byte(text))
	}
	digest := h.Sum(nil)
	return apitypes.ParseCapabilityName(entity.String() + "/capabilities/" + trait.id() + "-" + hex.EncodeToString(digest[:24]))
}

func quality(raw *state.Quality) (*api.ObservationQuality, error) {
	if raw == nil {
		return nil, ErrProjection
	}
	result := &api.ObservationQuality{}
	switch raw.Availability {
	case state.Availability_AVAILABILITY_AVAILABLE:
		result.Availability = api.Availability_AVAILABILITY_AVAILABLE
	case state.Availability_AVAILABILITY_UNKNOWN:
		result.Availability = api.Availability_AVAILABILITY_UNKNOWN
	case state.Availability_AVAILABILITY_UNAVAILABLE:
		result.Availability = api.Availability_AVAILABILITY_UNAVAILABLE
	default:
		return nil, ErrProjection
	}
	switch raw.Freshness {
	case state.Freshness_FRESHNESS_FRESH:
		result.Freshness = api.Freshness_FRESHNESS_CURRENT
	case state.Freshness_FRESHNESS_STALE:
		result.Freshness = api.Freshness_FRESHNESS_STALE
	default:
		return nil, ErrProjection
	}
	switch raw.Provenance {
	case state.Provenance_PROVENANCE_OBSERVED:
		result.Provenance = api.Provenance_PROVENANCE_OBSERVED
	case state.Provenance_PROVENANCE_RESTORED:
		result.Provenance = api.Provenance_PROVENANCE_RESTORED
	case state.Provenance_PROVENANCE_ESTIMATED:
		result.Provenance = api.Provenance_PROVENANCE_ESTIMATED
	default:
		return nil, ErrProjection
	}
	return result, nil
}

func bounds(raw *registry.NumberBounds) (*api.NumberBounds, error) {
	if raw == nil {
		return nil, nil
	}
	if !finite(raw.Minimum) || !finite(raw.Maximum) || raw.Minimum > raw.Maximum ||
		raw.Step != nil && (!finite(*raw.Step) || *raw.Step <= 0) {
		return nil, ErrProjection
	}
	result := &api.NumberBounds{Minimum: raw.Minimum, Maximum: raw.Maximum}
	if raw.Step != nil {
		step := *raw.Step
		result.Step = &step
	}
	return result, nil
}

// ProjectCapabilities is pure and preserves simultaneous observations from a
// composite source frame. No empty/unsupported source silently becomes power.
// The input state is either absent or an admitted value for exactly this entity.
// Only declarations/values already selected by view authorization may enter.
func ProjectCapabilities(home apitypes.HomeName, entityID domain.EntityID, descriptions []*registry.CapabilityDescription, observed *domain.ObservedState) ([]Capability, error) {
	entity, err := EntityName(home, entityID)
	if err != nil {
		return nil, err
	}
	var reading *state.ObservedState
	if observed != nil {
		id, _, _, _, _, err := observed.Components()
		if err != nil || id != entityID {
			return nil, ErrProjection
		}
		reading, err = contracts.EncodeObservedState(*observed)
		if err != nil {
			return nil, ErrProjection
		}
	}
	var result []Capability
	seen := make(map[string]bool)
	slots := make(map[authorization.CapabilitySlot]bool)
	for _, description := range descriptions {
		if description == nil {
			return nil, ErrProjection
		}
		slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
		if err != nil || slots[slot] {
			return nil, ErrProjection
		}
		slots[slot] = true
		projected, err := projectSource(description, reading)
		if err != nil {
			return nil, err
		}
		for _, item := range projected {
			name, err := capabilityName(entity, slot, item.trait)
			if err != nil || seen[name.String()] {
				return nil, ErrProjection
			}
			seen[name.String()] = true
			item.source, item.value.Name = slot, name.String()
			if extension := description.GetExtension(); extension != nil {
				item.actionMessage = extension.GetActionMessage()
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].value.Name < result[j].value.Name })
	return result, nil
}

func powerObservation(raw *state.ObservedState, value *bool) (*api.PowerObservation, error) {
	if raw == nil {
		return nil, nil
	}
	q, err := quality(raw.Quality)
	if err != nil {
		return nil, err
	}
	result := &api.PowerObservation{Quality: q, ObservedTime: proto.CloneOf(raw.ObservedAt)}
	if value != nil {
		power := api.PowerValue_POWER_VALUE_OFF
		if *value {
			power = api.PowerValue_POWER_VALUE_ON
		}
		result.Value = &power
	}
	return result, nil
}

func projectSource(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d == nil {
		return nil, ErrProjection
	}
	switch constraints := d.Constraints.(type) {
	case *registry.CapabilityDescription_Switch:
		if constraints == nil || constraints.Switch == nil {
			return nil, ErrProjection
		}
		var value *bool
		if reading, ok := raw.GetValue().(*state.ObservedState_SwitchOn); ok {
			v := reading.SwitchOn
			value = &v
		}
		observation, err := powerObservation(raw, value)
		if err != nil {
			return nil, err
		}
		return []Capability{{trait: Power, value: &api.Capability{Trait: &api.Capability_Power{Power: &api.PowerCapability{SupportsSet: constraints.Switch.SetOn, Observation: observation}}}}}, nil
	case *registry.CapabilityDescription_Light:
		return projectLight(constraints, raw)
	case *registry.CapabilityDescription_Climate:
		return projectThermal(constraints, raw)
	case *registry.CapabilityDescription_Measurement:
		return projectMeasurement(constraints, raw)
	case *registry.CapabilityDescription_Binary:
		return projectBinary(constraints, raw)
	case *registry.CapabilityDescription_Fan:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectFan(d, raw)
	case *registry.CapabilityDescription_Cover:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectCover(d, raw)
	case *registry.CapabilityDescription_Lock:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectLock(d, raw)
	case *registry.CapabilityDescription_Alarm:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectAlarm(d, raw)
	case *registry.CapabilityDescription_Helper:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectHelper(d, raw)
	case *registry.CapabilityDescription_Solar:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectSolar(d, raw)
	case *registry.CapabilityDescription_SolarEvent:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectSolarEvent(d, raw)
	case *registry.CapabilityDescription_Weather:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectWeather(d, raw)
	case *registry.CapabilityDescription_Presence:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectPresence(d, raw)
	case *registry.CapabilityDescription_PresenceDetails:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectPresenceDetails(d, raw)
	case *registry.CapabilityDescription_Extension:
		if constraints == nil {
			return nil, ErrProjection
		}
		return projectExtension(d, raw)
	case nil:
		return nil, ErrProjection
	default:
		return nil, ErrUnsupportedTrait
	}
}

func projectLight(c *registry.CapabilityDescription_Light, raw *state.ObservedState) ([]Capability, error) {
	if c == nil || c.Light == nil {
		return nil, ErrProjection
	}
	var power *bool
	var level *float64
	if value := raw.GetLight(); value != nil {
		power, level = value.On, value.BrightnessPercent
	}
	observation, err := powerObservation(raw, power)
	if err != nil {
		return nil, err
	}
	result := []Capability{{trait: Power, value: &api.Capability{Trait: &api.Capability_Power{Power: &api.PowerCapability{SupportsSet: c.Light.SetOn, Observation: observation}}}}}
	if c.Light.BrightnessPercent != nil || level != nil {
		b, err := bounds(c.Light.BrightnessPercent)
		if err != nil || b != nil && (b.Minimum < 0 || b.Maximum > 100) {
			return nil, ErrProjection
		}
		var limits *api.PercentageBounds
		if b != nil {
			limits = &api.PercentageBounds{Minimum: b.Minimum, Maximum: b.Maximum, Step: b.Step}
		}
		value := &api.LevelCapability{SupportsSet: b != nil, Bounds: limits}
		if raw != nil {
			q, err := quality(raw.Quality)
			if err != nil {
				return nil, err
			}
			value.Observation = &api.LevelObservation{Percentage: level, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
		}
		result = append(result, Capability{trait: Level, value: &api.Capability{Trait: &api.Capability_Level{Level: value}}})
	}
	composite, err := lightControl(c.Light, raw)
	if err != nil {
		return nil, err
	}
	result = append(result, Capability{trait: Light, value: &api.Capability{Trait: &api.Capability_LightControl{LightControl: composite}}})
	return result, nil
}
