package calendars

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
	"sync"
)

type ChangeKind uint8

const (
	ChangeInvalid ChangeKind = iota
	CalendarCreated
	CalendarUpdated
	CalendarDeleted
	EventCreated
	EventUpdated
	EventDeleted
)

type Change struct {
	kind     ChangeKind
	calendar CalendarID
	event    EventKey
	revision Revision
}

func (c Change) Kind() ChangeKind     { return c.kind }
func (c Change) Calendar() CalendarID { return c.calendar }
func (c Change) Event() (EventKey, bool) {
	return c.event, c.kind == EventCreated || c.kind == EventUpdated || c.kind == EventDeleted
}
func (c Change) Revision() Revision { return c.revision }

// Service owns operation lifetimes and bounded internal notifications. It has
// no background worker, HTTP listener, source connection or action executor.
// Mutations serialize through one cancelable slot until post-COMMIT publish.
type Service struct {
	repo          *Repository
	mu            sync.Mutex
	closed        bool
	calls         map[*operation]struct{}
	wg            sync.WaitGroup
	mutation      chan struct{}
	subscriptions map[*Subscription]struct{}
}
type operation struct{ cancel context.CancelFunc }

func NewService(repo *Repository) (*Service, error) {
	if repo == nil || repo.store == nil {
		return nil, failure(InvalidInput)
	}
	return &Service{repo: repo, calls: make(map[*operation]struct{}), mutation: make(chan struct{}, 1), subscriptions: make(map[*Subscription]struct{})}, nil
}
func (s *Service) begin(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.repo == nil {
		return nil, nil, failure(InvalidInput)
	}
	if err := contextValid(ctx); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, failure(Closed)
	}
	if len(s.calls) >= 64 {
		return nil, nil, failure(Busy)
	}
	bounded, cancel := context.WithCancel(ctx)
	call := &operation{cancel: cancel}
	s.calls[call] = struct{}{}
	s.wg.Add(1)
	return bounded, func() { cancel(); s.mu.Lock(); delete(s.calls, call); s.mu.Unlock(); s.wg.Done() }, nil
}
func serviceCall[T any](s *Service, ctx context.Context, mutation bool, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	bounded, done, err := s.begin(ctx)
	if err != nil {
		return zero, err
	}
	defer done()
	if mutation {
		select {
		case s.mutation <- struct{}{}:
			defer func() { <-s.mutation }()
		case <-bounded.Done():
			return zero, &Error{code: Canceled, cause: bounded.Err()}
		}
	}
	return fn(bounded)
}
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	for call := range s.calls {
		call.cancel()
	}
	s.mu.Unlock()
	// begin cannot Add after closed is set. Every synchronous expansion/SQL
	// operation must actually return before subscriptions and Close complete.
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subscriptions {
		s.finishSubscription(sub, Closed)
	}
}
func (s *Service) Bind(ctx context.Context, entity domain.EntityID) (CalendarID, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) (CalendarID, error) { return s.repo.Bind(ctx, entity) })
}
func (s *Service) GetCalendar(ctx context.Context, id CalendarID) (CalendarSnapshot, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) (CalendarSnapshot, error) { return s.repo.GetCalendar(ctx, id) })
}
func (s *Service) ListCalendars(ctx context.Context) ([]CalendarSnapshot, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) ([]CalendarSnapshot, error) { return s.repo.ListCalendars(ctx) })
}
func (s *Service) GetEvent(ctx context.Context, key EventKey) (EventSnapshot, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) (EventSnapshot, error) { return s.repo.GetEvent(ctx, key) })
}
func (s *Service) Query(ctx context.Context, id CalendarID, w Window, limit int, cursor Cursor) (Page, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) (Page, error) { return s.repo.Query(ctx, id, w, limit, cursor) })
}
func (s *Service) calendarChange(ctx context.Context, id CalendarID, expected Revision, name string, mutation calendarMutation, kind ChangeKind) (CalendarSnapshot, error) {
	return serviceCall(s, ctx, true, func(ctx context.Context) (CalendarSnapshot, error) {
		snapshot, err := s.repo.mutateCalendar(ctx, id, expected, name, mutation)
		if err == nil {
			s.publish(Change{kind: kind, calendar: id, revision: snapshot.revision})
		}
		return snapshot, err
	})
}
func (s *Service) CreateCalendar(ctx context.Context, id CalendarID, expected Revision, name string) (CalendarSnapshot, error) {
	return s.calendarChange(ctx, id, expected, name, calendarCreate, CalendarCreated)
}
func (s *Service) UpdateCalendar(ctx context.Context, id CalendarID, expected Revision, name string) (CalendarSnapshot, error) {
	return s.calendarChange(ctx, id, expected, name, calendarUpdate, CalendarUpdated)
}
func (s *Service) DeleteCalendar(ctx context.Context, id CalendarID, expected Revision) (CalendarSnapshot, error) {
	return s.calendarChange(ctx, id, expected, "", calendarDelete, CalendarDeleted)
}
func (s *Service) eventChange(ctx context.Context, key EventKey, expected Revision, event Event, mutation eventMutation, kind ChangeKind) (EventSnapshot, error) {
	return serviceCall(s, ctx, true, func(ctx context.Context) (EventSnapshot, error) {
		snapshot, err := s.repo.mutateEvent(ctx, key, expected, event, mutation)
		if err == nil {
			s.publish(Change{kind: kind, calendar: key.calendar, event: key, revision: snapshot.calendarRevision})
		}
		return snapshot, err
	})
}
func (s *Service) CreateEvent(ctx context.Context, key EventKey, expected Revision, event Event) (EventSnapshot, error) {
	return s.eventChange(ctx, key, expected, event, eventCreate, EventCreated)
}
func (s *Service) UpdateEvent(ctx context.Context, key EventKey, expected Revision, event Event) (EventSnapshot, error) {
	return s.eventChange(ctx, key, expected, event, eventUpdate, EventUpdated)
}
func (s *Service) DeleteEvent(ctx context.Context, key EventKey, expected Revision) (EventSnapshot, error) {
	return s.eventChange(ctx, key, expected, Event{}, eventDelete, EventDeleted)
}

const MaxSubscribers = 32
const NotificationCapacity = 16

type Subscription struct {
	owner    *Service
	queue    chan Change
	done     chan struct{}
	terminal ErrorCode
}

func (s *Service) Subscribe() (*Subscription, error) {
	if s == nil || s.repo == nil {
		return nil, failure(InvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, failure(Closed)
	}
	if len(s.subscriptions) >= MaxSubscribers {
		return nil, failure(CapacityExceeded)
	}
	sub := &Subscription{owner: s, queue: make(chan Change, NotificationCapacity), done: make(chan struct{})}
	s.subscriptions[sub] = struct{}{}
	return sub, nil
}

// Called under owner.mu. Overflow is terminal and observable; clients must
// acquire a fresh snapshot rather than treating dropped changes as success.
func (s *Service) finishSubscription(sub *Subscription, code ErrorCode) {
	if sub.terminal == ErrorInvalid {
		sub.terminal = code
		delete(s.subscriptions, sub)
		close(sub.done)
	}
}
func (s *Service) publish(change Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subscriptions {
		select {
		case sub.queue <- change:
		default:
			s.finishSubscription(sub, ResyncRequired)
		}
	}
}
func (sub *Subscription) Close() {
	if sub != nil && sub.owner != nil {
		sub.owner.mu.Lock()
		defer sub.owner.mu.Unlock()
		sub.owner.finishSubscription(sub, Closed)
	}
}
func (sub *Subscription) Next(ctx context.Context) (Change, error) {
	if sub == nil || sub.owner == nil {
		return Change{}, failure(InvalidInput)
	}
	if err := contextValid(ctx); err != nil {
		return Change{}, err
	}
	check := func() error {
		sub.owner.mu.Lock()
		defer sub.owner.mu.Unlock()
		if sub.terminal != ErrorInvalid {
			return failure(sub.terminal)
		}
		return nil
	}
	if err := check(); err != nil {
		return Change{}, err
	}
	select {
	case <-ctx.Done():
		return Change{}, &Error{code: Canceled, cause: ctx.Err()}
	case <-sub.done:
		if err := check(); err != nil {
			return Change{}, err
		}
		return Change{}, failure(Closed)
	case change := <-sub.queue:
		if err := check(); err != nil {
			return Change{}, err
		}
		return change, nil
	}
}

func (s *Service) ListEvents(ctx context.Context, id CalendarID) ([]EventSnapshot, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) ([]EventSnapshot, error) { return s.repo.ListEvents(ctx, id) })
}
