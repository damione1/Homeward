// Package sequencer owns the single in-memory admission order for Host
// mutations. It does not own identity resolution, transport, or persistence.
package sequencer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

var (
	ErrInvalidInput      = errors.New("invalid sequencer input")
	ErrEpochMismatch     = errors.New("sequencer cursor belongs to another epoch")
	ErrCursorExpired     = errors.New("sequencer cursor has expired from the memory journal")
	ErrSequenceExhausted = errors.New("sequencer sequence exhausted")
	ErrProjectionFull    = errors.New("sequencer projection is full")
)

const MaxJournalCapacity = 65536
const MaxProjectedEntities = 4096
const MaxHealthInstances = 1024

// Clock.Now must be bounded and free of SQL, network calls, and Host callbacks.
// RealClock reads the local wall clock; source observed_at never orders admission.
type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

type Cursor struct {
	epoch [16]byte
	seq   uint64
}

func (c Cursor) Components() (string, uint64, error) {
	if c.epoch == ([16]byte{}) {
		return "", 0, ErrInvalidInput
	}
	return hex.EncodeToString(c.epoch[:]), c.seq, nil
}

type Kind uint8

const (
	KindUnspecified Kind = iota
	KindState
	KindEvent
	KindHealth
)

// Entry is an immutable value returned from the journal. Only the matching
// accessor succeeds for its kind; the payload cannot be mutated by a reader.
type Entry struct {
	cursor     Cursor
	kind       Kind
	instance   domain.IntegrationInstanceID
	entity     domain.EntityID
	source     domain.SourceStamp
	causality  domain.Causality
	receivedAt time.Time
	state      domain.ObservedState
	transition domain.StateTransition
	event      integrations.DeviceEvent
	health     integrations.HealthReport
}

func (e Entry) Cursor() Cursor        { return e.cursor }
func (e Entry) Kind() Kind            { return e.kind }
func (e Entry) ReceivedAt() time.Time { return e.receivedAt }
func (e Entry) Origin() (domain.IntegrationInstanceID, domain.SourceStamp, domain.Causality) {
	return e.instance, e.source, e.causality
}
func (e Entry) State() (domain.EntityID, domain.ObservedState, domain.StateTransition, bool) {
	return e.entity, e.state, e.transition, e.kind == KindState
}
func (e Entry) Event() (domain.EntityID, integrations.DeviceEvent, bool) {
	return e.entity, e.event, e.kind == KindEvent
}
func (e Entry) Health() (integrations.HealthReport, bool) {
	return e.health, e.kind == KindHealth
}

// Snapshot copies the current projection and the cursor under one lock. Its
// maps belong to the caller. Future registry/capability projections must join
// this same capture boundary before a full server snapshot can be claimed.
type Snapshot struct {
	Cursor Cursor
	States map[domain.EntityID]domain.ObservedState
	Health map[domain.IntegrationInstanceID]integrations.HealthReport
}

type Sequencer struct {
	mu      sync.Mutex
	clock   Clock
	epoch   [16]byte
	seq     uint64
	journal []Entry
	count   int
	states  map[domain.EntityID]domain.ObservedState
	health  map[domain.IntegrationInstanceID]integrations.HealthReport
	limits  QueueLimits
	subs    map[*Subscription]struct{}
}

func New(clock Clock, journalCapacity int) (*Sequencer, error) {
	return NewWithLimits(clock, journalCapacity, DefaultQueueLimits())
}

func NewWithLimits(clock Clock, journalCapacity int, limits QueueLimits) (*Sequencer, error) {
	if clock == nil || journalCapacity < 1 || journalCapacity > MaxJournalCapacity || !limits.valid() {
		return nil, ErrInvalidInput
	}
	v := reflect.ValueOf(clock)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return nil, ErrInvalidInput
		}
	}
	s := &Sequencer{clock: clock, journal: make([]Entry, journalCapacity), states: make(map[domain.EntityID]domain.ObservedState), health: make(map[domain.IntegrationInstanceID]integrations.HealthReport), limits: limits, subs: make(map[*Subscription]struct{})}
	if _, err := rand.Read(s.epoch[:]); err != nil {
		return nil, err
	}
	if s.epoch == ([16]byte{}) {
		return nil, ErrInvalidInput
	}
	return s, nil
}

func (s *Sequencer) Start() (Cursor, error) {
	if s == nil || s.journal == nil {
		return Cursor{}, ErrInvalidInput
	}
	return Cursor{epoch: s.epoch}, nil
}

func (s *Sequencer) nextTime(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if s.seq == math.MaxUint64 {
		return time.Time{}, ErrSequenceExhausted
	}
	at := s.clock.Now()
	if at.IsZero() {
		return time.Time{}, ErrInvalidInput
	}
	return at.UTC(), nil
}

func (s *Sequencer) append(entry Entry) {
	s.seq++
	entry.cursor = Cursor{epoch: s.epoch, seq: s.seq}
	s.journal[(s.seq-1)%uint64(len(s.journal))] = entry
	if s.count < len(s.journal) {
		s.count++
	}
}

// AcceptState is called synchronously under the Host's session lock. The
// revision, projection update, and journal append share this one mutex.
func (s *Sequencer) AcceptState(ctx context.Context, entity domain.EntityID, accepted integrations.AcceptedState) error {
	if s == nil || s.journal == nil || ctx == nil {
		return ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return err
	}
	instance, source, causality, err := accepted.Metadata()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return err
	}
	previous, exists := s.states[entity]
	if !exists && len(s.states) >= MaxProjectedEntities {
		return ErrProjectionFull
	}
	var prev *domain.ObservedState
	var revision uint64 = 1
	if exists {
		prev = &previous
		_, _, _, _, current, err := previous.Components()
		if err != nil {
			return err
		}
		if current == math.MaxUint64 {
			return ErrSequenceExhausted
		}
		revision = current + 1
	}
	report, err := accepted.ReportAt(revision, at)
	if err != nil {
		return err
	}
	next, transition, err := domain.ApplyStateReport(entity, prev, report)
	if err != nil {
		return err
	}
	s.states[entity] = next
	s.append(Entry{kind: KindState, instance: instance, entity: entity, source: source, causality: causality, receivedAt: at, state: next, transition: transition})
	s.publish(s.journal[(s.seq-1)%uint64(len(s.journal))])
	return nil
}

func (s *Sequencer) AcceptEvent(ctx context.Context, entity domain.EntityID, accepted integrations.AcceptedEvent) error {
	if s == nil || s.journal == nil || ctx == nil {
		return ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return err
	}
	instance, causality, err := accepted.Metadata()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return err
	}
	s.append(Entry{kind: KindEvent, instance: instance, entity: entity, source: accepted.Source, causality: causality, receivedAt: at, event: accepted.Event})
	s.publish(s.journal[(s.seq-1)%uint64(len(s.journal))])
	return nil
}

func (s *Sequencer) AcceptHealth(ctx context.Context, instance domain.IntegrationInstanceID, report integrations.HealthReport) error {
	if s == nil || s.journal == nil || ctx == nil {
		return ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return err
	}
	source, causality, err := report.Metadata()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return err
	}
	if _, exists := s.health[instance]; !exists && len(s.health) >= MaxHealthInstances {
		return ErrProjectionFull
	}
	s.health[instance] = report
	s.append(Entry{kind: KindHealth, instance: instance, source: source, causality: causality, receivedAt: at, health: report})
	s.publish(s.journal[(s.seq-1)%uint64(len(s.journal))])
	return nil
}

func (s *Sequencer) Capture() (Snapshot, error) {
	if s == nil || s.journal == nil {
		return Snapshot{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make(map[domain.EntityID]domain.ObservedState, len(s.states))
	for id, state := range s.states {
		states[id] = state
	}
	health := make(map[domain.IntegrationInstanceID]integrations.HealthReport, len(s.health))
	for id, report := range s.health {
		health[id] = report
	}
	return Snapshot{Cursor: Cursor{epoch: s.epoch, seq: s.seq}, States: states, Health: health}, nil
}

func (s *Sequencer) State(entity domain.EntityID) (domain.ObservedState, bool, error) {
	if s == nil || s.journal == nil {
		return domain.ObservedState{}, false, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return domain.ObservedState{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[entity]
	return state, ok, nil
}

// ReadAfter returns a bounded slice in admission order. An expired or foreign
// cursor is an explicit error; callers must capture again, never infer replay.
func (s *Sequencer) ReadAfter(cursor Cursor, limit int) ([]Entry, Cursor, error) {
	if s == nil || s.journal == nil || limit < 1 || limit > len(s.journal) {
		return nil, Cursor{}, ErrInvalidInput
	}
	if _, _, err := cursor.Components(); err != nil {
		return nil, Cursor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor.epoch != s.epoch {
		return nil, Cursor{}, ErrEpochMismatch
	}
	if cursor.seq > s.seq {
		return nil, Cursor{}, ErrInvalidInput
	}
	if s.seq-cursor.seq > uint64(s.count) {
		return nil, Cursor{}, ErrCursorExpired
	}
	available := s.seq - cursor.seq
	if available > uint64(limit) {
		available = uint64(limit)
	}
	out := make([]Entry, 0, available)
	for i := uint64(0); i < available; i++ {
		seq := cursor.seq + i + 1
		out = append(out, s.journal[(seq-1)%uint64(len(s.journal))])
	}
	return out, Cursor{epoch: s.epoch, seq: cursor.seq + available}, nil
}
