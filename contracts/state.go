package contracts

import (
	"errors"
	"fmt"
	"math"
	"time"

	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrInvalidWireState = errors.New("invalid wire state")

func AdmitWireFanSpeed(speed state.FanSpeed) (domain.FanSpeed, error) {
	switch speed {
	case state.FanSpeed_FAN_SPEED_OFF:
		return domain.FanSpeedOff, nil
	case state.FanSpeed_FAN_SPEED_LOW:
		return domain.FanSpeedLow, nil
	case state.FanSpeed_FAN_SPEED_MEDIUM:
		return domain.FanSpeedMedium, nil
	case state.FanSpeed_FAN_SPEED_MEDIUM_HIGH:
		return domain.FanSpeedMediumHigh, nil
	case state.FanSpeed_FAN_SPEED_HIGH:
		return domain.FanSpeedHigh, nil
	case state.FanSpeed_FAN_SPEED_UNSPECIFIED:
	default:
	}
	return domain.FanSpeedUnspecified, ErrInvalidWireState
}

func WireFanSpeed(speed domain.FanSpeed) (state.FanSpeed, error) {
	switch speed {
	case domain.FanSpeedOff:
		return state.FanSpeed_FAN_SPEED_OFF, nil
	case domain.FanSpeedLow:
		return state.FanSpeed_FAN_SPEED_LOW, nil
	case domain.FanSpeedMedium:
		return state.FanSpeed_FAN_SPEED_MEDIUM, nil
	case domain.FanSpeedMediumHigh:
		return state.FanSpeed_FAN_SPEED_MEDIUM_HIGH, nil
	case domain.FanSpeedHigh:
		return state.FanSpeed_FAN_SPEED_HIGH, nil
	case domain.FanSpeedUnspecified:
	default:
	}
	return state.FanSpeed_FAN_SPEED_UNSPECIFIED, ErrInvalidWireState
}

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
	case *state.ObservedState_Weather:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		weather, err := AdmitWireWeatherValue(item.Weather)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainWeather(entity, measurement.Endpoint, weather); err != nil {
			return domain.ObservedState{}, err
		}
		admitted, err := domain.NewWeatherStateValue(weather)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &admitted
	case *state.ObservedState_Presence:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		presence, err := AdmitWirePresenceValue(item.Presence)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainPresence(entity, measurement.Endpoint, presence); err != nil {
			return domain.ObservedState{}, err
		}
		admitted, err := domain.NewPresenceStateValue(presence)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &admitted
	case *state.ObservedState_PresenceDetails:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		details, err := AdmitWirePresenceDetailsValue(item.PresenceDetails)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainPresenceDetails(entity, measurement.Endpoint, details); err != nil {
			return domain.ObservedState{}, err
		}
		admitted, err := domain.NewPresenceDetailsStateValue(details)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &admitted
	case *state.ObservedState_Cover:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		cover, err := AdmitWireCoverValue(item.Cover)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainCover(entity, measurement.Endpoint, cover); err != nil {
			return domain.ObservedState{}, err
		}
		admitted, err := domain.NewCoverStateValue(cover)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &admitted
	case *state.ObservedState_Lock:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		lock, err := AdmitWireLockValue(item.Lock)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainLock(entity, measurement.Endpoint, lock); err != nil {
			return domain.ObservedState{}, err
		}
		admitted, err := domain.NewLockStateValue(lock)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &admitted
	case *state.ObservedState_Alarm:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		alarm, err := AdmitWireAlarmValue(item.Alarm)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainAlarm(entity, measurement.Endpoint, alarm); err != nil {
			return domain.ObservedState{}, err
		}
		admitted, err := domain.NewAlarmStateValue(alarm)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &admitted
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
	case *state.ObservedState_Fan:
		if item == nil || item.Fan == nil || len(item.Fan.ProtoReflect().GetUnknown()) != 0 || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		speed, err := AdmitWireFanSpeed(item.Fan.Speed)
		if err != nil || measurement.Endpoint == "" || measurement.Catalog.AdmitDomainFan(entity, measurement.Endpoint, speed) != nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		v, err := domain.NewFanStateValue(speed)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
	case *state.ObservedState_Climate:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		climate, err := AdmitWireClimateValue(item.Climate)
		if err != nil {
			return domain.ObservedState{}, err
		}
		if err := measurement.Catalog.AdmitDomainClimate(entity, measurement.Endpoint, climate); err != nil {
			return domain.ObservedState{}, err
		}
		v, err := domain.NewClimateStateValue(climate)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
	case *state.ObservedState_Helper:
		if item == nil || item.Helper == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		helper, helperRevision, err := admitHelperWire(item.Helper, true)
		if err != nil || measurement.Catalog.AdmitDomainHelper(entity, measurement.Endpoint, helper) != nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		v, err := domain.NewHelperStateValue(helper, helperRevision)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
	case nil:
	case *state.ObservedState_Solar:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		solar, err := AdmitWireSolarValue(item.Solar)
		if err != nil || measurement.Catalog.AdmitDomainSolar(entity, measurement.Endpoint, solar) != nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		v, err := domain.NewSolarStateValue(solar)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
	case *state.ObservedState_SolarEvent:
		if item == nil || measurement == nil || measurement.Catalog == nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		solar, err := AdmitWireSolarEventValue(item.SolarEvent)
		if err != nil || measurement.Catalog.AdmitDomainSolarEvent(entity, measurement.Endpoint, solar) != nil {
			return domain.ObservedState{}, ErrInvalidWireState
		}
		v, err := domain.NewSolarEventStateValue(solar)
		if err != nil {
			return domain.ObservedState{}, err
		}
		value = &v
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
		case domain.ValueWeather:
			weather, _ := value.Weather()
			encoded, err := EncodeWeatherValue(weather)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Weather{Weather: encoded}
		case domain.ValuePresence:
			presence, _ := value.Presence()
			encoded, err := EncodePresenceValue(presence)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Presence{Presence: encoded}
		case domain.ValuePresenceDetails:
			details, _ := value.PresenceDetails()
			encoded, err := EncodePresenceDetailsValue(details)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_PresenceDetails{PresenceDetails: encoded}
		case domain.ValueCover:
			value, _ := value.Cover()
			encoded, err := EncodeCoverValue(value)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Cover{Cover: encoded}
		case domain.ValueLock:
			value, _ := value.Lock()
			encoded, err := EncodeLockValue(value)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Lock{Lock: encoded}
		case domain.ValueAlarm:
			value, _ := value.Alarm()
			encoded, err := EncodeAlarmValue(value)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Alarm{Alarm: encoded}
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
		case domain.ValueFan:
			speed, _ := value.Fan()
			wireSpeed, err := WireFanSpeed(speed)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Fan{Fan: &state.FanValue{Speed: wireSpeed}}
		case domain.ValueClimate:
			climate, _ := value.Climate()
			wireClimate, err := EncodeClimateValue(climate)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Climate{Climate: wireClimate}
		case domain.ValueHelper:
			helper, helperRevision, _ := value.Helper()
			wireHelper, err := encodeHelperWire(helper, helperRevision)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Helper{Helper: wireHelper}
		case domain.ValueSolar:
			solar, _ := value.Solar()
			encoded, err := EncodeSolarValue(solar)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_Solar{Solar: encoded}
		case domain.ValueSolarEvent:
			solar, _ := value.SolarEvent()
			encoded, err := EncodeSolarEventValue(solar)
			if err != nil {
				return nil, err
			}
			w.Value = &state.ObservedState_SolarEvent{SolarEvent: encoded}
		case domain.ValueUnspecified:
			return nil, ErrInvalidWireState
		default:
			return nil, ErrInvalidWireState
		}
	}
	return w, nil
}

// Helper wire is decoded at an admission boundary, never interpreted directly
// by an integration. A future oneof branch remains decodable but is refused.
func admitHelperWire(w *helperwire.HelperValue, requireRevision bool) (domain.HelperValue, uint64, error) {
	if w == nil || !knownFieldsOnly(w.ProtoReflect()) || (requireRevision && (w.HelperRevision == 0 || w.HelperRevision > math.MaxInt64)) || (!requireRevision && w.HelperRevision != 0) {
		return domain.HelperValue{}, 0, ErrInvalidWireState
	}
	var value domain.HelperValue
	var err error
	switch item := w.Value.(type) {
	case *helperwire.HelperValue_Boolean:
		if item == nil {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		value = domain.NewHelperBoolean(item.Boolean)
	case *helperwire.HelperValue_Number:
		if item == nil {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		value, err = domain.NewHelperNumber(item.Number)
	case *helperwire.HelperValue_Counter:
		if item == nil {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		value = domain.NewHelperCounter(item.Counter)
	case *helperwire.HelperValue_ButtonPressedAt:
		if item == nil || item.ButtonPressedAt == nil || !knownFieldsOnly(item.ButtonPressedAt.ProtoReflect()) || item.ButtonPressedAt.CheckValid() != nil || item.ButtonPressedAt.Nanos%1000 != 0 {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		value, err = domain.NewHelperButtonPress(item.ButtonPressedAt.AsTime())
	case *helperwire.HelperValue_Date:
		if item == nil || item.Date == nil || !knownFieldsOnly(item.Date.ProtoReflect()) {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		var date domain.CivilDate
		date, err = domain.NewCivilDate(int(item.Date.Year), int(item.Date.Month), int(item.Date.Day))
		if err == nil {
			value, err = domain.NewHelperDate(date)
		}
	case *helperwire.HelperValue_Time:
		if item == nil || item.Time == nil || !knownFieldsOnly(item.Time.ProtoReflect()) {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		var clock domain.CivilTime
		clock, err = domain.NewCivilTime(int(item.Time.Hour), int(item.Time.Minute), int(item.Time.Second))
		if err == nil {
			value, err = domain.NewHelperTime(clock)
		}
	case *helperwire.HelperValue_Datetime:
		if item == nil || item.Datetime == nil || !knownFieldsOnly(item.Datetime.ProtoReflect()) || item.Datetime.Date == nil || item.Datetime.Time == nil || !knownFieldsOnly(item.Datetime.Date.ProtoReflect()) || !knownFieldsOnly(item.Datetime.Time.ProtoReflect()) {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		date, de := domain.NewCivilDate(int(item.Datetime.Date.Year), int(item.Datetime.Date.Month), int(item.Datetime.Date.Day))
		clock, ce := domain.NewCivilTime(int(item.Datetime.Time.Hour), int(item.Datetime.Time.Minute), int(item.Datetime.Time.Second))
		if de != nil || ce != nil {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		civil, _ := domain.NewCivilDateTime(date, clock)
		value, err = domain.NewHelperDateTime(civil)
	case *helperwire.HelperValue_SelectedOption:
		if item == nil {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		value, err = domain.NewHelperSelect(item.SelectedOption)
	case *helperwire.HelperValue_Text:
		if item == nil {
			return domain.HelperValue{}, 0, ErrInvalidWireState
		}
		value, err = domain.NewHelperText(item.Text)
	default:
		return domain.HelperValue{}, 0, ErrInvalidWireState
	}
	if err != nil || !value.Valid() {
		return domain.HelperValue{}, 0, ErrInvalidWireState
	}
	return value, w.HelperRevision, nil
}

// AdmitHelperSetValue is the command boundary for a SET payload. The caller
// still checks the action against the active definition and SQL CAS revision.
func AdmitHelperSetValue(w *helperwire.HelperValue) (domain.HelperValue, error) {
	value, _, err := admitHelperWire(w, false)
	return value, err
}

func encodeHelperWire(v domain.HelperValue, revision uint64) (*helperwire.HelperValue, error) {
	kind, err := v.Kind()
	if err != nil || revision == 0 {
		return nil, ErrInvalidWireState
	}
	w := &helperwire.HelperValue{HelperRevision: revision}
	switch kind {
	case domain.HelperBoolean:
		value, _ := v.Boolean()
		w.Value = &helperwire.HelperValue_Boolean{Boolean: value}
	case domain.HelperNumber:
		value, _ := v.Number()
		w.Value = &helperwire.HelperValue_Number{Number: value}
	case domain.HelperCounter:
		value, _ := v.Counter()
		w.Value = &helperwire.HelperValue_Counter{Counter: value}
	case domain.HelperButtonPress:
		value, _ := v.ButtonPress()
		w.Value = &helperwire.HelperValue_ButtonPressedAt{ButtonPressedAt: timestamppb.New(value)}
	case domain.HelperDate:
		value, _ := v.Date()
		y, m, d, _ := value.Components()
		w.Value = &helperwire.HelperValue_Date{Date: &helperwire.CivilDate{Year: uint32(y), Month: uint32(m), Day: uint32(d)}}
	case domain.HelperTime:
		value, _ := v.Time()
		h, m, s, _ := value.Components()
		w.Value = &helperwire.HelperValue_Time{Time: &helperwire.CivilTime{Hour: uint32(h), Minute: uint32(m), Second: uint32(s)}}
	case domain.HelperDateTime:
		value, _ := v.DateTime()
		date, clock, _ := value.Components()
		y, month, day, _ := date.Components()
		h, minute, second, _ := clock.Components()
		w.Value = &helperwire.HelperValue_Datetime{Datetime: &helperwire.CivilDateTime{Date: &helperwire.CivilDate{Year: uint32(y), Month: uint32(month), Day: uint32(day)}, Time: &helperwire.CivilTime{Hour: uint32(h), Minute: uint32(minute), Second: uint32(second)}}}
	case domain.HelperSelect:
		value, _ := v.Select()
		w.Value = &helperwire.HelperValue_SelectedOption{SelectedOption: value}
	case domain.HelperText:
		value, _ := v.Text()
		w.Value = &helperwire.HelperValue_Text{Text: value}
	default:
		return nil, ErrInvalidWireState
	}
	return w, nil
}
