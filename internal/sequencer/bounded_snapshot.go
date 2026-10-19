package sequencer

import (
	"errors"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
	"unsafe"
)

var ErrSnapshotBudget = errors.New("sequencer capture exceeds its admitted budget")

// CheckCaptureAvailable preserves Capture's availability test without copying
// the projection. A detached registry is not yet unavailable.
func (s *Sequencer) CheckCaptureAvailable() error {
	if s == nil || s.journal == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryState == registryUnavailable {
		return ErrRegistryUnavailable
	}
	return nil
}

// CaptureBounded charges the global preparation before maps or protobufs are
// copied. Consequently even a later narrow filter can exceed this budget.
// No authority callbacks or SQL execute while the admission mutex is held.
func (s *Sequencer) CaptureBounded(maxBytes int) (Snapshot, error) {
	if s == nil || s.journal == nil || maxBytes < 1 || maxBytes > MaxJournalBytes {
		return Snapshot{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryState == registryUnavailable {
		return Snapshot{}, ErrRegistryUnavailable
	}
	charge := int(unsafe.Sizeof(Snapshot{})) + 1024
	add := func(bytes int) bool {
		if bytes < 0 || bytes > maxBytes-charge {
			return false
		}
		charge += bytes
		return true
	}
	if charge > maxBytes {
		return Snapshot{}, ErrSnapshotBudget
	}
	for id, state := range s.states {
		bytes, err := EstimateTriggerStateBytes(id, state)
		if err != nil || !add(bytes) {
			return Snapshot{}, ErrSnapshotBudget
		}
	}
	for _, record := range s.registry {
		bytes, err := EstimateTriggerRegistryBytes(record)
		if err != nil || !add(bytes) {
			return Snapshot{}, ErrSnapshotBudget
		}
	}
	for id, descriptions := range s.capabilities {
		name, err := id.Value()
		if err != nil || !add(len(name)+128+len(descriptions)*int(unsafe.Sizeof((*registrywire.CapabilityDescription)(nil)))) {
			return Snapshot{}, ErrSnapshotBudget
		}
		for _, description := range descriptions {
			if description == nil {
				return Snapshot{}, ErrInvalidInput
			}
			encoded := proto.Size(description)
			if encoded > (maxBytes-charge)/4 || !add(encoded*4+128) {
				return Snapshot{}, ErrSnapshotBudget
			}
		}
	}
	for id, health := range s.health {
		name, err := id.Value()
		_, detail, healthErr := health.Components()
		if err != nil || healthErr != nil || !add(len(name)+len(detail)+1024) {
			return Snapshot{}, ErrSnapshotBudget
		}
	}
	return s.captureLocked()
}
