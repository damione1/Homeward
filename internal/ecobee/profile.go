package ecobee

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/mctofu/homekit/client"
	wire "github.com/mctofu/homekit/client/characteristic"
	"github.com/mctofu/homekit/client/service"
)

// Adapted from the NHA-009 qualified profile. All bindings are private admitted
// values discovered from the authenticated accessory, never configured AID/IID.
type climateField uint8

const (
	thermalState climateField = iota
	requestedMode
	currentTemperature
	targetTemperature
	displayUnits
	relativeHumidity
	heatThreshold
	coolThreshold
	observedFan
	targetFan
	climateFieldCount
)

type fieldRule struct {
	typ, format, unit string
	required, control bool
}

var climateRules = [climateFieldCount]fieldRule{
	thermalState:       {"F", "uint8", "", true, false},
	requestedMode:      {"33", "uint8", "", true, true},
	currentTemperature: {"11", "float", "celsius", true, false},
	targetTemperature:  {"35", "float", "celsius", true, true},
	displayUnits:       {"36", "uint8", "", true, false},
	relativeHumidity:   {"10", "float", "percentage", false, false},
	heatThreshold:      {"12", "float", "celsius", false, true},
	coolThreshold:      {"D", "float", "celsius", false, true},
	observedFan:        {"AF", "uint8", "", false, false},
	targetFan:          {"BF", "uint8", "", false, true},
}

type binding struct {
	present, writable bool
	id                client.CharacteristicReadRequest
	min, max, step    float64
	hasBounds         bool
	allowed           []float64
}

type climateProfile struct {
	admitted bool
	bindings [climateFieldCount]binding
}

func admitProfile(accessories []*client.RawAccessory) (climateProfile, error) {
	var p climateProfile
	var selected *service.RawService
	var selectedAccessory *client.RawAccessory
	for _, a := range accessories {
		if a == nil || a.ID == 0 {
			return p, fmt.Errorf("accessory identity missing")
		}
		for _, s := range a.Services {
			if s != nil && s.Type == "4A" {
				if selected != nil {
					return p, fmt.Errorf("ambiguous thermostat service")
				}
				selected, selectedAccessory = s, a
			}
		}
	}
	if selected == nil || selected.ID == 0 {
		return p, fmt.Errorf("thermostat service missing")
	}
	services := []*service.RawService{selected}
	for _, s := range selectedAccessory.Services {
		if s != nil && (s.Type == "40" || s.Type == "B7") && (links(selected, s.ID) || links(s, selected.ID)) {
			services = append(services, s)
		}
	}
	seenIDs := make(map[uint64]bool)
	for _, s := range services {
		for _, ch := range s.Characteristics {
			if ch == nil {
				return climateProfile{}, fmt.Errorf("nil characteristic metadata")
			}
			for field, rule := range climateRules {
				if ch.Type != rule.typ || (s != selected && climateField(field) != observedFan && climateField(field) != targetFan) {
					continue
				}
				if ch.ID == 0 || seenIDs[ch.ID] || p.bindings[field].present {
					return climateProfile{}, fmt.Errorf("ambiguous characteristic %s", rule.typ)
				}
				b, err := admitBinding(selectedAccessory.ID, ch, rule)
				if err != nil {
					return climateProfile{}, err
				}
				p.bindings[field], seenIDs[ch.ID] = b, true
			}
		}
	}
	for field, rule := range climateRules {
		if rule.required && !p.bindings[field].present {
			return climateProfile{}, fmt.Errorf("required characteristic %s missing", rule.typ)
		}
	}
	p.admitted = true
	return p, nil
}

func links(s *service.RawService, id uint64) bool {
	for _, linked := range s.Linked {
		if linked == id {
			return true
		}
	}
	return false
}

func admitBinding(aid uint64, ch *wire.RawCharacteristic, rule fieldRule) (binding, error) {
	b := binding{id: client.CharacteristicReadRequest{AccessoryID: aid, CharacteristicID: ch.ID}}
	readable := false
	for _, permission := range ch.Permissions {
		readable = readable || permission == "pr"
		b.writable = b.writable || permission == "pw"
	}
	if !readable || ch.Format != rule.format || ch.Unit != rule.unit {
		return binding{}, fmt.Errorf("permissions/format/unit inconsistent for %s", rule.typ)
	}
	if len(ch.MinValue) > 0 || len(ch.MaxValue) > 0 || len(ch.StepValue) > 0 {
		var err error
		b.min, err = finiteNumber(ch.MinValue)
		if err != nil {
			return binding{}, fmt.Errorf("invalid minimum for %s", rule.typ)
		}
		b.max, err = finiteNumber(ch.MaxValue)
		if err != nil || b.min > b.max {
			return binding{}, fmt.Errorf("invalid maximum for %s", rule.typ)
		}
		b.step, err = finiteNumber(ch.StepValue)
		if err != nil || b.step <= 0 {
			return binding{}, fmt.Errorf("invalid step for %s", rule.typ)
		}
		b.hasBounds = true
	}
	if rule.control && rule.format == "float" && !b.hasBounds {
		return binding{}, fmt.Errorf("control bounds missing for %s", rule.typ)
	}
	if rule.format == "uint8" {
		seen := make(map[float64]bool, len(ch.ValidValues))
		for _, n := range ch.ValidValues {
			if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < 0 || n > 255 || seen[n] {
				return binding{}, fmt.Errorf("invalid valid-values")
			}
			seen[n] = true
			b.allowed = append(b.allowed, n)
		}
		if len(b.allowed) == 0 && b.hasBounds && b.min >= 0 && b.max <= 255 && b.step >= 1 {
			for n := b.min; n <= b.max; n += b.step {
				b.allowed = append(b.allowed, n)
			}
		}
	}
	b.present = true
	return b, nil
}

func finiteNumber(raw wire.Value) (float64, error) {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 || bytes.Equal(v, []byte("null")) {
		return 0, fmt.Errorf("number absent")
	}
	n, err := wire.Value(v).Float64()
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("number is not finite")
	}
	return n, nil
}

func (p climateProfile) readRequest() (*client.CharacteristicsReadRequest, error) {
	if !p.admitted {
		return nil, fmt.Errorf("profile not admitted")
	}
	r := &client.CharacteristicsReadRequest{Metadata: true, Permissions: true, Type: true}
	for _, b := range p.bindings {
		if b.present {
			r.Characteristics = append(r.Characteristics, b.id)
		}
	}
	return r, nil
}

func validState(field climateField, value float64) bool {
	switch field {
	case thermalState, observedFan:
		return math.Trunc(value) == value && value >= 0 && value <= 2
	case requestedMode:
		return math.Trunc(value) == value && value >= 0 && value <= 3
	case displayUnits, targetFan:
		return math.Trunc(value) == value && value >= 0 && value <= 1
	case currentTemperature, targetTemperature, relativeHumidity, heatThreshold, coolThreshold:
		return true
	default:
		return false
	}
}

func (p climateProfile) write(field climateField, value float64) (client.CharacteristicWriteRequest, error) {
	if !p.admitted || field >= climateFieldCount || !climateRules[field].control {
		return client.CharacteristicWriteRequest{}, fmt.Errorf("field is not an admitted control")
	}
	b := p.bindings[field]
	if !b.present || !b.writable || math.IsNaN(value) || math.IsInf(value, 0) || !validState(field, value) {
		return client.CharacteristicWriteRequest{}, fmt.Errorf("control value unavailable or invalid")
	}
	if (field == requestedMode || field == targetFan) && !b.accepts(value) {
		return client.CharacteristicWriteRequest{}, fmt.Errorf("control option not discovered")
	}
	if b.hasBounds && (value < b.min || value > b.max || math.Abs((value-b.min)/b.step-math.Round((value-b.min)/b.step)) > 1e-6) {
		return client.CharacteristicWriteRequest{}, fmt.Errorf("control outside discovered bounds/step")
	}
	return client.CharacteristicWriteRequest{AccessoryID: b.id.AccessoryID, CharacteristicID: b.id.CharacteristicID, Value: value}, nil
}

type valueKind uint8

const (
	valueAbsent valueKind = iota
	valuePresent
	valueNull
	valueDeviceError
	valueUnknown
	valueInvalid
	valueStale
)

type climateValue struct {
	kind   valueKind
	number float64
	at     time.Time
}

type climateObservation struct {
	values [climateFieldCount]climateValue
}

func (p climateProfile) observe(rows []*client.CharacteristicReadResponse, at time.Time) (climateObservation, error) {
	var observation climateObservation
	if !p.admitted || at.IsZero() {
		return observation, fmt.Errorf("unadmitted profile or missing observation time")
	}
	seen := [climateFieldCount]bool{}
	for _, row := range rows {
		if row == nil {
			return climateObservation{}, fmt.Errorf("nil observation")
		}
		matched := false
		for index, b := range p.bindings {
			if !b.present || b.id.AccessoryID != row.AccessoryID || b.id.CharacteristicID != row.CharacteristicID {
				continue
			}
			if seen[index] {
				return climateObservation{}, fmt.Errorf("duplicate observation")
			}
			seen[index], matched = true, true
			field, rule := climateField(index), climateRules[index]
			v := climateValue{at: at}
			raw := bytes.TrimSpace(row.Value)
			switch {
			case row.Status != nil && *row.Status != 0:
				v.kind = valueDeviceError
			case row.Unit != nil && *row.Unit != rule.unit, row.Format != nil && *row.Format != rule.format, row.Type != nil && *row.Type != rule.typ:
				v.kind = valueInvalid
			case len(raw) == 0:
				v.kind = valueAbsent
			case bytes.Equal(raw, []byte("null")):
				v.kind = valueNull
			default:
				n, err := finiteNumber(row.Value)
				switch {
				case err != nil:
					v.kind = valueInvalid
				case !validState(field, n):
					v.kind = valueUnknown
				case b.hasBounds && (n < b.min || n > b.max), field == relativeHumidity && (n < 0 || n > 100):
					v.kind = valueInvalid
				default:
					v.kind, v.number = valuePresent, n
				}
			}
			observation.values[index] = v
		}
		if !matched {
			return climateObservation{}, fmt.Errorf("observation not in discovered profile")
		}
	}
	lo, hi := observation.values[heatThreshold], observation.values[coolThreshold]
	if lo.kind == valuePresent && hi.kind == valuePresent && lo.number > hi.number {
		observation.values[heatThreshold].kind, observation.values[coolThreshold].kind = valueInvalid, valueInvalid
	}
	return observation, nil
}

func (o climateObservation) value(field climateField, now time.Time) climateValue {
	if field >= climateFieldCount {
		return climateValue{kind: valueInvalid}
	}
	v := o.values[field]
	if v.kind == valuePresent {
		if now.Before(v.at) {
			v.kind = valueInvalid
		} else if now.Sub(v.at) >= observationMaxAge {
			v.kind = valueStale
		}
	}
	return v
}

type observedActivity uint8

const (
	activityUnknown observedActivity = iota
	activityHeating
	activityCooling
	activityFanOnly
	activityIdle
)

func (o climateObservation) activity(now time.Time) (observedActivity, valueKind) {
	thermal := o.value(thermalState, now)
	if thermal.kind != valuePresent {
		return activityUnknown, thermal.kind
	}
	switch thermal.number {
	case 1:
		return activityHeating, valuePresent
	case 2:
		return activityCooling, valuePresent
	case 0:
		fan := o.value(observedFan, now)
		if fan.kind != valuePresent {
			return activityUnknown, fan.kind
		}
		switch fan.number {
		case 0, 1:
			return activityIdle, valuePresent
		case 2:
			return activityFanOnly, valuePresent
		}
	}
	return activityUnknown, valueUnknown
}

func (o climateObservation) targetConflict(now time.Time) bool {
	thermal, target := o.value(thermalState, now), o.value(requestedMode, now)
	return thermal.kind == valuePresent && target.kind == valuePresent &&
		((thermal.number == 1 && (target.number == 0 || target.number == 2)) ||
			(thermal.number == 2 && (target.number == 0 || target.number == 1)))
}

func (b binding) accepts(number float64) bool {
	for _, n := range b.allowed {
		if n == number {
			return true
		}
	}
	return false
}
