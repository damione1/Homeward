package automationtriggers

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/sequencer"
)

var (
	ErrInvalidPlan = errors.New("automation triggers: invalid plan")
	ErrCapacity    = errors.New("automation triggers: capacity exceeded")
)

const MaxTriggers = 256

// Resolver is supplied by the authoritative registry at plan admission.
// HA device references are never cast to Homeward device IDs.
type Resolver interface {
	ResolveEntity(domain.HAAlias) (domain.EntityID, error)
	ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error)
	ResolveCalendar(domain.HAAlias) (CalendarSourceID, error)
}

type SolarPosition struct {
	latitude, longitude float64
	valid               bool
}

func NewSolarPosition(latitude, longitude float64) (SolarPosition, error) {
	if math.IsNaN(latitude) || math.IsInf(latitude, 0) || math.IsNaN(longitude) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return SolarPosition{}, ErrInvalidPlan
	}
	return SolarPosition{latitude, longitude, true}, nil
}
func (p SolarPosition) Components() (latitude, longitude float64, err error) {
	if !p.valid {
		return 0, 0, ErrInvalidPlan
	}
	return p.latitude, p.longitude, nil
}

type SunAvailability uint8

const (
	SunAvailabilityInvalid SunAvailability = iota
	SunAbsent
	SunPresent
)

type SunResult struct {
	kind    SunAvailability
	instant time.Time
}

func NoSunEvent() SunResult { return SunResult{kind: SunAbsent} }
func SunEventAt(instant time.Time) (SunResult, error) {
	if instant.IsZero() {
		return SunResult{}, ErrInvalidPlan
	}
	return SunResult{kind: SunPresent, instant: instant.UTC()}, nil
}
func (r SunResult) Components() (time.Time, SunAvailability, error) {
	switch r.kind {
	case SunAbsent:
		return time.Time{}, SunAbsent, nil
	case SunPresent:
		if !r.instant.IsZero() {
			return r.instant, SunPresent, nil
		}
	}
	return time.Time{}, SunAvailabilityInvalid, ErrInvalidPlan
}

// SunSource is the future NHA-105 port. NHA-040 supplies synthetic tests only.
// Implementations must honor context cancellation, including while blocked;
// Close cancels the context before waiting for a source callback in progress.
type SunSource interface {
	NextSunset(context.Context, time.Time, string, SolarPosition) (SunResult, error)
}

type PlanInput struct {
	Automations   []ir.Automation
	Zone          string
	Resolver      Resolver
	MQTTSettings  mqtt.Settings
	SunSource     SunSource
	SolarPosition SolarPosition
}

type rule struct {
	automation string
	trigger    ir.Trigger
	entity     domain.EntityID
	device     domain.DeviceID
	calendar   CalendarSourceID
}

type Plan struct {
	valid           bool
	zone            string
	location        *time.Location
	rules           []rule
	charge          int
	mqttInstance    domain.IntegrationInstanceID
	hasMQTTInstance bool
	mqttFilters     []mqtt.TopicFilter
	sun             SunSource
	position        SolarPosition
}

func (p Plan) Valid() bool { return p.valid && p.location != nil }
func (p Plan) MQTTInstance() (domain.IntegrationInstanceID, bool) {
	return p.mqttInstance, p.hasMQTTInstance
}
func (p Plan) MQTTFilters() []mqtt.TopicFilter {
	return append([]mqtt.TopicFilter(nil), p.mqttFilters...)
}
func (p Plan) Count() int { return len(p.rules) }

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func NewPlan(input PlanInput) (Plan, error) {
	if nilInterface(input.Resolver) || input.Zone == "" || len(input.Zone) > 256 || len(input.Automations) == 0 {
		return Plan{}, ErrInvalidPlan
	}
	location, err := time.LoadLocation(input.Zone)
	if err != nil {
		return Plan{}, ErrInvalidPlan
	}
	plan := Plan{valid: true, zone: input.Zone, location: location, sun: input.SunSource, position: input.SolarPosition,
		charge: 512 + 2*len(input.Zone)}
	totalCount := 0
	for _, automation := range input.Automations {
		if !automation.Valid() {
			return Plan{}, ErrInvalidPlan
		}
		count, charge := automation.TriggerPlanBytes()
		if count > MaxTriggers-totalCount || charge > MaxEngineCharge-sequencer.MaxViewQueueBytes-runtimeFixedCharge-512-256-plan.charge {
			return Plan{}, ErrCapacity
		}
		totalCount += count
		plan.charge += charge
	}
	plan.rules = make([]rule, 0, totalCount)
	seen := make(map[string]bool)
	mqttSeen := make(map[mqtt.TopicFilter]bool)
	needsMQTT := false
	for _, automation := range input.Automations {
		for _, trigger := range automation.Triggers() {
			if len(plan.rules) == MaxTriggers {
				return Plan{}, ErrCapacity
			}
			if trigger.Key() == "" || seen[automation.ID()+"\x00"+trigger.Key()] {
				return Plan{}, ErrInvalidPlan
			}
			seen[automation.ID()+"\x00"+trigger.Key()] = true
			r := rule{automation: automation.ID(), trigger: trigger}
			switch trigger.Kind() {
			case ir.TriggerState, ir.TriggerNumericState:
				r.entity, err = input.Resolver.ResolveEntity(trigger.EntityAlias())
				if err != nil {
					return Plan{}, ErrInvalidPlan
				}
				if _, err = r.entity.Value(); err != nil {
					return Plan{}, ErrInvalidPlan
				}
			case ir.TriggerCalendar:
				r.calendar, err = input.Resolver.ResolveCalendar(trigger.EntityAlias())
				if err != nil || !r.calendar.Valid() {
					return Plan{}, ErrInvalidPlan
				}
			case ir.TriggerDeviceMQTT:
				needsMQTT = true
				r.device, err = input.Resolver.ResolveDevice(trigger.DeviceRef())
				if err != nil {
					return Plan{}, ErrInvalidPlan
				}
				if _, err = r.device.Value(); err != nil {
					return Plan{}, ErrInvalidPlan
				}
			case ir.TriggerMQTT:
				needsMQTT = true
				mqttSeen[trigger.Topic()] = true
			case ir.TriggerSunset:
				if nilInterface(input.SunSource) {
					return Plan{}, ErrInvalidPlan
				}
				if _, _, err = input.SolarPosition.Components(); err != nil {
					return Plan{}, ErrInvalidPlan
				}
			case ir.TriggerTime, ir.TriggerTimePattern, ir.TriggerEvent:
				if trigger.Kind() == ir.TriggerEvent {
					data := trigger.Data()
					_, devicePresent := data.DeviceRef().Value()
					_, commandPresent := data.Command()
					_, clusterPresent := data.ClusterID()
					_, endpointPresent := data.EndpointID()
					_, argsPresent := data.Args()
					if devicePresent == nil || commandPresent || clusterPresent || endpointPresent || argsPresent {
						return Plan{}, ErrInvalidPlan
					}
				}
			default:
				return Plan{}, ErrInvalidPlan
			}
			plan.rules = append(plan.rules, r)
		}
	}
	if needsMQTT {
		instance, err := input.MQTTSettings.Instance()
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		plan.mqttInstance, plan.hasMQTTInstance = instance, true
	}
	if len(mqttSeen) != 0 {
		subs, err := input.MQTTSettings.Subscriptions()
		if err != nil {
			return Plan{}, ErrInvalidPlan
		}
		allowed := make(map[mqtt.TopicFilter]bool, len(subs))
		for _, sub := range subs {
			allowed[sub] = true
		}
		for filter := range mqttSeen {
			if _, err := filter.Value(); err != nil || !allowed[filter] {
				return Plan{}, ErrInvalidPlan
			}
			plan.mqttFilters = append(plan.mqttFilters, filter)
		}
		sort.Slice(plan.mqttFilters, func(i, j int) bool {
			left, _ := plan.mqttFilters[i].Value()
			right, _ := plan.mqttFilters[j].Value()
			return left < right
		})
	}
	return plan, nil
}
