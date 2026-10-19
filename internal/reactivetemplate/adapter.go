package reactivetemplate

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrInvalidProjection = errors.New("reactive template: invalid projection")

// ReadViewFromSnapshot uses only the captured SQL registry and current Host
// states. An old display value never stands in for a missing current value.
func ReadViewFromSnapshot(snapshot sequencer.Snapshot, groups []bundle.Group) (template.ReadView, error) {
	if !snapshot.RegistryReady || len(snapshot.Registry) > sequencer.MaxProjectedEntities {
		return template.ReadView{}, ErrInvalidProjection
	}
	sources := make([]template.ReadSource, 0, len(snapshot.Registry))
	for entity, record := range snapshot.Registry {
		if record.Retired() {
			continue
		}
		_, id, err := record.IDs()
		if err != nil || id != entity {
			return template.ReadView{}, ErrInvalidProjection
		}
		_, _, aliases, err := record.Aliases()
		if err != nil {
			return template.ReadView{}, ErrInvalidProjection
		}
		state, exists := snapshot.States[entity]
		if !exists {
			source, err := template.NewUnobservedReadSource(entity, aliases)
			if err != nil {
				return template.ReadView{}, err
			}
			sources = append(sources, source)
			continue
		}
		stateID, quality, _, _, _, err := state.Components()
		if err != nil || stateID != entity {
			return template.ReadView{}, ErrInvalidProjection
		}
		var text *string
		var attrs []template.Binding
		if current, ok := state.CurrentValue(); ok {
			rendered, fields, err := formatCurrent(current)
			if err != nil {
				return template.ReadView{}, err
			}
			text, attrs = &rendered, fields
		}
		source, err := template.NewReadSource(entity, aliases, text, attrs, quality)
		if err != nil {
			return template.ReadView{}, err
		}
		sources = append(sources, source)
	}
	readGroups := make([]template.ReadGroup, 0, len(groups))
	for _, group := range groups {
		if !group.Valid() {
			return template.ReadView{}, ErrInvalidProjection
		}
		readGroup, err := template.NewReadGroup(group.ID(), group.Members())
		if err != nil {
			return template.ReadView{}, err
		}
		readGroups = append(readGroups, readGroup)
	}
	return template.NewReadView(sources, readGroups)
}

func formatCurrent(value domain.StateValue) (string, []template.Binding, error) {
	kind, err := value.Kind()
	if err != nil {
		return "", nil, err
	}
	switch kind {
	case domain.ValueSolar:
		solar, _ := value.Solar()
		input, err := solar.Components()
		if err != nil {
			return "", nil, err
		}
		text := ""
		switch input.Horizon {
		case domain.SolarAboveHorizon:
			text = "above_horizon"
		case domain.SolarBelowHorizon:
			text = "below_horizon"
		case domain.SolarHorizonUnspecified:
			return "", nil, ErrInvalidProjection
		default:
			return "", nil, ErrInvalidProjection
		}
		elevation, err := template.Float(input.ElevationDegrees)
		if err != nil {
			return "", nil, err
		}
		azimuth, err := template.Float(input.AzimuthDegrees)
		if err != nil {
			return "", nil, err
		}
		attrs := []template.Binding{{Name: "elevation", Value: elevation}, {Name: "azimuth", Value: azimuth}, {Name: "rising", Value: template.Bool(input.Rising)}}
		for i, kind := range domain.SolarEventKinds() {
			name, err := kind.Name()
			if err != nil {
				return "", nil, err
			}
			value, err := solarOccurrenceProjection(input.Next[i])
			if err != nil {
				return "", nil, err
			}
			attrs = append(attrs, template.Binding{Name: "next_" + name, Value: value})
		}
		return text, attrs, nil
	case domain.ValueSolarEvent:
		event, _ := value.SolarEvent()
		kind, occurrence, err := event.Components()
		if err != nil {
			return "", nil, err
		}
		name, _ := kind.Name()
		nameValue, err := template.String(name)
		if err != nil {
			return "", nil, err
		}
		projected, err := solarOccurrenceProjection(occurrence)
		if err != nil {
			return "", nil, err
		}
		text, present := projected.StringValue()
		if !present {
			text = "unknown"
		}
		return text, []template.Binding{{Name: "event", Value: nameValue}, {Name: "occurrence", Value: projected}}, nil
	case domain.ValueHelper:
		helper, _, ok := value.Helper()
		if !ok {
			return "", nil, ErrInvalidProjection
		}
		switch helperKind, _ := helper.Kind(); helperKind {
		case domain.HelperBoolean:
			v, _ := helper.Boolean()
			if v {
				return "on", nil, nil
			}
			return "off", nil, nil
		case domain.HelperNumber:
			v, _ := helper.Number()
			return strconv.FormatFloat(v, 'f', -1, 64), nil, nil
		case domain.HelperCounter:
			v, _ := helper.Counter()
			return strconv.FormatInt(v, 10), nil, nil
		case domain.HelperButtonPress:
			v, _ := helper.ButtonPress()
			return v.Format(time.RFC3339Nano), nil, nil
		case domain.HelperDate:
			v, _ := helper.Date()
			year, month, day, _ := v.Components()
			return fmt.Sprintf("%04d-%02d-%02d", year, month, day), nil, nil
		case domain.HelperTime:
			v, _ := helper.Time()
			hour, minute, second, _ := v.Components()
			return fmt.Sprintf("%02d:%02d:%02d", hour, minute, second), nil, nil
		case domain.HelperDateTime:
			v, _ := helper.DateTime()
			date, clock, _ := v.Components()
			year, month, day, _ := date.Components()
			hour, minute, second, _ := clock.Components()
			return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second), nil, nil
		case domain.HelperSelect:
			v, _ := helper.Select()
			return v, nil, nil
		case domain.HelperText:
			v, _ := helper.Text()
			return v, nil, nil
		default:
			return "", nil, ErrInvalidProjection
		}
	case domain.ValueSwitch:
		on, _ := value.Switch()
		if on {
			return "on", nil, nil
		}
		return "off", nil, nil
	case domain.ValueMeasurement:
		measurement, _ := value.Measurement()
		parts, err := measurement.Components()
		if err != nil {
			return "", nil, err
		}
		unit, err := parts.Unit.Symbol()
		if err != nil {
			return "", nil, err
		}
		v, err := template.String(unit)
		if err != nil {
			return "", nil, err
		}
		text := strconv.FormatFloat(parts.Value, 'f', -1, 64)
		return text, []template.Binding{{Name: "unit_of_measurement", Value: v}}, nil
	case domain.ValueLight:
		light, _ := value.Light()
		on, _, mireds, err := light.Components()
		if err != nil {
			return "", nil, err
		}
		var attrs []template.Binding
		if mireds != nil {
			attrs = append(attrs, template.Binding{Name: "color_temp", Value: template.Int(int64(*mireds))})
		}
		if on {
			return "on", attrs, nil
		}
		return "off", attrs, nil
	case domain.ValueFan:
		speed, _ := value.Fan()
		if _, err := speed.String(); err != nil {
			return "", nil, err
		}
		if speed == domain.FanSpeedOff {
			return "off", nil, nil
		}
		return "on", nil, nil
	case domain.ValueClimate:
		climate, _ := value.Climate()
		parts, err := climate.Components()
		if err != nil {
			return "", nil, err
		}
		text := "unknown"
		if parts.Mode != nil {
			text, err = climateMode(*parts.Mode)
			if err != nil {
				return "", nil, err
			}
		}
		attrs := make([]template.Binding, 0, 5)
		if parts.CurrentTemperature != nil {
			v, err := parts.CurrentTemperature.Value()
			if err != nil {
				return "", nil, err
			}
			tv, err := template.Float(v)
			if err != nil {
				return "", nil, err
			}
			attrs = append(attrs, template.Binding{Name: "current_temperature", Value: tv})
		}
		if parts.Setpoint != nil {
			kind, err := parts.Setpoint.Kind()
			if err != nil {
				return "", nil, err
			}
			switch kind {
			case domain.ClimateSetpointSingle:
				v, _ := parts.Setpoint.Single()
				n, err := v.Value()
				if err != nil {
					return "", nil, err
				}
				attr, err := template.Float(n)
				if err != nil {
					return "", nil, err
				}
				attrs = append(attrs, template.Binding{Name: "temperature", Value: attr})
			case domain.ClimateSetpointRange:
				low, high, _ := parts.Setpoint.Range()
				l, err := low.Value()
				if err != nil {
					return "", nil, err
				}
				h, err := high.Value()
				if err != nil {
					return "", nil, err
				}
				lv, err := template.Float(l)
				if err != nil {
					return "", nil, err
				}
				hv, err := template.Float(h)
				if err != nil {
					return "", nil, err
				}
				attrs = append(attrs, template.Binding{Name: "target_temp_low", Value: lv}, template.Binding{Name: "target_temp_high", Value: hv})
			default:
				return "", nil, ErrInvalidProjection
			}
		}
		if parts.Activity != nil {
			action, err := climateActivity(*parts.Activity)
			if err != nil {
				return "", nil, err
			}
			attr, err := template.String(action)
			if err != nil {
				return "", nil, err
			}
			attrs = append(attrs, template.Binding{Name: "hvac_action", Value: attr})
		}
		return text, attrs, nil
	case domain.ValueUnspecified:
		return "", nil, ErrInvalidProjection
	default:
		return "", nil, ErrInvalidProjection
	}
}

func solarOccurrenceProjection(value domain.SolarOccurrence) (template.Value, error) {
	at, reason, kind, err := value.Components()
	if err != nil {
		return template.Value{}, err
	}
	switch kind {
	case domain.SolarOccurrencePresent:
		return template.String(at.UTC().Format(time.RFC3339Nano))
	case domain.SolarOccurrenceAbsent:
		if reason != domain.SolarNoEventWithinSearchHorizon {
			return template.Value{}, ErrInvalidProjection
		}
		return template.None(), nil
	case domain.SolarOccurrenceUnspecified:
		return template.Value{}, ErrInvalidProjection
	default:
		return template.Value{}, ErrInvalidProjection
	}
}

func climateMode(mode domain.ClimateMode) (string, error) {
	switch mode {
	case domain.ClimateModeOff:
		return "off", nil
	case domain.ClimateModeHeat:
		return "heat", nil
	case domain.ClimateModeCool:
		return "cool", nil
	case domain.ClimateModeAuto:
		return "auto", nil
	case domain.ClimateModeDry:
		return "dry", nil
	case domain.ClimateModeFanOnly:
		return "fan_only", nil
	default:
		return "", ErrInvalidProjection
	}
}

func climateActivity(activity domain.ClimateActivity) (string, error) {
	switch activity {
	case domain.ClimateActivityIdle:
		return "idle", nil
	case domain.ClimateActivityHeating:
		return "heating", nil
	case domain.ClimateActivityCooling:
		return "cooling", nil
	case domain.ClimateActivityDrying:
		return "drying", nil
	case domain.ClimateActivityFanOnly:
		return "fan", nil
	default:
		return "", ErrInvalidProjection
	}
}
