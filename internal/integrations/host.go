// Package integrations defines the in-process boundary between drivers and the
// server. IdentityAuthority and Sink are supplied by the registry and sequencer
// owners; the Host itself owns only live session admission.
package integrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

var (
	ErrInvalidInput     = errors.New("invalid integration input")
	ErrInactiveSession  = errors.New("integration session is inactive")
	ErrForeignBinding   = errors.New("binding is not owned by this session")
	ErrIdentityConflict = errors.New("identity authority returned a conflicting binding")
	ErrAdmissionsClosed = errors.New("integration command admissions are closed")
)

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// SourceKey is an open, instance-local protocol identity. It carries no Host ID.
type SourceKey struct{ device, entity string }

func NewSourceKey(device, entity string) (SourceKey, error) {
	if device == "" || entity == "" || len(device) > 256 || len(entity) > 256 || !utf8.ValidString(device) || !utf8.ValidString(entity) {
		return SourceKey{}, ErrInvalidInput
	}
	return SourceKey{device, entity}, nil
}

func (k SourceKey) Components() (string, string, error) {
	if _, err := NewSourceKey(k.device, k.entity); err != nil {
		return "", "", err
	}
	return k.device, k.entity, nil
}

// Candidate owns cloned transport descriptions only at the adapter boundary.
// The Host admits them through CapabilityCatalog before publishing bindings.
type Candidate struct {
	key          SourceKey
	descriptions []*registry.CapabilityDescription
}

func NewCandidate(key SourceKey, descriptions []*registry.CapabilityDescription) (Candidate, error) {
	if _, _, err := key.Components(); err != nil || len(descriptions) == 0 || len(descriptions) > 32 {
		return Candidate{}, ErrInvalidInput
	}
	copyDescriptions := make([]*registry.CapabilityDescription, len(descriptions))
	for i, description := range descriptions {
		if description == nil {
			return Candidate{}, ErrInvalidInput
		}
		copyDescriptions[i] = proto.Clone(description).(*registry.CapabilityDescription)
	}
	return Candidate{key, copyDescriptions}, nil
}

// ResolvedIdentity is assigned by IdentityAuthority, never by a driver.
type ResolvedIdentity struct {
	Device domain.DeviceID
	Entity domain.EntityID
}

// IdentityAuthority must resolve an entire batch atomically. NHA-015 supplies
// the durable registry implementation; fixtures can use an in-memory authority.
type IdentityAuthority interface {
	Resolve(context.Context, domain.IntegrationInstanceID, []SourceKey) ([]ResolvedIdentity, error)
}

// StateObservation describes a raw driver report. The accepting sink allocates
// its revision and may call Report(revision) to use the NHA-017 domain transition.
type StateObservation struct {
	endpoint   string
	quality    domain.Quality
	value      *domain.StateValue
	observedAt *time.Time
	receivedAt time.Time
	sequence   *uint64
	causality  domain.Causality
}

func NewStateObservation(endpoint string, quality domain.Quality, value *domain.StateValue, observedAt *time.Time, receivedAt time.Time, sequence *uint64) (StateObservation, error) {
	if endpoint == "" || len(endpoint) > 128 || !utf8.ValidString(endpoint) {
		return StateObservation{}, ErrInvalidInput
	}
	probe, _ := domain.NewSourceStamp("host-admission-probe", nil)
	if _, err := domain.NewStateReport(quality, value, observedAt, receivedAt, probe, 1); err != nil {
		return StateObservation{}, err
	}
	result := StateObservation{endpoint: endpoint, quality: quality, receivedAt: receivedAt.UTC()}
	if value != nil {
		v := *value
		result.value = &v
	}
	if observedAt != nil {
		at := observedAt.UTC()
		result.observedAt = &at
	}
	if sequence != nil {
		seq := *sequence
		result.sequence = &seq
	}
	return result, nil
}

type AcceptedState struct {
	observation StateObservation
	source      domain.SourceStamp
	instance    domain.IntegrationInstanceID
	retirement  bool
}

// Retirement is set only by Host session revocation, never by a driver.
func (s AcceptedState) Retirement() bool { return s.retirement }

func (s AcceptedState) Report(revision uint64) (domain.StateReport, error) {
	return domain.NewStateReport(s.observation.quality, s.observation.value, s.observation.observedAt, s.observation.receivedAt, s.source, revision)
}

// ReportAt uses the sequencer's local admission clock. The observation's
// receivedAt remains available to older direct Sink fixtures but is not the
// authoritative receipt time for a sequenced mutation.
func (s AcceptedState) ReportAt(revision uint64, receivedAt time.Time) (domain.StateReport, error) {
	return domain.NewStateReport(s.observation.quality, s.observation.value, s.observation.observedAt, receivedAt, s.source, revision)
}

func (s StateObservation) WithCausality(causality domain.Causality) (StateObservation, error) {
	if _, _, err := causality.Components(); err != nil {
		return StateObservation{}, err
	}
	if _, err := domain.NewStateReport(s.quality, s.value, s.observedAt, s.receivedAt, mustProbeSource(), 1); err != nil {
		return StateObservation{}, err
	}
	s.causality = causality
	return s, nil
}

func (s AcceptedState) Metadata() (domain.IntegrationInstanceID, domain.SourceStamp, domain.Causality, error) {
	if _, err := s.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, domain.SourceStamp{}, domain.Causality{}, err
	}
	if _, _, err := s.source.Components(); err != nil {
		return domain.IntegrationInstanceID{}, domain.SourceStamp{}, domain.Causality{}, err
	}
	if _, _, err := s.observation.causality.Components(); err != nil {
		return domain.IntegrationInstanceID{}, domain.SourceStamp{}, domain.Causality{}, err
	}
	return s.instance, s.source, s.observation.causality, nil
}

func mustProbeSource() domain.SourceStamp {
	stamp, _ := domain.NewSourceStamp("host-admission-probe", nil)
	return stamp
}

var eventCode = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// DeviceEvent is an edge with a typed open code and no arbitrary payload map.
type DeviceEvent struct {
	code       string
	observedAt time.Time
	sequence   *uint64
	causality  domain.Causality
}

func NewDeviceEvent(code string, observedAt time.Time, sequence *uint64) (DeviceEvent, error) {
	if !eventCode.MatchString(code) || observedAt.IsZero() {
		return DeviceEvent{}, ErrInvalidInput
	}
	result := DeviceEvent{code: code, observedAt: observedAt.UTC()}
	if sequence != nil {
		seq := *sequence
		result.sequence = &seq
	}
	return result, nil
}

func (e DeviceEvent) Components() (string, time.Time, *uint64, error) {
	if !eventCode.MatchString(e.code) || e.observedAt.IsZero() {
		return "", time.Time{}, nil, ErrInvalidInput
	}
	if e.sequence != nil {
		seq := *e.sequence
		return e.code, e.observedAt, &seq, nil
	}
	return e.code, e.observedAt, nil, nil
}

func (e DeviceEvent) WithCausality(causality domain.Causality) (DeviceEvent, error) {
	if _, _, _, err := e.Components(); err != nil {
		return DeviceEvent{}, err
	}
	if _, _, err := causality.Components(); err != nil {
		return DeviceEvent{}, err
	}
	e.causality = causality
	return e, nil
}

type AcceptedEvent struct {
	Event    DeviceEvent
	Source   domain.SourceStamp
	instance domain.IntegrationInstanceID
}

func (e AcceptedEvent) Metadata() (domain.IntegrationInstanceID, domain.Causality, error) {
	if _, err := e.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, domain.Causality{}, err
	}
	if _, _, _, err := e.Event.Components(); err != nil {
		return domain.IntegrationInstanceID{}, domain.Causality{}, err
	}
	if _, _, err := e.Source.Components(); err != nil {
		return domain.IntegrationInstanceID{}, domain.Causality{}, err
	}
	if _, _, err := e.Event.causality.Components(); err != nil {
		return domain.IntegrationInstanceID{}, domain.Causality{}, err
	}
	return e.instance, e.Event.causality, nil
}

type HealthStatus uint8

const (
	HealthUnspecified HealthStatus = iota
	HealthReady
	HealthDegraded
	HealthUnavailable
)

type HealthReport struct {
	status    HealthStatus
	detail    string
	source    domain.SourceStamp
	causality domain.Causality
}

func NewHealthReport(status HealthStatus, detail string) (HealthReport, error) {
	switch status {
	case HealthReady, HealthDegraded, HealthUnavailable:
		if len(detail) <= 512 && utf8.ValidString(detail) {
			return HealthReport{status: status, detail: detail}, nil
		}
	case HealthUnspecified:
	default:
	}
	return HealthReport{}, ErrInvalidInput
}

func (h HealthReport) WithCausality(causality domain.Causality) (HealthReport, error) {
	if _, _, err := h.Components(); err != nil {
		return HealthReport{}, err
	}
	if _, _, err := causality.Components(); err != nil {
		return HealthReport{}, err
	}
	h.causality = causality
	return h, nil
}

func (h HealthReport) Metadata() (domain.SourceStamp, domain.Causality, error) {
	if _, _, err := h.Components(); err != nil {
		return domain.SourceStamp{}, domain.Causality{}, err
	}
	if _, _, err := h.source.Components(); err != nil {
		return domain.SourceStamp{}, domain.Causality{}, err
	}
	if _, _, err := h.causality.Components(); err != nil {
		return domain.SourceStamp{}, domain.Causality{}, err
	}
	return h.source, h.causality, nil
}

func (h HealthReport) Components() (HealthStatus, string, error) {
	if _, err := NewHealthReport(h.status, h.detail); err != nil {
		return HealthUnspecified, "", err
	}
	return h.status, h.detail, nil
}

// Sink accepts only reports whose live session and binding were checked by the
// Host. Each method must be bounded, synchronous, atomic for one item, and
// free of SQL/network I/O or callbacks into Host: admission holds the Host lock
// until the sink returns, so revocation is linearized with publication.
// NHA-018 supplies global ordering and revision allocation.
type Sink interface {
	AcceptState(context.Context, domain.EntityID, AcceptedState) error
	AcceptEvent(context.Context, domain.EntityID, AcceptedEvent) error
	AcceptHealth(context.Context, domain.IntegrationInstanceID, HealthReport) error
}

type Binding struct {
	key      SourceKey
	device   domain.DeviceID
	entity   domain.EntityID
	instance domain.IntegrationInstanceID
	session  string
}

func (b Binding) Source() (domain.IntegrationInstanceID, SourceKey, error) {
	if _, _, err := b.IDs(); err != nil {
		return domain.IntegrationInstanceID{}, SourceKey{}, err
	}
	return b.instance, b.key, nil
}

func (b Binding) IDs() (domain.DeviceID, domain.EntityID, error) {
	if _, err := b.device.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, ErrInvalidInput
	}
	if _, err := b.entity.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, ErrInvalidInput
	}
	if _, _, err := b.key.Components(); err != nil || b.session == "" {
		return domain.DeviceID{}, domain.EntityID{}, ErrInvalidInput
	}
	if _, err := b.instance.Value(); err != nil {
		return domain.DeviceID{}, domain.EntityID{}, ErrInvalidInput
	}
	return b.device, b.entity, nil
}

// SessionHost is the sole API handed to a driver. It has no session creation,
// registry mutation, SQL, automation, or action-dispatch access.
type SessionHost interface {
	Discover(context.Context, []Candidate) ([]Binding, error)
	Publish(context.Context, Binding, StateObservation) error
	Emit(context.Context, Binding, DeviceEvent) error
	ReportHealth(context.Context, HealthReport) error
}

type Driver interface {
	Start(context.Context, SessionHost) error
	Stop(context.Context) error
}

type Manager struct {
	mu                         sync.Mutex
	admission                  admissionPhase
	authority                  IdentityAuthority
	sink                       Sink
	catalog                    *contracts.CapabilityCatalog
	active                     map[domain.IntegrationInstanceID]*Session
	owners                     map[domain.EntityID]Binding
	commandOwners              map[commandOwnerSlot]domain.EntityID
	inflight                   map[commandEndpointSlot]bool
	volatile                   *volatileCommandLedger
	unprotectedCommandDispatch bool
	commandPolicies            map[domain.CommandID]commandPolicy
	actions                    *actionTracker
	supervisor                 *Supervisor
	durable                    *DurableCommandService
	composition                compositionPhase
	durableRegistration        durableRegistrationPhase
}

type Session struct {
	host          *Manager
	instance      domain.IntegrationInstanceID
	id            string
	active        bool
	done          chan struct{}
	retireErr     error
	work          *taskGroup
	cancelWork    context.CancelFunc
	bindings      map[domain.EntityID]Binding
	commandDriver CommandDriver
}

type admissionPhase uint8

const (
	admissionUnspecified admissionPhase = iota
	admissionOpen
	admissionClosed
)

type compositionPhase uint8

const (
	compositionUnspecified compositionPhase = iota
	compositionOpen
	compositionFrozen
)

type durableRegistrationPhase uint8

const (
	durableRegistrationUnspecified durableRegistrationPhase = iota
	durableRegistrationFree
	durableRegistrationBuilding
	durableRegistrationReady
)

func NewHost(authority IdentityAuthority, sink Sink, catalog *contracts.CapabilityCatalog) (*Manager, error) {
	if nilInterface(authority) || nilInterface(sink) || !catalog.Ready() {
		return nil, ErrInvalidInput
	}
	return &Manager{authority: authority, sink: sink, catalog: catalog, admission: admissionOpen, composition: compositionOpen, durableRegistration: durableRegistrationFree, active: make(map[domain.IntegrationInstanceID]*Session), owners: make(map[domain.EntityID]Binding), commandOwners: make(map[commandOwnerSlot]domain.EntityID), inflight: make(map[commandEndpointSlot]bool), commandPolicies: make(map[domain.CommandID]commandPolicy), actions: newActionTracker(realActionClock{})}, nil
}

// CloseAdmissions linearizes shutdown with both command-policy claims and the
// final route lease. Already leased effects keep their NHA-025 uncertainty.
func (h *Manager) CloseAdmissions() error {
	if h == nil || h.active == nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.admission = admissionClosed
	return nil
}

func (h *Manager) AdmissionsOpen() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admission == admissionOpen
}

// reserveDurable linearizes construction against Supervisor.Start before any
// SQL guard is acquired. A second constructor and a late constructor fail
// without touching PostgreSQL.
func (h *Manager) reserveDurable() error {
	if h == nil || h.active == nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.admission != admissionOpen {
		return ErrAdmissionsClosed
	}
	if h.composition != compositionOpen {
		return ErrSupervisorState
	}
	if h.durableRegistration != durableRegistrationFree {
		return ErrDurableRuntimeBusy
	}
	h.durableRegistration = durableRegistrationBuilding
	return nil
}

func (h *Manager) abortDurable() {
	h.mu.Lock()
	if h.durableRegistration == durableRegistrationBuilding {
		h.durableRegistration = durableRegistrationFree
	}
	h.mu.Unlock()
}

// completeDurable publishes only a fully recovered service. Closing it is
// always performed outside Host's lock.
func (h *Manager) completeDurable(service *DurableCommandService) error {
	if service == nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.durableRegistration != durableRegistrationBuilding || h.composition != compositionOpen {
		return ErrSupervisorState
	}
	if h.admission != admissionOpen {
		return ErrAdmissionsClosed
	}
	h.durable = service
	h.durableRegistration = durableRegistrationReady
	return nil
}

func (h *Manager) startServing(supervisor *Supervisor) (*DurableCommandService, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.supervisor != supervisor || h.composition != compositionOpen || h.durableRegistration == durableRegistrationBuilding || h.admission != admissionOpen {
		return nil, ErrSupervisorState
	}
	h.composition = compositionFrozen
	return h.durable, nil
}

// ClosePreparedRuntime owns cleanup until Supervisor.Start freezes composition.
// Configure must finish synchronously before this call; no service I/O runs
// under Host's mutex. The returned channel closes after the durable guard is
// physically released, including after a bounded Close times out.
func (h *Manager) ClosePreparedRuntime(ctx context.Context) (<-chan struct{}, error) {
	if h == nil || h.active == nil || ctx == nil {
		return nil, ErrInvalidInput
	}
	h.mu.Lock()
	if h.composition != compositionOpen || h.durableRegistration == durableRegistrationBuilding {
		h.mu.Unlock()
		return nil, ErrSupervisorState
	}
	h.admission = admissionClosed
	service := h.durable
	h.mu.Unlock()
	if service == nil {
		return nil, nil
	}
	return service.Done(), service.Close(ctx)
}

// ReportPilotDegraded records a failed factory before any session exists.
// It refuses to overwrite the health of an independently opened owner.
func (h *Manager) ReportPilotDegraded(instance domain.IntegrationInstanceID) error {
	if h == nil || h.active == nil {
		return ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active[instance] != nil {
		return ErrInactiveSession
	}
	report, _ := NewHealthReport(HealthDegraded, "pilot factory failed")
	report.source, _ = domain.NewSourceStamp("supervisor", nil)
	return h.sink.AcceptHealth(context.Background(), instance, report)
}

// Capabilities returns the admitted declarations for a bound Host entity. It
// does not grant ownership and returns clones so callers cannot change limits.
func (h *Manager) Capabilities(entity domain.EntityID) ([]*registry.CapabilityDescription, error) {
	if h == nil || h.catalog == nil {
		return nil, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.catalog.Descriptions(entity)
}

func (h *Manager) OpenSession(instance domain.IntegrationInstanceID) (*Session, error) {
	if h == nil || h.active == nil {
		return nil, ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return nil, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.admission != admissionOpen {
		return nil, ErrAdmissionsClosed
	}
	if previous := h.active[instance]; previous != nil {
		if err := h.retireLocked(previous, HealthDegraded, "session replaced"); err != nil {
			return nil, err
		}
	}
	s := &Session{host: h, instance: instance, id: hex.EncodeToString(token[:]), active: true, done: make(chan struct{}), bindings: make(map[domain.EntityID]Binding)}
	h.active[instance] = s
	return s, nil
}

func (h *Manager) Revoke(s *Session) error {
	return h.RevokeWithHealth(s, HealthDegraded, "session revoked")
}

// RevokeWithHealth fences the session first, then reports unavailable states
// and health through the same sequencer sink while the Host lock is held.
func (h *Manager) RevokeWithHealth(s *Session, status HealthStatus, detail string) error {
	if h == nil || s == nil || s.host != h {
		return ErrInvalidInput
	}
	if status != HealthDegraded && status != HealthUnavailable {
		return ErrInvalidInput
	}
	if _, err := NewHealthReport(status, detail); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.active || h.active[s.instance] != s {
		return ErrInactiveSession
	}
	return h.retireLocked(s, status, detail)
}

func (h *Manager) retireLocked(s *Session, status HealthStatus, detail string) error {
	s.active = false
	delete(h.active, s.instance)
	if s.cancelWork != nil {
		s.cancelWork()
	}
	quality, _ := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceObserved)
	source, _ := domain.NewSourceStamp(s.id, nil)
	var failures []error
	entities := make([]domain.EntityID, 0, len(s.bindings))
	for entity := range s.bindings {
		entities = append(entities, entity)
	}
	sort.Slice(entities, func(i, j int) bool {
		left, _ := entities[i].Value()
		right, _ := entities[j].Value()
		return left < right
	})
	for _, entity := range entities {
		binding := s.bindings[entity]
		if h.owners[entity] == binding {
			delete(h.owners, entity)
			observation, err := NewStateObservation("session", quality, nil, nil, time.Now(), nil)
			if err == nil {
				err = h.sink.AcceptState(context.Background(), entity, AcceptedState{observation: observation, source: source, instance: s.instance, retirement: true})
			}
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	report, _ := NewHealthReport(status, detail)
	report.source = source
	if err := h.sink.AcceptHealth(context.Background(), s.instance, report); err != nil {
		failures = append(failures, err)
	}
	s.retireErr = errors.Join(failures...)
	close(s.done)
	return s.retireErr
}

func (h *Manager) manageSession(s *Session, group *taskGroup, cancel context.CancelFunc) error {
	if h == nil || s == nil || group == nil || cancel == nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.live() || s.work != nil {
		return ErrInactiveSession
	}
	s.work, s.cancelWork = group, cancel
	return nil
}

// Done closes at spontaneous replacement or revocation, without requiring a
// later Publish call by the worker to notice that its lease was lost.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

func (s *Session) RetirementError() error {
	if s == nil || s.host == nil {
		return ErrInvalidInput
	}
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	if s.active {
		return ErrSupervisorState
	}
	return s.retireErr
}

func (s *Session) live() bool {
	return s != nil && s.host != nil && s.active && s.host.active[s.instance] == s
}

func (s *Session) Discover(ctx context.Context, candidates []Candidate) ([]Binding, error) {
	if s == nil || s.host == nil {
		return nil, ErrInactiveSession
	}
	h := s.host
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 || len(candidates) > 256 {
		return nil, ErrInvalidInput
	}
	keys := make([]SourceKey, len(candidates))
	seenKeys := make(map[SourceKey]bool, len(candidates))
	for i, candidate := range candidates {
		if _, err := NewCandidate(candidate.key, candidate.descriptions); err != nil {
			return nil, ErrInvalidInput
		}
		if seenKeys[candidate.key] {
			return nil, ErrInvalidInput
		}
		seenKeys[candidate.key] = true
		keys[i] = candidate.key
		seenCapabilities := make(map[string]bool)
		for _, description := range candidate.descriptions {
			if err := h.catalog.AdmitDriverCapability(description); err != nil {
				return nil, err
			}
			key := description.SchemaId + "\x00" + description.Endpoint
			if seenCapabilities[key] {
				return nil, ErrInvalidInput
			}
			seenCapabilities[key] = true
		}
	}
	h.mu.Lock()
	if !s.live() {
		h.mu.Unlock()
		return nil, ErrInactiveSession
	}
	h.mu.Unlock()
	resolved, err := h.authority.Resolve(ctx, s.instance, keys)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.live() {
		return nil, ErrInactiveSession
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(resolved) != len(keys) {
		return nil, ErrIdentityConflict
	}
	bindings := make([]Binding, len(keys))
	seenEntities := make(map[domain.EntityID]bool, len(keys))
	for i, identity := range resolved {
		if _, err := identity.Device.Value(); err != nil {
			return nil, ErrIdentityConflict
		}
		if _, err := identity.Entity.Value(); err != nil {
			return nil, ErrIdentityConflict
		}
		if seenEntities[identity.Entity] {
			return nil, ErrIdentityConflict
		}
		seenEntities[identity.Entity] = true
		if previous, exists := h.owners[identity.Entity]; exists && (previous.key != keys[i] || previous.device != identity.Device || previous.instance != s.instance) {
			return nil, ErrIdentityConflict
		}
		bindings[i] = Binding{key: keys[i], device: identity.Device, entity: identity.Entity, instance: s.instance, session: s.id}
	}
	registrations := make([]contracts.DriverCapabilityRegistration, 0)
	declarations := make([]DiscoveryDeclaration, 0, len(bindings))
	for i, binding := range bindings {
		declaration, err := newDiscoveryDeclaration(binding.device, binding.entity, s.instance, binding.key, candidates[i].descriptions)
		if err != nil {
			return nil, err
		}
		declarations = append(declarations, declaration)
		for _, description := range candidates[i].descriptions {
			registrations = append(registrations, contracts.DriverCapabilityRegistration{Entity: binding.entity, Description: description})
		}
	}
	if err := h.catalog.WithDriverCapabilitiesBatch(registrations, func() error {
		if projection, ok := h.sink.(ProjectionSink); ok {
			return projection.AcceptDiscovery(ctx, declarations)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		s.bindings[binding.entity] = binding
		h.owners[binding.entity] = binding
	}
	return bindings, nil
}

func (s *Session) owned(binding Binding) error {
	if !s.live() {
		return ErrInactiveSession
	}
	if _, _, err := binding.IDs(); err != nil {
		return ErrForeignBinding
	}
	current, ok := s.bindings[binding.entity]
	if !ok || current != binding || s.host.owners[binding.entity] != binding {
		return ErrForeignBinding
	}
	return nil
}

func (s *Session) Publish(ctx context.Context, binding Binding, observation StateObservation) error {
	if s == nil || s.host == nil {
		return ErrInactiveSession
	}
	h := s.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := s.owned(binding); err != nil {
		return err
	}
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, _ := domain.NewSourceStamp(s.id, observation.sequence)
	if _, err := domain.NewStateReport(observation.quality, observation.value, observation.observedAt, observation.receivedAt, probe, 1); err != nil {
		return err
	}
	if observation.endpoint == "" {
		return ErrInvalidInput
	}
	var supported bool
	proofSchema := ""
	proofAmbiguous := false
	descriptions, err := h.catalog.Descriptions(binding.entity)
	if err != nil {
		return err
	}
	for _, description := range descriptions {
		if description.Endpoint != observation.endpoint {
			continue
		}
		matched := false
		if observation.value == nil {
			supported = true
			break
		}
		kind, _ := observation.value.Kind()
		switch kind {
		case domain.ValueSwitch:
			switch description.SchemaId {
			case contracts.SwitchSchema, contracts.BinarySchema, contracts.LightSchema, contracts.FanSchema:
				matched = true
			case contracts.MeasurementSchema, contracts.ClimateSchema:
				matched = false
			default:
				matched = false
			}
		case domain.ValueMeasurement:
			if description.SchemaId == contracts.MeasurementSchema {
				measurement, ok := observation.value.Measurement()
				if !ok {
					return ErrInvalidInput
				}
				if err := h.catalog.AdmitDomainMeasurement(binding.entity, observation.endpoint, measurement); err != nil {
					return err
				}
				matched = true
			}
		case domain.ValueUnspecified:
			return ErrInvalidInput
		default:
			return ErrInvalidInput
		}
		if matched {
			supported = true
			if proofSchema == "" {
				proofSchema = description.SchemaId
			} else if proofSchema != description.SchemaId {
				proofAmbiguous = true
			}
		}
	}
	if !supported {
		return ErrInvalidInput
	}
	if _, _, err := observation.causality.Components(); err != nil {
		return err
	}
	accepted := AcceptedState{observation: observation, source: probe, instance: s.instance}
	if receipts, ok := h.sink.(ActionReceiptSink); ok {
		receipt, err := receipts.AcceptStateWithActionReceipt(ctx, binding.entity, accepted)
		if err != nil {
			return err
		}
		// The sequencer lock is released. Host still fences session ownership.
		if !proofAmbiguous {
			h.actions.observe(binding, observation.endpoint, proofSchema, receipt)
		}
		return nil
	}
	return h.sink.AcceptState(ctx, binding.entity, accepted)
}

func (s *Session) Emit(ctx context.Context, binding Binding, event DeviceEvent) error {
	if s == nil || s.host == nil {
		return ErrInactiveSession
	}
	h := s.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := s.owned(binding); err != nil {
		return err
	}
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, _, err := event.Components(); err != nil {
		return err
	}
	source, err := domain.NewSourceStamp(s.id, event.sequence)
	if err != nil {
		return err
	}
	if _, _, err := event.causality.Components(); err != nil {
		return err
	}
	return h.sink.AcceptEvent(ctx, binding.entity, AcceptedEvent{Event: event, Source: source, instance: s.instance})
}

func (s *Session) ReportHealth(ctx context.Context, report HealthReport) error {
	if s == nil || s.host == nil {
		return ErrInactiveSession
	}
	h := s.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.live() {
		return ErrInactiveSession
	}
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := report.Components(); err != nil {
		return err
	}
	if _, _, err := report.causality.Components(); err != nil {
		return err
	}
	report.source, _ = domain.NewSourceStamp(s.id, nil)
	return h.sink.AcceptHealth(ctx, s.instance, report)
}
