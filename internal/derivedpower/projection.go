package derivedpower

import (
	"math"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

// The fixed source programs return raw values and attributes, never defaulted
// arithmetic. Their tracked reads are the sole admission proof for a result.
func Definition(kind Kind, target domain.EntityID) (reactivetemplate.Definition, error) {
	binding, err := NewBinding(kind, target)
	if err != nil {
		return reactivetemplate.Definition{}, err
	}
	var source string
	switch kind {
	case KindUPS:
		source = "{{ [states('sensor.ups_load'), state_attr('sensor.ups_load', 'unit_of_measurement')] }}"
	case KindFloorKitchen:
		source = "{{ state_attr('climate.plancher_cuisine', 'hvac_action') }}"
	case KindHVAC:
		source = "{{ state_attr('climate.home', 'hvac_action') }}"
	default:
		return reactivetemplate.Definition{}, ErrInvalidPower
	}
	program, err := template.Compile(source)
	if err != nil {
		return reactivetemplate.Definition{}, err
	}
	// The template returns raw source fields for admission, not the calculated
	// W value; publication is the sole owner of the output target state.
	definition, err := reactivetemplate.NewDefinition(binding.id, program, nil, nil)
	if err != nil {
		return reactivetemplate.Definition{}, err
	}
	if kind == KindHVAC {
		return definition.WithSourceReports()
	}
	return definition, nil
}

type Status uint8

const (
	StatusUnspecified Status = iota
	StatusEstimated
	StatusMissingSource
)

type Projection struct {
	status Status
	watts  float64
}

func (p Projection) Status() Status         { return p.status }
func (p Projection) Watts() (float64, bool) { return p.watts, p.status == StatusEstimated }

func Project(kind Kind, update reactivetemplate.Update) (Projection, error) {
	name, err := kind.ProgramName()
	if err != nil {
		return Projection{}, err
	}
	id, err := update.ID().Value()
	if err != nil || id != name {
		return Projection{}, ErrInvalidPower
	}
	missing := Projection{status: StatusMissingSource}
	result, current := update.CurrentResult()
	if !current || !qualifiedReads(kind, update.Reads()) {
		return missing, nil
	}
	var watts float64
	switch kind {
	case KindUPS:
		items, ok := result.Native().Items()
		if !ok || len(items) != 2 {
			return missing, nil
		}
		loadText, ok := items[0].StringValue()
		if !ok {
			return missing, nil
		}
		unit, ok := items[1].StringValue()
		if !ok || unit != "%" {
			return missing, nil
		}
		load, err := strconv.ParseFloat(strings.TrimSpace(loadText), 64)
		if err != nil || math.IsNaN(load) || math.IsInf(load, 0) {
			return missing, nil
		}
		watts = load / 100 * 900
	case KindFloorKitchen, KindHVAC:
		activityText, ok := result.Native().StringValue()
		if !ok {
			return missing, nil
		}
		activity, ok := reactivetemplate.ParseClimateActivity(activityText)
		if !ok {
			return missing, nil
		}
		switch kind {
		case KindFloorKitchen:
			switch activity {
			case domain.ClimateActivityHeating:
				watts = 1294
			case domain.ClimateActivityIdle:
				watts = 0
			default:
				return missing, nil
			}
		case KindHVAC:
			switch activity {
			case domain.ClimateActivityHeating:
				watts = 15400
			case domain.ClimateActivityCooling:
				watts = 3200
			case domain.ClimateActivityFanOnly:
				watts = 400
			case domain.ClimateActivityIdle:
				watts = 0
			default:
				return missing, nil
			}
		}
	default:
		return Projection{}, ErrInvalidPower
	}
	if math.IsNaN(watts) || math.IsInf(watts, 0) {
		return missing, nil
	}
	return Projection{status: StatusEstimated, watts: watts}, nil
}

func qualifiedReads(kind Kind, reads []template.Read) bool {
	source, err := kind.SourceAlias()
	if err != nil {
		return false
	}
	needed := 1
	if kind == KindUPS {
		needed = 2
	}
	if len(reads) != needed {
		return false
	}
	var identity domain.EntityID
	for i, read := range reads {
		dependency := read.Dependency()
		alias, ok := dependency.Alias()
		aliasText, aliasErr := alias.Value()
		if !ok || aliasErr != nil || aliasText != source || !read.Present() {
			return false
		}
		if kind == KindUPS {
			if i == 0 && dependency.Kind() != template.DependencyState || i == 1 && (dependency.Kind() != template.DependencyAttribute || dependency.Attribute() != "unit_of_measurement") {
				return false
			}
		} else if dependency.Kind() != template.DependencyAttribute || dependency.Attribute() != "hvac_action" {
			return false
		}
		entity, ok := read.Entity()
		if !ok || i != 0 && entity != identity {
			return false
		}
		identity = entity
		quality, ok := read.Quality()
		if !ok {
			return false
		}
		availability, freshness, provenance, err := quality.Components()
		if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
			return false
		}
	}
	return true
}
