package sequencer

import (
	"context"
	"sort"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

var _ integrations.ProjectionSink = (*Sequencer)(nil)

type registryProjectionState uint8

const (
	registryDetached registryProjectionState = iota
	registryReady
	registryUnavailable
)

func cloneDescriptions(source []*wireregistry.CapabilityDescription) []*wireregistry.CapabilityDescription {
	out := make([]*wireregistry.CapabilityDescription, len(source))
	for i, description := range source {
		out[i] = proto.Clone(description).(*wireregistry.CapabilityDescription)
	}
	return out
}

// PublishRegistry replaces the already loaded SQL image at one sequence.
// The caller performs all SQL and parsing before taking the sequencer lock.
func (s *Sequencer) PublishRegistry(ctx context.Context, records []integrations.RegistryRecord) error {
	if s == nil || s.journal == nil || ctx == nil || len(records) > MaxProjectedEntities {
		return ErrInvalidInput
	}
	next := make(map[domain.EntityID]integrations.RegistryRecord, len(records))
	for _, record := range records {
		_, entity, err := record.IDs()
		if err != nil {
			return err
		}
		if _, _, _, err := record.Aliases(); err != nil {
			return err
		}
		if _, _, err := record.Source(); err != nil {
			return err
		}
		if _, exists := next[entity]; exists {
			return ErrIdentityConflict
		}
		next[entity] = record
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return err
	}
	changed := make([]integrations.RegistryRecord, 0)
	for entity, record := range next {
		old, exists := s.registry[entity]
		if !exists || !record.Equal(old) {
			changed = append(changed, record)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		_, left, _ := changed[i].IDs()
		_, right, _ := changed[j].IDs()
		l, _ := left.Value()
		r, _ := right.Value()
		return l < r
	})
	// Capabilities of retired/removed identities cannot leak into a new view.
	for entity := range s.capabilities {
		if record, ok := next[entity]; !ok || record.Retired() {
			delete(s.capabilities, entity)
		}
	}
	s.registry = next
	s.registryState = registryReady
	s.rebuildCommandRoutes()
	s.publish(s.append(Entry{kind: KindRegistry, receivedAt: at, registry: changed}))
	return nil
}

// InvalidateRegistry closes admission after a durable write whose new SQL
// image could not be published. A later successful reload reopens admission.
func (s *Sequencer) InvalidateRegistry() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registryState = registryUnavailable
	for sub := range s.subs {
		if sub.class == SubscriptionView && !sub.closed {
			cursor := Cursor{epoch: s.epoch, seq: s.seq}
			sub.loss = &Loss{From: cursor, To: cursor, Reason: LossRegistryUnavailable}
			sub.degraded = true
			sub.replayValid = false
			clear(sub.queue)
			sub.queue = sub.queue[:0]
			sub.queueBytes = 0
			sub.signal()
		}
	}
}

func (s *Sequencer) admitIdentity(entity domain.EntityID, instance domain.IntegrationInstanceID) error {
	switch s.registryState {
	case registryDetached:
		return nil
	case registryUnavailable:
		return ErrRegistryUnavailable
	case registryReady:
	default:
		return ErrInvalidInput
	}
	record, exists := s.registry[entity]
	if !exists || record.Retired() {
		return ErrIdentityConflict
	}
	owner, _, err := record.Source()
	if err != nil || owner != instance {
		return ErrIdentityConflict
	}
	return nil
}

// AcceptDiscovery publishes every binding and its capability set at one seq.
// A stale SQL resolution is refused if a tombstone was published meanwhile.
func (s *Sequencer) AcceptDiscovery(ctx context.Context, declarations []integrations.DiscoveryDeclaration) error {
	if s == nil || s.journal == nil || ctx == nil || len(declarations) == 0 || len(declarations) > 256 {
		return ErrInvalidInput
	}
	staged := make(map[domain.EntityID][]*wireregistry.CapabilityDescription, len(declarations))
	for _, declaration := range declarations {
		_, entity, _, _, err := declaration.Identity()
		if err != nil {
			return err
		}
		if _, exists := staged[entity]; exists {
			return ErrIdentityConflict
		}
		staged[entity] = declaration.Descriptions()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return err
	}
	if len(s.capabilities)+len(staged) > MaxProjectedEntities {
		newCount := len(s.capabilities)
		for entity := range staged {
			if _, exists := s.capabilities[entity]; !exists {
				newCount++
			}
		}
		if newCount > MaxProjectedEntities {
			return ErrProjectionFull
		}
	}
	for _, declaration := range declarations {
		device, entity, instance, key, _ := declaration.Identity()
		if err := s.admitIdentity(entity, instance); err != nil {
			return err
		}
		if s.registryState == registryReady {
			record := s.registry[entity]
			registeredDevice, _, _ := record.IDs()
			_, registeredKey, _ := record.Source()
			if registeredDevice != device || registeredKey != key {
				return ErrIdentityConflict
			}
		}
	}
	for entity, descriptions := range staged {
		s.capabilities[entity] = descriptions
	}
	s.rebuildCommandRoutes()
	s.publish(s.append(Entry{kind: KindCapabilities, receivedAt: at, capabilities: append([]integrations.DiscoveryDeclaration(nil), declarations...)}))
	return nil
}

// WithdrawDiscovery removes one live capability set and invalidates command
// routes. Registry identity and historical state remain in the projection.
func (s *Sequencer) WithdrawDiscovery(ctx context.Context, entity domain.EntityID) error {
	if s == nil || s.journal == nil || ctx == nil {
		return ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return err
	}
	delete(s.capabilities, entity)
	s.rebuildCommandRoutes()
	s.publish(s.append(Entry{kind: KindCapabilities, receivedAt: at}))
	return nil
}
