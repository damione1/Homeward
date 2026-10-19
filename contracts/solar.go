package contracts

import (
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func AdmitWireSolarEventKind(value solarwire.SolarEventKind) (domain.SolarEventKind, error) {
	switch value {
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN:
		return domain.SolarDawn, nil
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_DUSK:
		return domain.SolarDusk, nil
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_MIDNIGHT:
		return domain.SolarMidnight, nil
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_NOON:
		return domain.SolarNoon, nil
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_RISING:
		return domain.SolarRising, nil
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_SETTING:
		return domain.SolarSetting, nil
	case solarwire.SolarEventKind_SOLAR_EVENT_KIND_UNSPECIFIED:
		return domain.SolarEventUnspecified, ErrInvalidWireState
	default:
		return domain.SolarEventUnspecified, ErrInvalidWireState
	}
}

func WireSolarEventKind(value domain.SolarEventKind) (solarwire.SolarEventKind, error) {
	switch value {
	case domain.SolarDawn:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN, nil
	case domain.SolarDusk:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_DUSK, nil
	case domain.SolarMidnight:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_MIDNIGHT, nil
	case domain.SolarNoon:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_NOON, nil
	case domain.SolarRising:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_RISING, nil
	case domain.SolarSetting:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_SETTING, nil
	case domain.SolarEventUnspecified:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_UNSPECIFIED, ErrInvalidWireState
	default:
		return solarwire.SolarEventKind_SOLAR_EVENT_KIND_UNSPECIFIED, ErrInvalidWireState
	}
}

func admitSolarOccurrence(w *solarwire.SolarOccurrence) (domain.SolarOccurrence, error) {
	if w == nil || !knownFieldsOnly(w.ProtoReflect()) {
		return domain.SolarOccurrence{}, ErrInvalidWireState
	}
	switch v := w.Occurrence.(type) {
	case *solarwire.SolarOccurrence_At:
		if v == nil || v.At == nil || !knownFieldsOnly(v.At.ProtoReflect()) || v.At.CheckValid() != nil {
			return domain.SolarOccurrence{}, ErrInvalidWireState
		}
		return domain.SolarOccurrenceAt(v.At.AsTime())
	case *solarwire.SolarOccurrence_Absent:
		if v == nil {
			return domain.SolarOccurrence{}, ErrInvalidWireState
		}
		switch v.Absent {
		case solarwire.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON:
			return domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
		case solarwire.SolarAbsenceReason_SOLAR_ABSENCE_REASON_UNSPECIFIED, solarwire.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_TRANSIT_ON_DATE:
			return domain.SolarOccurrence{}, ErrInvalidWireState
		default:
			return domain.SolarOccurrence{}, ErrInvalidWireState
		}
	case nil:
		return domain.SolarOccurrence{}, ErrInvalidWireState
	default:
		return domain.SolarOccurrence{}, ErrInvalidWireState
	}
}

func encodeSolarOccurrence(value domain.SolarOccurrence) (*solarwire.SolarOccurrence, error) {
	at, reason, kind, err := value.Components()
	if err != nil {
		return nil, ErrInvalidWireState
	}
	switch kind {
	case domain.SolarOccurrencePresent:
		return &solarwire.SolarOccurrence{Occurrence: &solarwire.SolarOccurrence_At{At: timestamppb.New(at)}}, nil
	case domain.SolarOccurrenceAbsent:
		switch reason {
		case domain.SolarNoEventWithinSearchHorizon:
			return &solarwire.SolarOccurrence{Occurrence: &solarwire.SolarOccurrence_Absent{Absent: solarwire.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON}}, nil
		case domain.SolarAbsenceUnspecified, domain.SolarNoTransitOnDate:
			return nil, ErrInvalidWireState
		default:
			return nil, ErrInvalidWireState
		}
	case domain.SolarOccurrenceUnspecified:
		return nil, ErrInvalidWireState
	default:
		return nil, ErrInvalidWireState
	}
}

func AdmitWireSolarValue(w *solarwire.SolarValue) (domain.SolarValue, error) {
	if w == nil || !knownFieldsOnly(w.ProtoReflect()) || w.ElevationDegrees == nil || w.AzimuthDegrees == nil || w.Rising == nil {
		return domain.SolarValue{}, ErrInvalidWireState
	}
	var horizon domain.SolarHorizon
	switch w.Horizon {
	case solarwire.SolarHorizon_SOLAR_HORIZON_ABOVE:
		horizon = domain.SolarAboveHorizon
	case solarwire.SolarHorizon_SOLAR_HORIZON_BELOW:
		horizon = domain.SolarBelowHorizon
	case solarwire.SolarHorizon_SOLAR_HORIZON_UNSPECIFIED:
		return domain.SolarValue{}, ErrInvalidWireState
	default:
		return domain.SolarValue{}, ErrInvalidWireState
	}
	input := domain.SolarValueInput{Horizon: horizon, ElevationDegrees: *w.ElevationDegrees, AzimuthDegrees: *w.AzimuthDegrees, Rising: *w.Rising}
	for i, occurrence := range [domain.SolarEventCount]*solarwire.SolarOccurrence{w.NextDawn, w.NextDusk, w.NextMidnight, w.NextNoon, w.NextRising, w.NextSetting} {
		value, err := admitSolarOccurrence(occurrence)
		if err != nil {
			return domain.SolarValue{}, err
		}
		input.Next[i] = value
	}
	return domain.NewSolarValue(input)
}

func EncodeSolarValue(value domain.SolarValue) (*solarwire.SolarValue, error) {
	input, err := value.Components()
	if err != nil {
		return nil, err
	}
	w := &solarwire.SolarValue{ElevationDegrees: &input.ElevationDegrees, AzimuthDegrees: &input.AzimuthDegrees, Rising: &input.Rising}
	switch input.Horizon {
	case domain.SolarAboveHorizon:
		w.Horizon = solarwire.SolarHorizon_SOLAR_HORIZON_ABOVE
	case domain.SolarBelowHorizon:
		w.Horizon = solarwire.SolarHorizon_SOLAR_HORIZON_BELOW
	case domain.SolarHorizonUnspecified:
		return nil, ErrInvalidWireState
	default:
		return nil, ErrInvalidWireState
	}
	var next [domain.SolarEventCount]*solarwire.SolarOccurrence
	for i, occurrence := range input.Next {
		next[i], err = encodeSolarOccurrence(occurrence)
		if err != nil {
			return nil, err
		}
	}
	w.NextDawn, w.NextDusk, w.NextMidnight, w.NextNoon, w.NextRising, w.NextSetting = next[0], next[1], next[2], next[3], next[4], next[5]
	return w, nil
}

func AdmitWireSolarEventValue(w *solarwire.SolarEventValue) (domain.SolarEventValue, error) {
	if w == nil || !knownFieldsOnly(w.ProtoReflect()) {
		return domain.SolarEventValue{}, ErrInvalidWireState
	}
	kind, err := AdmitWireSolarEventKind(w.Event)
	if err != nil {
		return domain.SolarEventValue{}, err
	}
	occurrence, err := admitSolarOccurrence(w.Occurrence)
	if err != nil {
		return domain.SolarEventValue{}, err
	}
	return domain.NewSolarEventValue(kind, occurrence)
}

func EncodeSolarEventValue(value domain.SolarEventValue) (*solarwire.SolarEventValue, error) {
	kind, occurrence, err := value.Components()
	if err != nil {
		return nil, err
	}
	event, err := WireSolarEventKind(kind)
	if err != nil {
		return nil, err
	}
	w, err := encodeSolarOccurrence(occurrence)
	if err != nil {
		return nil, err
	}
	return &solarwire.SolarEventValue{Event: event, Occurrence: w}, nil
}

func validSolarConstraints(value *solarwire.SolarConstraints) bool {
	return value != nil && knownFieldsOnly(value.ProtoReflect())
}
func validSolarEventConstraints(value *solarwire.SolarEventConstraints) bool {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return false
	}
	_, err := AdmitWireSolarEventKind(value.Event)
	return err == nil
}

func (c *CapabilityCatalog) AdmitDomainSolar(entity domain.EntityID, endpoint string, value domain.SolarValue) error {
	if !c.ready() || endpoint == "" {
		return ErrInvalidCapability
	}
	id, err := entity.Value()
	if err != nil {
		return err
	}
	if _, err := value.Components(); err != nil {
		return err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	d := c.instances[id][capabilityKey(SolarSchema, endpoint)]
	if d == nil || !validSolarConstraints(d.GetSolar()) {
		return ErrInvalidCapability
	}
	return nil
}
func (c *CapabilityCatalog) AdmitDomainSolarEvent(entity domain.EntityID, endpoint string, value domain.SolarEventValue) error {
	if !c.ready() || endpoint == "" {
		return ErrInvalidCapability
	}
	id, err := entity.Value()
	if err != nil {
		return err
	}
	kind, _, err := value.Components()
	if err != nil {
		return err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	d := c.instances[id][capabilityKey(SolarEventSchema, endpoint)]
	if d == nil || !validSolarEventConstraints(d.GetSolarEvent()) {
		return ErrInvalidCapability
	}
	declared, err := AdmitWireSolarEventKind(d.GetSolarEvent().Event)
	if err != nil || declared != kind {
		return ErrInvalidCapability
	}
	return nil
}

// Detect the branch itself, including a typed-nil wrapper. Calling generated
// GetSolar alone is not an adequate native-only gate for historical workers.
func NativeSolarDescription(d *registrywire.CapabilityDescription) bool {
	if d == nil {
		return false
	}
	if d.SchemaId == SolarSchema || d.SchemaId == SolarEventSchema {
		return true
	}
	switch d.Constraints.(type) {
	case *registrywire.CapabilityDescription_Solar, *registrywire.CapabilityDescription_SolarEvent:
		return true
	default:
		return false
	}
}
