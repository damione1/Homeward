package mediasessions

import (
	"context"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"reflect"
	"sync"
	"time"
)

type Clock interface{ Now() time.Time }
type Service struct {
	store      *storage.Store
	authority  *authorization.Authority
	clock      Clock
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	closed     bool
	active     int
	wait       sync.WaitGroup
	operations map[ID]map[*Operation]struct{}
	videos     map[domain.ActorID]*Operation
}

func nilClock(clock Clock) bool {
	if clock == nil {
		return true
	}
	value := reflect.ValueOf(clock)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
func NewService(store *storage.Store, authority *authorization.Authority, clock Clock) (*Service, error) {
	if store == nil || authority == nil || nilClock(clock) || clock.Now().IsZero() {
		return nil, failure(Invalid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{store: store, authority: authority, clock: clock, ctx: ctx, cancel: cancel, operations: map[ID]map[*Operation]struct{}{}, videos: map[domain.ActorID]*Operation{}}, nil
}
func (s *Service) enter() error {
	if s == nil || s.store == nil || s.authority == nil || s.ctx == nil || s.cancel == nil || nilClock(s.clock) {
		return failure(Invalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return failure(Closed)
	}
	if s.active >= MaxConcurrentCalls {
		return failure(ActiveCapacity)
	}
	s.active++
	s.wait.Add(1)
	return nil
}
func (s *Service) leave() { s.mu.Lock(); s.active--; s.mu.Unlock(); s.wait.Done() }
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	for _, entries := range s.operations {
		for operation := range entries {
			operation.cancel()
		}
	}
	s.mu.Unlock()
	s.wait.Wait()
}

type Admission struct {
	service    *Service
	permit     *authorization.Permit
	evidence   authorization.RightsEvidence
	actor      domain.ActorID
	credential [32]byte
	entity     domain.EntityID
	slot       authorization.CapabilitySlot
	cancel     context.CancelFunc
	stop       func() bool
	once       sync.Once
}

func (a *Admission) Context() context.Context {
	if a == nil || a.permit == nil {
		return nil
	}
	return a.permit.Context()
}
func (a *Admission) Ready() bool { return a != nil && a.permit != nil && a.permit.Ready() }
func (a *Admission) CredentialBinding() [32]byte {
	if a == nil {
		return [32]byte{}
	}
	return a.credential
}
func (a *Admission) Release() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if a.permit != nil {
			a.permit.Release()
		}
		if a.stop != nil {
			a.stop()
		}
		if a.cancel != nil {
			a.cancel()
		}
		if a.service != nil {
			a.service.leave()
		}
	})
}
func (s *Service) principal(ctx context.Context) (authorization.Principal, domain.ActorID, [32]byte, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return authorization.Principal{}, domain.ActorID{}, [32]byte{}, failure(Denied)
	}
	actor, err := principal.Actor()
	if err != nil {
		return authorization.Principal{}, domain.ActorID{}, [32]byte{}, failure(Denied)
	}
	current, err := s.authority.Current(principal)
	if err != nil {
		return authorization.Principal{}, domain.ActorID{}, [32]byte{}, failure(Denied)
	}
	_, binding, err := current.Binding()
	if err != nil {
		return authorization.Principal{}, domain.ActorID{}, [32]byte{}, failure(Denied)
	}
	return principal, actor, binding, nil
}

// AdmitSource obtains the exact permission already used by camera/artwork
// reads. It retains no Authority lock across SQL, encoding or source I/O.
func (s *Service) AdmitSource(ctx context.Context, entity domain.EntityID, slot authorization.CapabilitySlot) (*Admission, error) {
	if ctx == nil {
		return nil, failure(Invalid)
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	principal, actor, binding, err := s.principal(ctx)
	if err != nil {
		s.leave()
		return nil, err
	}
	object, err := authorization.EntityObject(entity, slot)
	if err != nil {
		s.leave()
		return nil, failure(Denied)
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		s.leave()
		return nil, failure(Denied)
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	permit, err := s.authority.Permit(call, principal, permission, object)
	if err != nil {
		stop()
		cancel()
		s.leave()
		return nil, failure(Denied)
	}
	evidence, err := permit.MediaEvidence()
	if err != nil {
		permit.Release()
		stop()
		cancel()
		s.leave()
		return nil, failure(Denied)
	}
	// The caller binding was captured before acquiring the retained permit.
	// Reject a policy/session transition between those two admissions.
	_, _, admittedBinding, bindingErr := s.principal(permit.Context())
	if bindingErr != nil || admittedBinding != binding || !permit.Ready() {
		permit.Release()
		stop()
		cancel()
		s.leave()
		return nil, failure(Denied)
	}
	return &Admission{service: s, permit: permit, evidence: evidence, actor: actor, credential: binding, entity: entity, slot: slot, cancel: cancel, stop: stop}, nil
}
func (s *Service) validAdmission(admission *Admission) bool {
	return s != nil && admission != nil && admission.service == s && admission.Ready() && admission.credential != ([32]byte{})
}

// Locator contains only the identity needed to acquire the original source's
// exact current permit. It never exposes artwork references or content metadata.
// Foreign/unknown session IDs are indistinguishable under the owned-credential
// SQL predicate; no content operation can use a Locator without Admission.
type Locator struct {
	id         ID
	actor      domain.ActorID
	credential [32]byte
	entity     domain.EntityID
	slot       authorization.CapabilitySlot
	kind       Kind
}

func (l Locator) ID() ID                             { return l.id }
func (l Locator) Entity() domain.EntityID            { return l.entity }
func (l Locator) Slot() authorization.CapabilitySlot { return l.slot }
func (l Locator) Kind() Kind                         { return l.kind }
func (s *Service) LookupOwned(ctx context.Context, id ID) (Locator, error) {
	if !id.Valid() || ctx == nil {
		return Locator{}, failure(Invalid)
	}
	if err := s.enter(); err != nil {
		return Locator{}, err
	}
	defer s.leave()
	_, actor, binding, err := s.principal(ctx)
	if err != nil {
		return Locator{}, err
	}
	return s.lookup(ctx, id, actor, binding)
}

// Operation is only a cancellation/join registration. SQL remains the sole
// owner of session identity, lifetime, source binding and retirement state.
type Operation struct {
	service    *Service
	id         ID
	ctx        context.Context
	cancel     context.CancelFunc
	stop       func() bool
	done       chan struct{}
	videoActor domain.ActorID
	video      bool
	once       sync.Once
}

func (s *Service) BeginOperation(ctx context.Context, id ID) (*Operation, error) {
	if s == nil || s.ctx == nil || ctx == nil || !id.Valid() {
		return nil, failure(Invalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, failure(Closed)
	}
	if len(s.operations) >= MaxActiveGlobal {
		return nil, failure(ActiveCapacity)
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	operation := &Operation{service: s, id: id, ctx: child, cancel: cancel, stop: stop, done: make(chan struct{})}
	entries := s.operations[id]
	if entries == nil {
		entries = map[*Operation]struct{}{}
		s.operations[id] = entries
	}
	if len(entries) >= 16 {
		stop()
		cancel()
		return nil, failure(ActiveCapacity)
	}
	entries[operation] = struct{}{}
	s.wait.Add(1)
	return operation, nil
}
func (o *Operation) Context() context.Context {
	if o == nil {
		return nil
	}
	return o.ctx
}
func (o *Operation) Close() {
	if o == nil {
		return
	}
	o.once.Do(func() {
		o.cancel()
		o.stop()
		s := o.service
		s.mu.Lock()
		if o.video && s.videos[o.videoActor] == o {
			delete(s.videos, o.videoActor)
		}
		entries := s.operations[o.id]
		delete(entries, o)
		if len(entries) == 0 {
			delete(s.operations, o.id)
		}
		close(o.done)
		s.mu.Unlock()
		s.wait.Done()
	})
}
func (s *Service) cancelOperations(ctx context.Context, id ID) error {
	s.mu.Lock()
	var operations []*Operation
	for operation := range s.operations[id] {
		operation.cancel()
		operations = append(operations, operation)
	}
	s.mu.Unlock()
	for _, operation := range operations {
		select {
		case <-operation.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ClaimVideo reserves only active execution, after BeginOperation registered
// cancellation and Get authenticated the SQL-owned Record. It carries no lease
// metadata or permission and is released synchronously by Operation.Close.
func (s *Service) ClaimVideo(operation *Operation, record Record) error {
	if s == nil || operation == nil || operation.service != s || !record.valid() || operation.id != record.id || record.binding.kind != Camera {
		return failure(Invalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || operation.ctx.Err() != nil {
		return failure(Closed)
	}
	if _, exists := s.operations[operation.id][operation]; !exists {
		return failure(Closed)
	}
	if operation.video || s.videos[record.actor] != nil || len(s.videos) >= 8 {
		return failure(ActiveCapacity)
	}
	operation.video = true
	operation.videoActor = record.actor
	s.videos[record.actor] = operation
	return nil
}
