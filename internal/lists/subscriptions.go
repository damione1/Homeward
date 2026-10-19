package lists

import (
	"context"
	"crypto/sha256"
	"reflect"
	"sync"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

type Change struct {
	list     domain.ListID
	revision ListRevision
}

func (c Change) ListID() domain.ListID  { return c.list }
func (c Change) Revision() ListRevision { return c.revision }

type Subscription struct {
	service    *Service
	permit     *authorization.Permit
	authSub    *authorization.Subscription
	list       domain.ListID
	collection bool
	selection  authorization.ListSelection
	rights     uint64
	semantic   [sha256.Size]byte
	queue      chan Change
	done       chan struct{}
	mu         sync.Mutex
	terminal   bool
	closed     bool
	once       sync.Once
}

func (s *Service) subscribe(ctx context.Context, id domain.ListID, collection bool) (*Subscription, error) {
	if ctx == nil {
		return nil, failure(Invalid)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, failure(Denied)
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	permission, err := listPermission(authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, failure(Invalid)
	}
	object := authorization.ListCollectionObject()
	if !collection {
		object, err = listObject(id)
		if err != nil {
			return nil, failure(Invalid)
		}
	}
	permit, err := s.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, failure(Denied)
	}
	evidence, selection, err := permit.ListEvidence()
	if err != nil {
		permit.Release()
		return nil, failure(Denied)
	}
	rights, semantic, err := evidence.Binding()
	if err != nil {
		permit.Release()
		return nil, failure(Denied)
	}
	authSub, err := s.authority.Subscribe(ctx, principal)
	if err != nil {
		permit.Release()
		return nil, failure(Capacity)
	}
	if !permit.Ready() {
		authSub.Close()
		permit.Release()
		return nil, failure(Denied)
	}
	sub := &Subscription{service: s, permit: permit, authSub: authSub, list: id, collection: collection, selection: selection, rights: rights, semantic: semantic, queue: make(chan Change, MaxNotificationQueue), done: make(chan struct{})}
	s.mu.Lock()
	if s.closed || len(s.subs) >= MaxListSubscriptions {
		closed := s.closed
		s.mu.Unlock()
		authSub.Close()
		permit.Release()
		if closed {
			return nil, failure(Closed)
		}
		return nil, failure(Capacity)
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	return sub, nil
}
func (s *Service) SubscribeList(ctx context.Context, id domain.ListID) (*Subscription, error) {
	return s.subscribe(ctx, id, false)
}
func (s *Service) SubscribeCatalog(ctx context.Context) (*Subscription, error) {
	return s.subscribe(ctx, domain.ListID{}, true)
}

func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		close(s.done)
		s.terminal = true
		s.closed = true
		s.mu.Unlock()
		if s.service != nil {
			s.service.mu.Lock()
			delete(s.service.subs, s)
			s.service.mu.Unlock()
		}
		s.releaseLease()
	})
}
func (s *Subscription) releaseLease() {
	if s.authSub != nil {
		s.authSub.Close()
	}
	if s.permit != nil {
		s.permit.Release()
	}
}
func (s *Subscription) allowed(id domain.ListID) bool {
	if !s.collection {
		return s.list == id
	}
	if s.selection.All() {
		return true
	}
	ids, err := s.selection.IDs()
	if err != nil {
		return false
	}
	for _, allowed := range ids {
		if allowed == id {
			return true
		}
	}
	return false
}
func (s *Subscription) enqueue(change Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal || !s.allowed(change.list) {
		return
	}
	select {
	case s.queue <- change:
	default:
		s.terminal = true
	}
}
func (s *Subscription) resync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return
	}
	s.terminal = true
	select {
	case s.queue <- Change{}:
	default:
	}
}
func (s *Service) publish(id domain.ListID, revision int64) {
	rev, err := NewListRevision(id, revision)
	if err != nil {
		return
	}
	change := Change{list: id, revision: rev}
	s.mu.Lock()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.enqueue(change)
	}
}
func (s *Service) resyncList(id domain.ListID) {
	s.mu.Lock()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		if sub.allowed(id) {
			sub.resync()
		}
	}
}
func (s *Service) resyncCatalogAll() {
	s.mu.Lock()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		if sub.collection && sub.selection.All() {
			sub.resync()
		}
	}
}
func (s *Subscription) Next(ctx context.Context) (Change, error) {
	if s == nil || !validContext(ctx) || s.permit == nil || s.authSub == nil {
		return Change{}, failure(Invalid)
	}
	for {
		s.mu.Lock()
		terminal := s.terminal
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return Change{}, failure(Closed)
		}
		// Keep the lease until Close, even after overflow or commit-unknown.
		// This lets a revoked caller receive Denied without disclosing activity.
		if !s.permit.Ready() {
			return Change{}, failure(Denied)
		}
		if terminal {
			return Change{}, failure(ResyncRequired)
		}
		select {
		case <-ctx.Done():
			return Change{}, ctx.Err()
		case <-s.done:
			return Change{}, failure(Closed)
		case <-s.permit.Context().Done():
			return Change{}, failure(Denied)
		case <-s.authSub.Changes():
			if !s.permit.Ready() {
				return Change{}, failure(Denied)
			}
			return Change{}, failure(ResyncRequired)
		case change := <-s.queue:
			s.mu.Lock()
			terminal = s.terminal
			closed = s.closed
			s.mu.Unlock()
			if closed {
				return Change{}, failure(Closed)
			}
			if !s.permit.Ready() {
				return Change{}, failure(Denied)
			}
			if terminal {
				return Change{}, failure(ResyncRequired)
			}
			if !s.permit.Ready() {
				return Change{}, failure(Denied)
			}
			evidence, selection, err := s.permit.ListEvidence()
			if err != nil {
				return Change{}, failure(Denied)
			}
			rights, semantic, err := evidence.Binding()
			if err != nil || rights != s.rights || semantic != s.semantic {
				return Change{}, failure(ResyncRequired)
			}
			if s.collection {
				if selection.All() != s.selection.All() {
					return Change{}, failure(ResyncRequired)
				}
				ids, _ := selection.IDs()
				prior, _ := s.selection.IDs()
				if len(ids) != len(prior) {
					return Change{}, failure(ResyncRequired)
				}
				for i := range ids {
					if ids[i] != prior[i] {
						return Change{}, failure(ResyncRequired)
					}
				}
			}
			if !s.allowed(change.list) {
				continue
			}
			return change, nil
		}
	}
}
func validContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}
