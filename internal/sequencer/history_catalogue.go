package sequencer

import (
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// EntityCatalogue is an immutable narrow capture from the existing projection.
// Cursor supplies the existing031 admitted high-water, not a catalogue revision.
// Historical read tokens bind the declarations/configuration, not state events.
type EntityCatalogue struct {
	record         integrations.RegistryRecord
	descriptions   []*registry.CapabilityDescription
	cursor         Cursor
	estimatedBytes int
}

func (c EntityCatalogue) Record() integrations.RegistryRecord { return c.record }
func (c EntityCatalogue) Descriptions() []*registry.CapabilityDescription {
	return cloneDescriptions(c.descriptions)
}
func (c EntityCatalogue) Cursor() Cursor      { return c.cursor }
func (c EntityCatalogue) EstimatedBytes() int { return c.estimatedBytes }

// CaptureEntityCatalogue preflights only the selected entity before cloning.
// No SQL, authority callback or state/health/global map copy occurs under s.mu.
func (s *Sequencer) CaptureEntityCatalogue(entity domain.EntityID, maxBytes int) (EntityCatalogue, error) {
	if s == nil || s.journal == nil || maxBytes < 1 || maxBytes > MaxJournalBytes {
		return EntityCatalogue{}, ErrInvalidInput
	}
	name, err := entity.Value()
	if err != nil {
		return EntityCatalogue{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.registryState {
	case registryUnavailable:
		return EntityCatalogue{}, ErrRegistryUnavailable
	case registryDetached:
		return EntityCatalogue{}, ErrIdentityConflict
	case registryReady:
	default:
		return EntityCatalogue{}, ErrInvalidInput
	}
	record, present := s.registry[entity]
	if !present || record.Retired() {
		return EntityCatalogue{}, ErrIdentityConflict
	}
	descriptions := s.capabilities[entity]
	if len(descriptions) == 0 {
		return EntityCatalogue{}, ErrIdentityConflict
	}
	charge, err := EstimateTriggerRegistryBytes(record)
	if err != nil {
		return EntityCatalogue{}, ErrSnapshotBudget
	}
	charge += 256 + len(name)
	if charge > maxBytes {
		return EntityCatalogue{}, ErrSnapshotBudget
	}
	for _, description := range descriptions {
		if description == nil {
			return EntityCatalogue{}, ErrInvalidInput
		}
		encoded := proto.Size(description)
		if encoded > (maxBytes-charge)/4 || encoded*4+128 > maxBytes-charge {
			return EntityCatalogue{}, ErrSnapshotBudget
		}
		charge += encoded*4 + 128
	}
	return EntityCatalogue{record: record, descriptions: cloneDescriptions(descriptions), cursor: Cursor{epoch: s.epoch, seq: s.seq}, estimatedBytes: charge}, nil
}
