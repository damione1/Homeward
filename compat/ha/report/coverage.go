package report

import (
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/domain"
)

func irLocation(source ir.Source) Location {
	at := source.At()
	return location(parser.Position{File: at.File, Line: at.Line, Column: at.Column}, source.Path())
}

func (b *builder) unsupported(object *Object, source Location, refs []domain.HAAlias) {
	b.add(object, nha047.ConstructionInvalid, 0, 0, source, refs, UnsupportedConstruction)
}

func (b *builder) automationFeatures(object *Object, automation ir.Automation) {
	if automation.Mode() == ir.AutomationSingle {
		b.add(object, nha047.AutomationSingle, 0, 0, irLocation(automation.Source()), nil, RefusalNone)
	} else {
		b.unsupported(object, irLocation(automation.Source()), nil)
	}
	if _, specified := automation.Max(); specified {
		b.unsupported(object, irLocation(automation.Source()), nil)
	}
	if len(automation.Variables()) != 0 || len(automation.Triggers()) != 1 {
		b.unsupported(object, irLocation(automation.Source()), nil)
	}
	for _, trigger := range automation.Triggers() {
		source := irLocation(trigger.Source())
		switch trigger.Kind() {
		case ir.TriggerTime:
			b.add(object, nha047.TriggerTime, float64(trigger.SecondsOfDay()), 0, source, nil, RefusalNone)
		case ir.TriggerSunset:
			b.add(object, nha047.TriggerSunset, float64(trigger.OffsetSeconds()), 0, source, nil, RefusalNone)
		case ir.TriggerInvalid, ir.TriggerEvent, ir.TriggerMQTT, ir.TriggerDeviceMQTT, ir.TriggerState, ir.TriggerNumericState, ir.TriggerTimePattern, ir.TriggerCalendar:
			b.unsupported(object, source, nil)
		default:
			b.unsupported(object, source, nil)
		}
	}
	if len(automation.Conditions()) > 2 {
		b.unsupported(object, irLocation(automation.Source()), nil)
	}
	for _, condition := range automation.Conditions() {
		source := irLocation(condition.Source())
		switch condition.Kind() {
		case ir.ConditionState:
			if condition.State() != "on" {
				b.unsupported(object, source, []domain.HAAlias{condition.EntityAlias()})
				continue
			}
			b.add(object, nha047.ConditionState, 0, 0, source, []domain.HAAlias{condition.EntityAlias()}, RefusalNone)
		case ir.ConditionInvalid, ir.ConditionTrigger, ir.ConditionTemplate:
			b.unsupported(object, source, nil)
		default:
			b.unsupported(object, source, nil)
		}
	}
	if len(automation.Actions()) != 1 {
		b.unsupported(object, irLocation(automation.Source()), nil)
	}
	for _, action := range automation.Actions() {
		source := irLocation(action.Source())
		if action.Kind() != ir.ActionCall {
			b.unsupported(object, source, nil)
			continue
		}
		call := action.Call()
		refs := make([]domain.HAAlias, 0, len(call.Targets()))
		for _, target := range call.Targets() {
			refs = append(refs, target.Alias())
		}
		if len(refs) != 1 {
			b.unsupported(object, source, refs)
			continue
		}
		switch call.Kind() {
		case ir.CallClimateSetTemperature:
			value := call.Temperature()
			if !value.Present() {
				b.unsupported(object, source, refs)
				continue
			}
			refusal := b.declared(refs[0], nha047.ClimateSetpoint, value.Value(), 0)
			b.add(object, nha047.ClimateSetpoint, value.Value(), 0, source, refs, refusal)
		case ir.CallSwitchTurnOn:
			b.add(object, nha047.SwitchTurnOn, 0, 0, source, refs, b.declared(refs[0], nha047.SwitchTurnOn, 0, 0))
		case ir.CallInvalid, ir.CallLightTurnOn, ir.CallLightTurnOff, ir.CallSceneTurnOn, ir.CallScript:
			b.unsupported(object, source, refs)
		default:
			b.unsupported(object, source, refs)
		}
	}
}

func (b *builder) sceneFeatures(object *Object, definition scene.Definition) {
	if len(definition.Members()) > 256 {
		object.refusal = UnsupportedConstruction
		return
	}
	for _, member := range definition.Members() {
		refs := []domain.HAAlias{member.Alias()}
		source := location(member.Source(), "$.entities")
		if len(member.Diagnostics()) != 0 {
			b.unsupported(object, source, refs)
			continue
		}
		b.add(object, nha047.SceneMetadata, 0, 0, source, nil, RefusalNone)
		switch member.Kind() {
		case scene.MemberLight:
			light, _ := member.Light()
			on, brightness, ct := light.Components()
			if !on {
				b.add(object, nha047.SceneLightOff, 0, 0, source, refs, b.declared(member.Alias(), nha047.SceneLightOff, 0, 0))
				continue
			}
			if brightness == nil {
				b.unsupported(object, source, refs)
				continue
			}
			b.add(object, nha047.SceneLightBrightness, float64(*brightness), 0, source, refs, b.declared(member.Alias(), nha047.SceneLightBrightness, float64(*brightness), 0))
			if ct != nil {
				b.add(object, nha047.SceneLightCT, float64(*brightness), float64(*ct), source, refs, b.declared(member.Alias(), nha047.SceneLightCT, float64(*brightness), float64(*ct)))
			}
		case scene.MemberSwitch:
			on, _ := member.Switch()
			value := float64(0)
			if on {
				value = 1
			}
			b.add(object, nha047.SceneSwitch, value, 0, source, refs, b.declared(member.Alias(), nha047.SceneSwitch, value, 0))
		case scene.MemberFan:
			fan, _ := member.Fan()
			value := float64(0)
			if fan.On() {
				value = float64(fan.Percentage())
			}
			b.add(object, nha047.SceneFan, value, 0, source, refs, b.declared(member.Alias(), nha047.SceneFan, value, 0))
		case scene.MemberClimate:
			// This profile cannot reproduce humidity/fan/preset climate commands.
			// Keeping the whole member refused prevents a lossy partial migration.
			b.add(object, nha047.SceneClimateUnsupported, 0, 0, source, refs, UnsupportedConstruction)
		case scene.MemberInvalid:
			b.unsupported(object, source, refs)
		default:
			b.unsupported(object, source, refs)
		}
	}
}

func (b *builder) declared(id domain.HAAlias, kind nha047.Construction, value, secondary float64) RefusalKind {
	if r := b.reference(id); r != RefusalNone {
		return r
	}
	d, ok := b.declarations[id]
	if !ok {
		return MissingDeclaredCapability
	}
	switch kind {
	case nha047.SceneLightOff:
		if d.description.GetLight() != nil && d.conversion != NativeOnly {
			return qualifiedSceneLightConversion(d.conversion)
		}
	case nha047.SceneLightBrightness:
		if light := d.description.GetLight(); light != nil && d.conversion != NativeOnly && admitsBounds(light.BrightnessPercent, value*100/255) {
			return qualifiedSceneLightConversion(d.conversion)
		}
	case nha047.SceneLightCT:
		if light := d.description.GetLight(); light != nil && d.conversion != NativeOnly && admitsBounds(light.ColorTempMireds, secondary) {
			return qualifiedSceneLightConversion(d.conversion)
		}
	case nha047.SceneSwitch, nha047.SwitchTurnOn:
		if state := d.description.GetSwitch(); state != nil && state.SetOn {
			return RefusalNone
		}
	case nha047.SceneFan:
		if d.description.GetFan() != nil && d.conversion == CasetaHAFan4 {
			return RefusalNone
		}
	case nha047.ClimateSetpoint:
		if climate := d.description.GetClimate(); climate != nil && admitsBounds(climate.SetpointCelsius, value) {
			return RefusalNone
		}
	case nha047.ConstructionInvalid, nha047.AutomationSingle, nha047.TriggerTime, nha047.TriggerSunset, nha047.ConditionState, nha047.SceneClimateUnsupported, nha047.SceneMetadata, nha047.LegacyServiceAlias, nha047.ExcludedReference, nha047.BlueprintSourceMissing:
		return UnsupportedConstruction
	default:
		return UnsupportedConstruction
	}
	return MissingDeclaredCapability
}

// The scene producer exercises MQTT255. The independent179 oracle is not
// consumed by this report, so it cannot extend the admitted conversion set.
func qualifiedSceneLightConversion(conversion Conversion) RefusalKind {
	switch conversion {
	case MQTTHA255:
		return RefusalNone
	case MQTTHA254, CasetaHALight100:
		return CoverageMissing
	case ConversionUnspecified, NativeOnly, CasetaHAFan4:
		return MissingDeclaredCapability
	default:
		return MissingDeclaredCapability
	}
}
