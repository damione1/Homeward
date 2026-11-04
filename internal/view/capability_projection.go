package view

import (
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

// Validate the trusted projector's result against the already filtered item,
// before retaining it. A programming error cannot attach another entity or an
// unreadable source slot to this capture.
func validateProjection(entity domain.EntityID, descriptions []*registry.CapabilityDescription, projected []apiresources.Capability) (int, error) {
	if len(projected) > MaxSnapshotBytes/512 {
		return 0, ErrResourceExhausted
	}
	allowed := make(map[authorization.CapabilitySlot]bool, len(descriptions))
	for _, description := range descriptions {
		if description == nil {
			return 0, ErrInvalidInput
		}
		slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
		if err != nil {
			return 0, ErrInvalidInput
		}
		allowed[slot] = true
	}
	retained := 0
	names := make(map[string]bool, len(projected))
	for _, capability := range projected {
		if !allowed[capability.Source()] {
			return 0, ErrInvalidInput
		}
		charge, err := capability.RetainedBytes()
		if err != nil {
			return 0, ErrInvalidInput
		}
		if charge > MaxSnapshotBytes-retained {
			return 0, ErrResourceExhausted
		}
		resource := capability.Resource()
		name, err := apitypes.ParseCapabilityName(resource.Name)
		if err != nil || names[resource.Name] {
			return 0, ErrInvalidInput
		}
		expected, err := apitypes.EntityNameForIdentity(name.Entity().Home(), entity)
		if err != nil || name.Entity() != expected {
			return 0, ErrInvalidInput
		}
		names[resource.Name] = true
		retained += charge
	}
	return retained, nil
}
