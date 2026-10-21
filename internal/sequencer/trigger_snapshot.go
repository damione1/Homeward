package sequencer

import (
	"errors"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

var ErrTriggerViewBudget = errors.New("sequencer trigger view exceeds its admitted memory budget")

// TriggerSnapshot contains only the projections needed to establish a trigger
// capture boundary. Maps belong to the caller; no capability descriptions or
// health reports are cloned. EstimatedBytes is conservative accounting, not a
// measurement of the Go heap allocator's physical footprint.
type TriggerSnapshot struct {
	registryReady  bool
	Cursor         Cursor
	States         map[domain.EntityID]domain.ObservedState
	Registry       map[domain.EntityID]integrations.RegistryRecord
	StateCharges   map[domain.EntityID]int
	RecordCharges  map[domain.EntityID]int
	CapturedTick   time.Duration
	estimatedBytes int
}

func (s TriggerSnapshot) EstimatedBytes() int { return s.estimatedBytes }

// RegistryReady reports the registry state captured under the same sequencer lock.
func (s TriggerSnapshot) RegistryReady() bool { return s.registryReady }

const triggerMapBaseCharge = 512

// EstimateTriggerStateBytes and EstimateTriggerRegistryBytes reuse the journal's
// retained value estimator and add room for the trigger projection's map slots
// and per-entity charge ledger. They are upper-bound admission charges, not
// exact heap-size reports.
func EstimateTriggerStateBytes(id domain.EntityID, state domain.ObservedState) (int, error) {
	entity, _, _, source, _, err := state.Components()
	if err != nil || entity != id {
		return 0, ErrInvalidInput
	}
	entry := Entry{kind: KindState, entity: id, state: state, source: source}
	base := retainedBytes(entry)
	if base <= 0 || base > MaxJournalBytes {
		return 0, ErrTriggerViewBudget
	}
	return base + 256 + 2*(int(unsafe.Sizeof(id))+int(unsafe.Sizeof(state))) + 64, nil
}

func EstimateTriggerRegistryBytes(record integrations.RegistryRecord) (int, error) {
	_, entity, err := record.IDs()
	if err != nil {
		return 0, ErrInvalidInput
	}
	base := retainedBytes(Entry{kind: KindRegistry, registry: []integrations.RegistryRecord{record}})
	if base <= 0 || base > MaxJournalBytes {
		return 0, ErrTriggerViewBudget
	}
	return base + 2*(int(unsafe.Sizeof(entity))+int(unsafe.Sizeof(record))) + 64, nil
}

// CaptureTriggerView estimates the complete trigger projection while holding
// the sequencer lock. It refuses a budget overrun before allocating/copying the
// projection maps. Each individual state or record is already bounded by its
// owning domain and the authoritative retainedBytes estimator.
func (s *Sequencer) CaptureTriggerView(maxBytes int) (TriggerSnapshot, error) {
	if s == nil || s.journal == nil || maxBytes < triggerMapBaseCharge || maxBytes > MaxJournalBytes {
		return TriggerSnapshot{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryState == registryUnavailable {
		return TriggerSnapshot{}, ErrRegistryUnavailable
	}
	charge := triggerMapBaseCharge
	stateCount := 0
	for id, state := range s.states {
		if s.registryState == registryReady {
			record, present := s.registry[id]
			if !present || record.Retired() {
				continue
			}
		}
		item, err := EstimateTriggerStateBytes(id, state)
		if err != nil {
			return TriggerSnapshot{}, err
		}
		if item > maxBytes-charge {
			return TriggerSnapshot{}, ErrTriggerViewBudget
		}
		charge += item
		stateCount++
	}
	recordCount := 0
	for _, record := range s.registry {
		if record.Retired() {
			continue
		}
		item, err := EstimateTriggerRegistryBytes(record)
		if err != nil {
			return TriggerSnapshot{}, err
		}
		if item > maxBytes-charge {
			return TriggerSnapshot{}, ErrTriggerViewBudget
		}
		charge += item
		recordCount++
	}
	states := make(map[domain.EntityID]domain.ObservedState, stateCount)
	stateCharges := make(map[domain.EntityID]int, stateCount)
	for id, state := range s.states {
		if s.registryState == registryReady {
			record, present := s.registry[id]
			if !present || record.Retired() {
				continue
			}
		}
		states[id] = state
		item, _ := EstimateTriggerStateBytes(id, state) // Validated under the same lock above.
		stateCharges[id] = item
	}
	registry := make(map[domain.EntityID]integrations.RegistryRecord, recordCount)
	recordCharges := make(map[domain.EntityID]int, recordCount)
	for id, record := range s.registry {
		if record.Retired() {
			continue
		}
		registry[id] = record
		item, _ := EstimateTriggerRegistryBytes(record)
		recordCharges[id] = item
	}
	return TriggerSnapshot{registryReady: s.registryState == registryReady, Cursor: Cursor{epoch: s.epoch, seq: s.seq}, States: states, Registry: registry,
		StateCharges: stateCharges, RecordCharges: recordCharges, CapturedTick: s.MonotonicNow(), estimatedBytes: charge}, nil
}
