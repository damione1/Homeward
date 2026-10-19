package lists

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

const MaxConcurrentCalls = 64
const MaxListSubscriptions = 64
const MaxNotificationQueue = 16

type Service struct {
	repo             *Repository
	authority        *authorization.Authority
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	closed           bool
	closedDone       chan struct{}
	active           int
	wait             sync.WaitGroup
	subs             map[*Subscription]struct{}
	afterKnownCommit func()
}

func NewService(repo *Repository, authority *authorization.Authority) (*Service, error) {
	if repo == nil || repo.store == nil || authority == nil {
		return nil, failure(Invalid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{repo: repo, authority: authority, ctx: ctx, cancel: cancel, subs: make(map[*Subscription]struct{}), closedDone: make(chan struct{})}, nil
}
func (s *Service) enter() error {
	if s == nil {
		return failure(Invalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return failure(Closed)
	}
	if s.active >= MaxConcurrentCalls {
		return failure(Capacity)
	}
	s.active++
	s.wait.Add(1)
	return nil
}
func (s *Service) leave() { s.mu.Lock(); s.active--; s.mu.Unlock(); s.wait.Done() }
func (s *Service) Close() error {
	if s == nil {
		return failure(Invalid)
	}
	s.mu.Lock()
	if s.closed {
		done := s.closedDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.closed = true
	s.cancel()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.Close()
	}
	s.wait.Wait()
	close(s.closedDone)
	return nil
}

func listPermission(op authv1.ScopeOperation) (authorization.Permission, error) {
	return authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_LISTS, op)
}
func listObject(id domain.ListID) (authorization.Object, error) { return authorization.ListObject(id) }

// execute obtains a new real Permit and private proof for every call, then
// releases all Authority locks before the repository's account→policy SQL
// fence. No authority method is invoked until that transaction has ended.
func execute[T any](s *Service, ctx context.Context, op authv1.ScopeOperation, object authorization.Object, write bool,
	fn func(context.Context, pgx.Tx, authorization.RightsEvidence, authorization.ListSelection) (T, error),
	onCommit func(T)) (T, error) {
	return executeWithUnknown(s, ctx, op, object, write, fn, onCommit, nil)
}
func executeWithUnknown[T any](s *Service, ctx context.Context, op authv1.ScopeOperation, object authorization.Object, write bool,
	fn func(context.Context, pgx.Tx, authorization.RightsEvidence, authorization.ListSelection) (T, error),
	onCommit func(T), onUnknown func(T)) (T, error) {
	var zero T
	if ctx == nil || fn == nil {
		return zero, failure(Invalid)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return zero, failure(Denied)
	}
	if err := s.enter(); err != nil {
		return zero, err
	}
	defer s.leave()
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	permission, err := listPermission(op)
	if err != nil {
		return zero, failure(Invalid)
	}
	permit, err := s.authority.Permit(callCtx, principal, permission, object)
	if err != nil {
		return zero, failure(Denied)
	}
	defer permit.Release()
	evidence, selection, err := permit.ListEvidence()
	if err != nil {
		return zero, failure(Denied)
	}
	var value T
	err = s.repo.transaction(permit.Context(), evidence, func(txCtx context.Context, tx pgx.Tx) error {
		var operationErr error
		value, operationErr = fn(txCtx, tx, evidence, selection)
		return operationErr
	})
	if err != nil {
		if IsCode(err, CommitUnknown) && write && onUnknown != nil {
			onUnknown(value)
		}
		if !IsCode(err, CommitUnknown) && ctx.Err() == nil && s.ctx.Err() == nil && !permit.Ready() {
			return zero, failure(Denied)
		}
		return zero, err
	}
	if onCommit != nil {
		onCommit(value)
	}
	if s.afterKnownCommit != nil {
		s.afterKnownCommit()
	}
	if !permit.Ready() {
		if write {
			return zero, failure(CommittedResponseWithheld)
		}
		return zero, failure(Denied)
	}
	return value, nil
}

func freshID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", failure(Unavailable)
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}
func freshListID() (domain.ListID, error) {
	text, err := freshID("list-")
	if err != nil {
		return domain.ListID{}, err
	}
	return domain.ParseListID(text)
}
func freshItemUID() (ItemUID, error) {
	text, err := freshID("item-")
	if err != nil {
		return ItemUID{}, err
	}
	return ParseItemUID(text)
}
func wrapReadErr(err error) error {
	if err == nil {
		return nil
	}
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	return failure(Unavailable)
}
