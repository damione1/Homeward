package entityfilter

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// traitField is constrained by the current generated Capability oneof. It is
// private and admits no arbitrary attribute key or independent string catalogue.
type traitField struct{ number protoreflect.FieldNumber }

func parseTrait(value string) (traitField, error) {
	fields := (&api.Capability{}).ProtoReflect().Descriptor().Oneofs().ByName("trait").Fields()
	field := fields.ByName(protoreflect.Name(value))
	if field == nil {
		return traitField{}, reject(UnknownTrait, 0)
	}
	return traitField{field.Number()}, nil
}

// Candidate owns only the facts needed for selection, copied from an already
// authorized native metadata/capability capture. No retained provider objects,
// hidden capabilities or read-through callbacks are consulted by Match.
type Candidate struct {
	name           apitypes.EntityName
	label          apitypes.DisplayName
	device         apitypes.DeviceName
	traits         []traitField
	availabilities uint8
	valid          bool
}

func NewCandidate(home apitypes.HomeName, metadata apitypes.EntityMetadata, device *apitypes.DeviceName, capabilities []apiresources.Capability) (Candidate, error) {
	if !home.Valid() || !metadata.Valid() || len(capabilities) > MaxCandidateCapabilities {
		return Candidate{}, reject(InvalidCandidate, -1)
	}
	name, err := apitypes.EntityNameForIdentity(home, metadata.Entity())
	if err != nil {
		return Candidate{}, reject(InvalidCandidate, -1)
	}
	result := Candidate{name: name, label: metadata.Label()}
	if device != nil {
		if !device.Valid() || device.Home() != home {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		result.device = *device
	}
	names := make(map[apitypes.CapabilityName]bool, len(capabilities))
	for _, capability := range capabilities {
		resource := capability.Resource()
		if resource == nil {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		capName, err := apitypes.ParseCapabilityName(resource.Name)
		if err != nil || capName.Entity() != name || names[capName] {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		names[capName] = true
		message := resource.ProtoReflect()
		field := message.WhichOneof(message.Descriptor().Oneofs().ByName("trait"))
		if field == nil {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		selected := traitField{field.Number()}
		found := false
		for _, existing := range result.traits {
			found = found || existing == selected
		}
		if !found {
			result.traits = append(result.traits, selected)
		}
		// This traversal names actual fields in the generated trait envelopes. It
		// reads their common typed quality; it does not inspect observed values.
		trait := message.Get(field).Message()
		if !trait.IsValid() {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		observation := trait.Descriptor().Fields().ByName("observation")
		if observation == nil {
			continue
		} // Metadata-only opaque extensions.
		if observation.Kind() != protoreflect.MessageKind {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		if !trait.Has(observation) {
			continue
		}
		reading := trait.Get(observation).Message()
		quality := reading.Descriptor().Fields().ByName("quality")
		if quality == nil || quality.Kind() != protoreflect.MessageKind {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		if !reading.Has(quality) {
			continue
		}
		q := reading.Get(quality).Message()
		availability := q.Descriptor().Fields().ByName("availability")
		if availability == nil || availability.Enum() == nil || availability.Enum().FullName() != api.Availability(0).Descriptor().FullName() {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		value := api.Availability(q.Get(availability).Enum())
		if value == api.Availability_AVAILABILITY_UNSPECIFIED {
			continue // No established fact, distinct from explicit UNKNOWN.
		}
		bit, known := availabilityBit(value)
		if !known {
			return Candidate{}, reject(InvalidCandidate, -1)
		}
		result.availabilities |= bit
	}
	result.valid = true
	return result, nil
}

const (
	availableBit uint8 = 1 << iota
	unavailableBit
	unknownBit
)

func availabilityBit(value api.Availability) (uint8, bool) {
	switch value {
	case api.Availability_AVAILABILITY_AVAILABLE:
		return availableBit, true
	case api.Availability_AVAILABILITY_UNAVAILABLE:
		return unavailableBit, true
	case api.Availability_AVAILABILITY_UNKNOWN:
		return unknownBit, true
	case api.Availability_AVAILABILITY_UNSPECIFIED:
		return 0, false
	default:
		return 0, false
	}
}
func parseAvailability(value string) (api.Availability, bool) {
	descriptor := api.Availability(0).Descriptor().Values().ByName(protoreflect.Name("AVAILABILITY_" + value))
	if descriptor == nil {
		return 0, false
	}
	admitted := api.Availability(descriptor.Number())
	_, ok := availabilityBit(admitted)
	return admitted, ok
}
