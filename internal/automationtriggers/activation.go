package automationtriggers

import (
	"reflect"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/mqtt"
)

// clonePlan gives a candidate its own identity and bounded rule allocations.
// The existing admitted charge includes these same IR/rule/wait allocations;
// RetainPlan reserves that charge before the candidate enters the service.
func (p Plan) clonePlan() (Plan, error) {
	if !p.Valid() {
		return Plan{}, ErrInvalidPlan
	}
	next := p
	next.token = &PlanIdentity{admitted: true}
	next.rules = append([]rule(nil), p.rules...)
	next.mqttFilters = append([]mqtt.TopicFilter(nil), p.mqttFilters...)
	next.waits = make(map[waitSource]waitDefinition, len(p.waits))
	for key, definition := range p.waits {
		definition.rules = append([]rule(nil), definition.rules...)
		next.waits[key] = definition
	}
	return next, nil
}

// ClonePlan reads the interpreter's causal plan, including its not-yet-started
// waits, even when another image is already the permanent 040 authority.
func (r *PlanRetention) ClonePlan() (Plan, error) {
	if r == nil || r.service == nil || r.image == nil {
		return Plan{}, ErrInvalidPlan
	}
	r.service.mu.Lock()
	defer r.service.mu.Unlock()
	if r.state == planRetentionReleased || r.image.refs <= 0 || r.service.closed {
		return Plan{}, ErrInvalidPlan
	}
	return r.image.plan.clonePlan()
}

// WithSolar replaces the operational solar port only. An absent source is
// admissible only when this program has no permanent or transient sun rule.
// An empty zone keeps the already admitted civil zone when solar is disabled.
func (p Plan) WithSolar(input PlanInput) (Plan, error) {
	next, err := p.clonePlan()
	if err != nil {
		return Plan{}, err
	}
	if input.Zone != "" {
		if len(input.Zone) > 256 {
			return Plan{}, ErrInvalidPlan
		}
		location, err := time.LoadLocation(input.Zone)
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		next.charge += 2 * (len(input.Zone) - len(next.zone))
		next.zone, next.location = input.Zone, location
	}
	if input.SunSource != nil && nilInterface(input.SunSource) {
		return Plan{}, ErrInvalidPlan
	}
	validate := func(rules []rule) error {
		for _, rule := range rules {
			if rule.trigger.Kind() == ir.TriggerSunset {
				if nilInterface(input.SunSource) {
					return ErrInvalidPlan
				}
				if _, _, err := input.SolarPosition.Components(); err != nil {
					return ErrInvalidPlan
				}
			}
		}
		return nil
	}
	if err := validate(next.rules); err != nil {
		return Plan{}, err
	}
	for _, definition := range next.waits {
		if err := validate(definition.rules); err != nil {
			return Plan{}, err
		}
	}
	next.sun, next.position = input.SunSource, input.SolarPosition
	return next, nil
}

// WithMQTT keeps the exact static topic union. The proof owns the real new
// mapper factory; the server installs that factory after joining the old one.
func (p Plan) WithMQTT(settings mqtt.Settings, ingress mqtt.TriggerEdgePilot) (Plan, error) {
	next, err := p.clonePlan()
	if err != nil {
		return Plan{}, err
	}
	if next.hasMQTTInstance {
		instance, err := settings.Instance()
		if err != nil || !ingress.Covers(instance, next.mqttFilters) {
			return Plan{}, ErrInvalidPlan
		}
		next.mqttInstance, next.mqttIngress, next.mqttEnvironment = instance, ingress, next.token
	}
	return next, nil
}

// PreserveMQTTEnvironment keeps only correlation with the unchanged real
// transport. It retains no old rule array, settings map or program image.
func (p Plan) PreserveMQTTEnvironment(previous Plan) (Plan, error) {
	if !p.Valid() || !previous.Valid() || p.hasMQTTInstance != previous.hasMQTTInstance || p.mqttInstance != previous.mqttInstance || !reflect.DeepEqual(p.mqttFilters, previous.mqttFilters) {
		return Plan{}, ErrInvalidPlan
	}
	next, err := p.clonePlan()
	if err != nil {
		return Plan{}, err
	}
	next.mqttEnvironment = previous.mqttEnvironment
	return next, nil
}

type EnvironmentChanges struct{ sun, civil, mqtt bool }

func (e EnvironmentChanges) Sun() bool   { return e.sun }
func (e EnvironmentChanges) Civil() bool { return e.civil }
func (e EnvironmentChanges) MQTT() bool  { return e.mqtt }

func (r *PlanRetention) EnvironmentChanges(next *PlanRetention) (EnvironmentChanges, error) {
	if r == nil || next == nil || r.service == nil || r.service != next.service || r.image == nil || next.image == nil {
		return EnvironmentChanges{}, ErrInvalidPlan
	}
	r.service.mu.Lock()
	defer r.service.mu.Unlock()
	if r.state == planRetentionReleased || next.state == planRetentionReleased || r.image.refs <= 0 || next.image.refs <= 0 {
		return EnvironmentChanges{}, ErrInvalidPlan
	}
	a, b := r.image.plan, next.image.plan
	return EnvironmentChanges{sun: a.zone != b.zone || a.position != b.position || nilInterface(a.sun) != nilInterface(b.sun),
		civil: a.zone != b.zone, mqtt: a.hasMQTTInstance != b.hasMQTTInstance || a.mqttInstance != b.mqttInstance || a.mqttEnvironment != b.mqttEnvironment}, nil
}
