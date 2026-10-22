package derivedpower

import (
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

// HVACSource is the registry identity attested by trusted server composition
// for the configured, qualified Ecobee. Its zero value admits no HVAC source.
// Alias text and source quality alone cannot establish this identity.
type HVACSource struct {
	entity domain.EntityID
	valid  bool
}

func NewHVACSource(entity domain.EntityID) (HVACSource, error) {
	if _, err := entity.Value(); err != nil {
		return HVACSource{}, ErrInvalidPower
	}
	return HVACSource{entity: entity, valid: true}, nil
}

// NewHVACBoundPublisher retains the three existing formulas and outputs. The
// source proof is immutable for this publisher's lifetime; replacement uses a
// new publisher after the existing activation barrier drains the previous one.
func NewHVACBoundPublisher(bindings []Binding, source HVACSource) (*Publisher, error) {
	publisher, err := NewPublisher(bindings)
	if err != nil {
		return nil, err
	}
	publisher.hvacSource = source
	return publisher, nil
}

func (s HVACSource) admits(update reactivetemplate.Update) bool {
	if !s.valid {
		return false
	}
	reads := update.Reads()
	if len(reads) != 1 {
		return false
	}
	entity, present := reads[0].Entity()
	return present && entity == s.entity
}
