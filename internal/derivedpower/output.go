package derivedpower

import (
	"context"
	"reflect"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

// Publisher owns one Host session and exactly the three admitted output
// bindings. It never sends a command or makes a source-device connection.
type Publisher struct {
	configured [3]Binding
	outputs    [3]integrations.Binding
	session    *integrations.Session
	unit       domain.Unit
}

// A context interface may contain a nil pointer and still compare non-nil.
// Reject it before opening a Host session or submitting a publication.
func nilPowerContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func NewPublisher(bindings []Binding) (*Publisher, error) {
	if len(bindings) != 3 {
		return nil, ErrInvalidPower
	}
	var p Publisher
	var seen [3]bool
	for _, binding := range bindings {
		if !binding.kind.valid() {
			return nil, ErrInvalidPower
		}
		expected, err := NewBinding(binding.kind, binding.target)
		if err != nil || expected != binding {
			return nil, ErrInvalidPower
		}
		index := int(binding.kind - KindUPS)
		if seen[index] {
			return nil, ErrInvalidPower
		}
		seen[index] = true
		p.configured[index] = binding
	}
	unit, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		return nil, err
	}
	p.unit = unit
	return &p, nil
}

// Prepare resolves bindings before reactive publications can enter their
// bounded output queue, and checks Host's resolved IDs against the registry.
func (p *Publisher) Prepare(ctx context.Context, host *integrations.Manager) error {
	if p == nil || host == nil || nilPowerContext(ctx) || p.session != nil {
		return ErrInvalidPower
	}
	instance, err := Instance()
	if err != nil {
		return err
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		return err
	}
	candidates := make([]integrations.Candidate, 0, len(p.configured))
	for _, binding := range p.configured {
		candidate, err := integrations.NewCandidate(binding.key, []*wireregistry.CapabilityDescription{{
			SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Estimated power", Endpoint: "power",
			Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{
				Unit: "W", Precision: 3, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT,
				Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
			}},
		}})
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	outputs, err := session.Discover(ctx, candidates)
	if err != nil || len(outputs) != len(p.outputs) {
		if err != nil {
			return err
		}
		return ErrInvalidPower
	}
	for i, output := range outputs {
		_, entity, err := output.IDs()
		if err != nil || entity != p.configured[i].target {
			return ErrInvalidPower
		}
		p.outputs[i] = output
	}
	p.session = session
	return nil
}

func (p *Publisher) Publish(ctx context.Context, update reactivetemplate.Update) error {
	if p == nil || p.session == nil || nilPowerContext(ctx) {
		return ErrInvalidPower
	}
	var kind Kind
	for _, binding := range p.configured {
		if binding.id == update.ID() {
			kind = binding.kind
			break
		}
	}
	if !kind.valid() {
		return ErrInvalidPower
	}
	projection, err := Project(kind, update)
	if err != nil {
		return err
	}
	index := int(kind - KindUPS)
	now := time.Now().UTC()
	var value *domain.StateValue
	quality, err := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceEstimated)
	if err != nil {
		return err
	}
	if watts, ok := projection.Watts(); ok {
		measurement, err := domain.NewMeasurement(watts, p.unit, 3, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			return err
		}
		state, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			return err
		}
		value = &state
		quality, err = domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceEstimated)
		if err != nil {
			return err
		}
	}
	// The tracked read carries no capture timestamp. Receipt-time integration
	// remains available to NHA-108 without inventing a source capture time.
	observation, err := integrations.NewStateObservation("power", quality, value, nil, now, nil)
	if err != nil {
		return err
	}
	return p.session.Publish(ctx, p.outputs[index], observation)
}
