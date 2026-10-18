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
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

var (
	ErrInvalidInput        = errors.New("invalid sequencer input")
	ErrEpochMismatch       = errors.New("sequencer cursor belongs to another epoch")
	ErrCursorExpired       = errors.New("sequencer cursor has expired from the memory journal")
	ErrSequenceExhausted   = errors.New("sequencer sequence exhausted")
	ErrProjectionFull      = errors.New("sequencer projection is full")
	ErrRegistryUnavailable = errors.New("registry projection is unavailable")
	ErrIdentityConflict    = errors.New("discovery identity is absent or retired in registry projection")
)

const MaxJournalCapacity = 65536
const MaxProjectedEntities = 4096
const MaxHealthInstances = 1024
const JournalWindow = 120 * time.Second
const MaxJournalBytes = 32 << 20

// Clock.Now must be bounded and free of SQL, network calls, and Host callbacks.
// RealClock reads the local wall clock; source observed_at never orders admission.
type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

var monotonicOrigin = time.Now()

func (RealClock) MonoNow() time.Duration { return time.Since(monotonicOrigin) }

// MonotonicClock is an optional test seam. The value must be a process-local,
// nondecreasing elapsed duration; it never appears as a wall-clock timestamp.
type MonotonicClock interface{ MonoNow() time.Duration }

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

// ParseCursor admits the transport representation; possession of this value
// alone grants no replay authority. The view layer verifies its signed proof.
func ParseCursor(epoch string, sequence uint64) (Cursor, error) {
	if len(epoch) != 32 {
		return Cursor{}, ErrInvalidInput
	}
	decoded, err := hex.DecodeString(epoch)
	if err != nil {
		return Cursor{}, ErrInvalidInput
	}
	var id [16]byte
	copy(id[:], decoded)
	cursor := Cursor{epoch: id, seq: sequence}
	if _, _, err := cursor.Components(); err != nil {
		return Cursor{}, err
	}
	return cursor, nil
}

type Kind uint8

const (
	KindUnspecified Kind = iota
	KindState
	KindEvent
	KindHealth
	KindRegistry
	KindCapabilities
)

// Entry is an immutable value returned from the journal. Only the matching
// accessor succeeds for its kind; the payload cannot be mutated by a reader.
type Entry struct {
	cursor         Cursor
	kind           Kind
	instance       domain.IntegrationInstanceID
	entity         domain.EntityID
	source         domain.SourceStamp
	causality      domain.Causality
	receivedAt     time.Time
	admittedTick   time.Duration
	estimatedBytes int
	state          domain.ObservedState
	transition     domain.StateTransition
	event          integrations.DeviceEvent
	health         integrations.HealthReport
	registry       []integrations.RegistryRecord
	capabilities   []integrations.DiscoveryDeclaration
}

func (e Entry) Cursor() Cursor               { return e.cursor }
func (e Entry) Kind() Kind                   { return e.kind }
func (e Entry) ReceivedAt() time.Time        { return e.receivedAt }
func (e Entry) AdmissionTick() time.Duration { return e.admittedTick }

// Origin is present only for reports admitted from an integration session.
// Registry and capability publications carry no integration-report origin.
func (e Entry) Origin() (domain.IntegrationInstanceID, domain.SourceStamp, domain.Causality, bool) {
	switch e.kind {
	case KindState, KindEvent, KindHealth:
		return e.instance, e.source, e.causality, true
	case KindRegistry, KindCapabilities, KindUnspecified:
		return domain.IntegrationInstanceID{}, domain.SourceStamp{}, domain.Causality{}, false
	default:
		return domain.IntegrationInstanceID{}, domain.SourceStamp{}, domain.Causality{}, false
	}
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
func (e Entry) Registry() ([]integrations.RegistryRecord, bool) {
	if e.kind != KindRegistry {
		return nil, false
	}
	return append([]integrations.RegistryRecord(nil), e.registry...), true
}
func (e Entry) Capabilities() ([]integrations.DiscoveryDeclaration, bool) {
	if e.kind != KindCapabilities {
		return nil, false
	}
	return append([]integrations.DiscoveryDeclaration(nil), e.capabilities...), true
}

// Snapshot copies the current projection and the cursor under one lock. Its
// maps belong to the caller. Future registry/capability projections must join
// this same capture boundary before a full server snapshot can be claimed.
type Snapshot struct {
	Cursor        Cursor
	States        map[domain.EntityID]domain.ObservedState
	Health        map[domain.IntegrationInstanceID]integrations.HealthReport
	Registry      map[domain.EntityID]integrations.RegistryRecord
	Capabilities  map[domain.EntityID][]*wireregistry.CapabilityDescription
	RegistryReady bool
	CapturedTick  time.Duration
}

type Sequencer struct {
	mu             sync.Mutex
	clock          Clock
	started        time.Time
	lastTick       time.Duration
	epoch          [16]byte
	seq            uint64
	journal        []Entry
	journalSizes   []int
	journalBytes   int
	count          int
	states         map[domain.EntityID]domain.ObservedState
	health         map[domain.IntegrationInstanceID]integrations.HealthReport
	registry       map[domain.EntityID]integrations.RegistryRecord
	capabilities   map[domain.EntityID][]*wireregistry.CapabilityDescription
	commandRoutes  map[commandRouteSlot]map[domain.EntityID]struct{}
	commandAliases map[domain.HAAlias]domain.EntityID
	registryState  registryProjectionState
	limits         QueueLimits
	subs           map[*Subscription]struct{}
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
	s := &Sequencer{clock: clock, started: time.Now(), journal: make([]Entry, journalCapacity), journalSizes: make([]int, journalCapacity), states: make(map[domain.EntityID]domain.ObservedState), health: make(map[domain.IntegrationInstanceID]integrations.HealthReport), registry: make(map[domain.EntityID]integrations.RegistryRecord), capabilities: make(map[domain.EntityID][]*wireregistry.CapabilityDescription), commandRoutes: make(map[commandRouteSlot]map[domain.EntityID]struct{}), commandAliases: make(map[domain.HAAlias]domain.EntityID), limits: limits, subs: make(map[*Subscription]struct{})}
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

func (s *Sequencer) append(entry Entry) Entry {
	tick := s.MonotonicNow()
	if tick < s.lastTick {
		tick = s.lastTick
	}
	s.lastTick = tick
	s.seq++
	entry.cursor = Cursor{epoch: s.epoch, seq: s.seq}
	entry.admittedTick = tick
	index := (s.seq - 1) % uint64(len(s.journal))
	s.journalBytes -= s.journalSizes[index]
	entry.estimatedBytes = retainedBytes(entry)
	s.journal[index] = entry
	s.journalSizes[index] = entry.estimatedBytes
	s.journalBytes += s.journalSizes[index]
	if s.count < len(s.journal) {
		s.count++
	}
	s.pruneJournal(tick)
	return entry
}

// retainedBytes is a conservative memory estimate, including the fixed Entry
// body, owned strings, slices and encoded declarations. The projection has a
// separate budget to qualify in NHA-165.
func retainedBytes(entry Entry) int {
	size := int(unsafe.Sizeof(Entry{})) + 256
	instance, source, _, present := entry.Origin()
	if present {
		name, _ := instance.Value()
		session, _, _ := source.Components()
		size += len(name) + len(session)
	}
	switch entry.kind {
	case KindState:
		id, state, _, _ := entry.State()
		name, _ := id.Value()
		size += len(name)
		wire, err := contracts.EncodeObservedState(state)
		if err != nil {
			return MaxJournalBytes + 1
		}
		size += proto.Size(wire) * 4
	case KindEvent:
		id, _, _ := entry.Event()
		name, _ := id.Value()
		size += len(name) + 1024
	case KindHealth:
		size += 1024
	case KindRegistry:
		for _, record := range entry.registry {
			device, entity, _ := record.IDs()
			d, _ := device.Value()
			e, _ := entity.Value()
			instance, key, _ := record.Source()
			i, _ := instance.Value()
			kd, ke, _ := key.Components()
			size += 512 + len(d) + len(e) + len(i) + len(kd) + len(ke)
			_, _, aliases, _ := record.Aliases()
			for _, alias := range aliases {
				a, _ := alias.Value()
				size += len(a) + 32
			}
		}
	case KindCapabilities:
		for _, declaration := range entry.capabilities {
			_, id, _, _, _ := declaration.Identity()
			name, _ := id.Value()
			size += 512 + len(name)
			for _, description := range declaration.Descriptions() {
				size += proto.Size(description)*4 + 128
			}
		}
	case KindUnspecified:
		return MaxJournalBytes + 1
	default:
		return MaxJournalBytes + 1
	}
	return size
}

// pruneJournal keeps a contiguous suffix. A consumer behind that suffix gets
// ErrCursorExpired; admission and live subscriber queues remain independent.
func (s *Sequencer) pruneJournal(now time.Duration) {
	for s.count > 0 {
		oldest := s.seq - uint64(s.count) + 1
		index := (oldest - 1) % uint64(len(s.journal))
		entry := s.journal[index]
		if s.journalBytes <= MaxJournalBytes && now >= entry.admittedTick && now-entry.admittedTick <= JournalWindow {
			break
		}
		s.journalBytes -= s.journalSizes[index]
		s.journalSizes[index] = 0
		s.journal[index] = Entry{}
		s.count--
	}
}

func (s *Sequencer) MonotonicNow() time.Duration {
	if s == nil || s.journal == nil {
		return -1
	}
	if clock, ok := s.clock.(MonotonicClock); ok {
		return clock.MonoNow()
	}
	return time.Since(s.started)
}

// AcceptState is called synchronously under the Host's session lock. The
// revision, projection update, and journal append share this one mutex.
func (s *Sequencer) AcceptState(ctx context.Context, entity domain.EntityID, accepted integrations.AcceptedState) error {
	_, err := s.AcceptStateWithActionReceipt(ctx, entity, accepted)
	return err
}

// AcceptStateWithActionReceipt returns the exact admission point, without a
// full snapshot or a callback while the sequencer lock is held.
func (s *Sequencer) AcceptStateWithActionReceipt(ctx context.Context, entity domain.EntityID, accepted integrations.AcceptedState) (integrations.ActionReceipt, error) {
	if s == nil || s.journal == nil || ctx == nil {
		return integrations.ActionReceipt{}, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return integrations.ActionReceipt{}, err
	}
	instance, source, causality, err := accepted.Metadata()
	if err != nil {
		return integrations.ActionReceipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, err := s.nextTime(ctx)
	if err != nil {
		return integrations.ActionReceipt{}, err
	}
	previous, exists := s.states[entity]
	if accepted.Retirement() {
		// Registry retirement can precede session loss. The Host owns this
		// marker, and it may only end the old session's own projection.
		if exists {
			_, _, _, oldSource, _, err := previous.Components()
			if err != nil {
				return integrations.ActionReceipt{}, err
			}
			oldSession, _, _ := oldSource.Components()
			retiringSession, _, _ := source.Components()
			if oldSession != retiringSession {
				return integrations.ActionReceipt{}, nil
			}
		}
	} else if err := s.admitIdentity(entity, instance); err != nil {
		return integrations.ActionReceipt{}, err
	}
	if !exists && len(s.states) >= MaxProjectedEntities {
		return integrations.ActionReceipt{}, ErrProjectionFull
	}
	var prev *domain.ObservedState
	var revision uint64 = 1
	if exists {
		prev = &previous
		_, _, _, _, current, err := previous.Components()
		if err != nil {
			return integrations.ActionReceipt{}, err
		}
		if current == math.MaxUint64 {
			return integrations.ActionReceipt{}, ErrSequenceExhausted
		}
		revision = current + 1
	}
	report, err := accepted.ReportAt(revision, at)
	if err != nil {
		return integrations.ActionReceipt{}, err
	}
	next, transition, err := domain.ApplyStateReport(entity, prev, report)
	if err != nil {
		return integrations.ActionReceipt{}, err
	}
	s.states[entity] = next
	entry := s.append(Entry{kind: KindState, instance: instance, entity: entity, source: source, causality: causality, receivedAt: at, state: next, transition: transition})
	s.publish(entry)
	point, _ := integrations.NewActionPoint(entry.cursor.seq, revision, entry.admittedTick)
	return integrations.NewActionReceipt(point, next, instance, source, causality)
}

func (s *Sequencer) CommandActionPoint(entity domain.EntityID) (integrations.ActionPoint, error) {
	if s == nil || s.journal == nil {
		return integrations.ActionPoint{}, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return integrations.ActionPoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var revision uint64
	if current, ok := s.states[entity]; ok {
		_, _, _, _, revision, _ = current.Components()
	}
	return integrations.NewActionPoint(s.seq, revision, s.MonotonicNow())
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
	if err := s.admitIdentity(entity, instance); err != nil {
		return err
	}
	s.publish(s.append(Entry{kind: KindEvent, instance: instance, entity: entity, source: accepted.Source, causality: causality, receivedAt: at, event: accepted.Event}))
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
	s.publish(s.append(Entry{kind: KindHealth, instance: instance, source: source, causality: causality, receivedAt: at, health: report}))
	return nil
}

func (s *Sequencer) Capture() (Snapshot, error) {
	if s == nil || s.journal == nil {
		return Snapshot{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryState == registryUnavailable {
		return Snapshot{}, ErrRegistryUnavailable
	}
	states := make(map[domain.EntityID]domain.ObservedState, len(s.states))
	for id, state := range s.states {
		states[id] = state
	}
	health := make(map[domain.IntegrationInstanceID]integrations.HealthReport, len(s.health))
	for id, report := range s.health {
		health[id] = report
	}
	registry := make(map[domain.EntityID]integrations.RegistryRecord, len(s.registry))
	for id, record := range s.registry {
		registry[id] = record
	}
	capabilities := make(map[domain.EntityID][]*wireregistry.CapabilityDescription, len(s.capabilities))
	for id, descriptions := range s.capabilities {
		capabilities[id] = cloneDescriptions(descriptions)
	}
	return Snapshot{Cursor: Cursor{epoch: s.epoch, seq: s.seq}, States: states, Health: health, Registry: registry, Capabilities: capabilities, RegistryReady: s.registryState == registryReady, CapturedTick: s.MonotonicNow()}, nil
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
	s.pruneJournal(s.MonotonicNow())
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
