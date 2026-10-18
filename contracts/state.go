package contracts

import (
	"errors"
	"fmt"
	"time"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrInvalidWireState = errors.New("invalid wire state")

// StateCapabilityAdmission supplies the capability context missing from state
// wire values carrying per-instance measurement or light constraints.
type StateCapabilityAdmission struct {
	Catalog  *CapabilityCatalog
	Endpoint string
}

// StateMeasurementAdmission remains the previous source-compatible spelling.
type StateMeasurementAdmission = StateCapabilityAdmission

func admitQuality(wire *state.Quality) (domain.Quality, error) {
	if wire == nil || len(wire.ProtoReflect().GetUnknown()) != 0 {
		return domain.Quality{}, ErrInvalidWireState
	}
	var availability domain.Availability
	switch wire.Availability {
	case state.Availability_AVAILABILITY_AVAILABLE:
		availability = domain.AvailabilityAvailable
	case state.Availability_AVAILABILITY_UNKNOWN:
		availability = domain.AvailabilityUnknown
	case state.Availability_AVAILABILITY_UNAVAILABLE:
		availability = domain.AvailabilityUnavailable
	case state.Availability_AVAILABILITY_UNSPECIFIED:
		return domain.Quality{}, ErrInvalidWireState
	default:
		return domain.Quality{}, ErrInvalidWireState
	}
	var freshness domain.Freshness
	switch wire.Freshness {
	case state.Freshness_FRESHNESS_FRESH:
		freshness = domain.FreshnessFresh
	case state.Freshness_FRESHNESS_STALE:
		freshness = domain.FreshnessStale
	case state.Freshness_FRESHNESS_UNSPECIFIED:
		return domain.Quality{}, ErrInvalidWireState
	default:
		return domain.Quality{}, ErrInvalidWireState
	}
	var provenance domain.Provenance
	switch wire.Provenance {
	case state.Provenance_PROVENANCE_OBSERVED:
		provenance = domain.ProvenanceObserved
	case state.Provenance_PROVENANCE_RESTORED:
		provenance = domain.ProvenanceRestored
	case state.Provenance_PROVENANCE_ESTIMATED:
		provenance = domain.ProvenanceEstimated
	case state.Provenance_PROVENANCE_UNSPECIFIED:
		return domain.Quality{}, ErrInvalidWireState
	default:
		return domain.Quality{}, ErrInvalidWireState
	}
	return domain.NewQuality(availability, freshness, provenance)
}

func wireQuality(quality domain.Quality) (*state.Quality, error) {
	a, f, p, err := quality.Components()
	if err != nil {
		return nil, err
	}
	out := &state.Quality{}
	switch a {
	case domain.AvailabilityAvailable:
		out.Availability = state.Availability_AVAILABILITY_AVAILABLE
	case domain.AvailabilityUnknown:
		out.Availability = state.Availability_AVAILABILITY_UNKNOWN
	case domain.AvailabilityUnavailable:
		out.Availability = state.Availability_AVAILABILITY_UNAVAILABLE
	case domain.AvailabilityUnspecified:
		return nil, ErrInvalidWireState
	default:
		return nil, ErrInvalidWireState
	}
	switch f {
	case domain.FreshnessFresh:
		out.Freshness = state.Freshness_FRESHNESS_FRESH
	case domain.FreshnessStale:
		out.Freshness = state.Freshness_FRESHNESS_STALE
	case domain.FreshnessUnspecified:
		return nil, ErrInvalidWireState
	default:
		return nil, ErrInvalidWireState
	}
	switch p {
	case domain.ProvenanceObserved:
		out.Provenance = state.Provenance_PROVENANCE_OBSERVED
	case domain.ProvenanceRestored:
		out.Provenance = state.Provenance_PROVENANCE_RESTORED
	case domain.ProvenanceEstimated:
		out.Provenance = state.Provenance_PROVENANCE_ESTIMATED
	case domain.ProvenanceUnspecified:
		return nil, ErrInvalidWireState
	default:
		return nil, ErrInvalidWireState
	}
	return out, nil
}

func admitTimestamp(at *timestamppb.Timestamp, required bool) (*time.Time, error) {
	if at == nil {
		if required {
			return nil, ErrInvalidWireState
		}
		return nil, nil
	}
	if err := at.CheckValid(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWireState, err)
	}
	value := at.AsTime().UTC()
	if value.IsZero() {
		return nil, ErrInvalidWireState
	}
	return &value, nil
}

// AdmitWireState accepts a host state snapshot with authoritative transition
// clocks. Snapshots can skip intermediate reports, so no transition is inferred
// from the optional previous snapshot. Unknown wire fields remain decodable by
// generated bindings but are not admitted to the operational domain.
func AdmitWireState(wire *state.ObservedState, previous *domain.ObservedState, measurement *StateCapabilityAdmission) (domain.ObservedState, error) {
	if wire == nil || len(wire.ProtoReflect().GetUnknown()) != 0 || wire.Entity == nil {
		return domain.ObservedState{}, ErrInvalidWireState
	}
	ref, ok := wire.Entity.Reference.(*registry.EntityRef_EntityId)
	if !ok || ref == nil || len(wire.Entity.ProtoReflect().GetUnknown()) != 0 {
		return domain.ObservedState{}, ErrInvalidWireState
	}
	entity, err := domain.ParseEntityID(ref.EntityId)
	if err != nil {
		return domain.ObservedState{}, err
	}
	quality, err := admitQuality(wire.Quality)
	if err != nil {
		return domain.ObservedState{}, err
	}
	observed, err := admitTimestamp(wire.ObservedAt, false)
	if err != nil {
		return domain.ObservedState{}, err
	}
	received, err := admitTimestamp(wire.ReceivedAt, true)
	if err != nil {
		return domain.ObservedState{}, err
	}
	changed, err := admitTimestamp(wire.LastChanged, true)
	if err != nil {
		return domain.ObservedState{}, err
	}
	updated, err := admitTimestamp(wire.LastUpdated, true)
	if err != nil {
		return domain.ObservedState{}, err
	}
	reported, err := admitTimestamp(wire.LastReported, true)
	if err != nil {
		return domain.ObservedState{}, err
	}
	source, err := domain.NewSourceStamp(wire.SourceSession, wire.SourceSeq)
	if err != nil {
		return domain.ObservedState{}, err
	}
	var value *domain.StateValue
	switch item := wire.Value.(type) {
	case *state.ObservedState_SwitchOn:
		if item == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		v := domain.NewSwitchValue(item.SwitchOn)
		value = &v
	case *state.ObservedState_Measurement:
		if item == nil || item.Measurement == nil || len(item.Measurement.ProtoReflect().GetUnknown()) != 0 || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		m, err := measurement.Catalog.AdmitMeasurement(entity, measurement.Endpoint, item.Measurement)
		if err != nil {
			return domain.ObservedState{}, err
		}
		v, err := domain.NewMeasurementValue(m)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
	case *state.ObservedState_Light:
		if item == nil || item.Light == nil || item.Light.On == nil || len(item.Light.ProtoReflect().GetUnknown()) != 0 {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		light, err := domain.NewLightValue(*item.Light.On, item.Light.BrightnessPercent, item.Light.ColorTempMireds)
		if err != nil {
			return domain.ObservedState{}, err
		}
		v, err := domain.NewLightStateValue(light)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if measurement == nil || measurement.Catalog == nil || measurement.Endpoint == "" {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		if err := measurement.Catalog.AdmitDomainLight(entity, measurement.Endpoint, light); err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
	case nil:
	default:
		return domain.ObservedState{}, ErrInvalidWireState
	}
	return domain.NewObservedStateSnapshot(entity, quality, value, domain.StateTimes{
		ObservedAt: observed, ReceivedAt: *received, LastChanged: *changed,
		LastUpdated: *updated, LastReported: *reported,
	}, source, wire.Revision, previous)
}

// EncodeObservedState projects a validated domain snapshot. Last-known display
// values are intentionally separate from the current wire value oneof.
func EncodeObservedState(observed domain.ObservedState) (*state.ObservedState, error) {
	entity, quality, times, source, revision, err := observed.Components()
	if err != nil {
		return nil, err
	}
	for _, at := range []time.Time{times.ReceivedAt, times.LastChanged, times.LastUpdated, times.LastReported} {
		if err := timestamppb.New(at).CheckValid(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidWireState, err)
		}
	}
	if times.ObservedAt != nil {
		if err := timestamppb.New(*times.ObservedAt).CheckValid(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidWireState, err)
		}
	}
	id, err := entity.Value()
	if err != nil {
		return nil, err
	}
	wq, err := wireQuality(quality)
	if err != nil {
		return nil, err
	}
	session, seq, err := source.Components()
	if err != nil {
		return nil, err
	}
	w := &state.ObservedState{
		Entity:  &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: id}},
		Quality: wq, Revision: revision, ReceivedAt: timestamppb.New(times.ReceivedAt),
		LastChanged: timestamppb.New(times.LastChanged), LastUpdated: timestamppb.New(times.LastUpdated),
		LastReported: timestamppb.New(times.LastReported), SourceSession: session, SourceSeq: seq,
	}
	if times.ObservedAt != nil {
		w.ObservedAt = timestamppb.New(*times.ObservedAt)
	}
	if value, present := observed.CurrentValue(); present {
		kind, err := value.Kind()
		if err != nil {
			return nil, err
		}
		switch kind {
		case domain.ValueSwitch:
			on, _ := value.Switch()
			w.Value = &state.ObservedState_SwitchOn{SwitchOn: on}
		case domain.ValueMeasurement:
			m, _ := value.Measurement()
			parts, err := m.Components()
			if err != nil {
				return nil, err
			}
			unit, err := parts.Unit.Symbol()
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Measurement{Measurement: &state.Measurement{Value: parts.Value, Unit: unit}}
		case domain.ValueLight:
			light, _ := value.Light()
			on, brightness, colorTemp, err := light.Components()
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Light{Light: &state.LightValue{On: &on, BrightnessPercent: brightness, ColorTempMireds: colorTemp}}
		case domain.ValueUnspecified:
			return nil, ErrInvalidWireState
		default:
			return nil, ErrInvalidWireState
		}
	}
	return w, nil
}
