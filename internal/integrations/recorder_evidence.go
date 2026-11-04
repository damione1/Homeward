package integrations

import (
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/recorderevidence"
)

// NewRecorderEvidence must be called with the registry and declarations from
// the same native publication cut as the state admission.
func NewRecorderEvidence(record RegistryRecord, descriptions []*registry.CapabilityDescription) (recorderevidence.Evidence, error) {
	device, entity, err := record.IDs()
	if err != nil {
		return recorderevidence.Evidence{}, ErrInvalidInput
	}
	instance, source, err := record.Source()
	if err != nil || len(descriptions) == 0 || len(descriptions) > authorization.MaxSlots {
		return recorderevidence.Evidence{}, ErrInvalidInput
	}
	slots := make([]authorization.CapabilitySlot, len(descriptions))
	for i, d := range descriptions {
		if d == nil {
			return recorderevidence.Evidence{}, ErrInvalidInput
		}
		slots[i], err = authorization.NewCapabilitySlot(d.SchemaId, d.Endpoint)
		if err != nil {
			return recorderevidence.Evidence{}, ErrInvalidInput
		}
	}
	sd, se, err := source.Components()
	if err != nil {
		return recorderevidence.Evidence{}, ErrInvalidInput
	}
	binding, err := recorderevidence.NewSource(sd, se)
	if err != nil {
		return recorderevidence.Evidence{}, err
	}
	return recorderevidence.New(device, entity, instance, binding, slots)
}
