package server

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

const sessionCoordinatorAccountLimit = 16384

var errSessionCoordinatorBusy = errors.New("server: session admission capacity exhausted")

// sessionCoordinator is process-local ordering, not a second source of
// identity. Every admission still reads PostgreSQL while holding its account
// gate. The single Go process serves public, MCP and operator transports.
type sessionCoordinator struct {
	store            *storage.Store
	clock            sessionClock
	mu               sync.Mutex
	accounts         map[string]*sessionAccountGate
	authority        *authorization.Authority
	pendingCreations list.List // private queue of account keys, protected by mu
}

// One clock owns both admission time and expiry cancellation. Tests can
// advance logical time without waiting for a wall-clock 15-minute deadline.
type sessionClock interface {
	Now() time.Time
	At(time.Time, func()) sessionTimer
}
type sessionTimer interface{ Stop() bool }
type realSessionClock struct{}

func (realSessionClock) Now() time.Time { return time.Now() }
func (realSessionClock) At(deadline time.Time, f func()) sessionTimer {
	return time.AfterFunc(time.Until(deadline), f)
}

type sessionAccountGate struct {
	account         contracts.AccountID
	permit          chan struct{}
	refs            int // holders and waiters; leases are counted separately
	quarantined     bool
	leases          map[*sessionLease]struct{}
	pendingCreation *list.Element
}

type sessionGuard struct {
	coordinator *sessionCoordinator
	key         string
	gate        *sessionAccountGate
	once        sync.Once
	mutation    *authorization.Mutation
	termination authv1.AuthorizationTerminationReason
	creating    bool
}

type sessionLease struct {
	owner       storage.SessionOwner
	gate        *sessionAccountGate
	coordinator *sessionCoordinator
	cancel      context.CancelFunc
	cancelCause context.CancelCauseFunc
	once        sync.Once
}

type sessionLeaseContextKey struct{}

func newSessionCoordinator(store *storage.Store) (*sessionCoordinator, error) {
	return newSessionCoordinatorWithClock(store, realSessionClock{})
}

func newSessionCoordinatorWithClock(store *storage.Store, clock sessionClock) (*sessionCoordinator, error) {
	if store == nil || clock == nil || typedNilPublic(clock) {
		return nil, storage.ErrSessionInvalid
	}
	return &sessionCoordinator{store: store, clock: clock, accounts: make(map[string]*sessionAccountGate), authority: authorization.NewAuthority()}, nil
}

func (c *sessionCoordinator) enter(ctx context.Context, account contracts.AccountID) (*sessionGuard, error) {
	if c == nil || c.store == nil || ctx == nil {
		return nil, storage.ErrSessionInvalid
	}
	key, err := account.Value()
	if err != nil {
		return nil, storage.ErrSessionInvalid
	}
	c.mu.Lock()
	gate := c.accounts[key]
	if gate == nil {
		if len(c.accounts) >= sessionCoordinatorAccountLimit {
			c.mu.Unlock()
			return nil, errSessionCoordinatorBusy
		}
		gate = &sessionAccountGate{account: account, permit: make(chan struct{}, 1), leases: make(map[*sessionLease]struct{})}
		gate.permit <- struct{}{}
		c.accounts[key] = gate
	}
	gate.refs++
	c.mu.Unlock()
	select {
	case <-gate.permit:
	case <-ctx.Done():
		c.mu.Lock()
		gate.refs--
		c.evictIdleLocked(key, gate)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
	guard := &sessionGuard{coordinator: c, key: key, gate: gate}
	c.mu.Lock()
	quarantined := gate.quarantined
	c.mu.Unlock()
	if quarantined {
		if err := guard.reconcilePrimary(ctx); err != nil {
			guard.release()
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		guard.release()
		return nil, err
	}
	return guard, nil
}

func (c *sessionCoordinator) evictIdleLocked(key string, gate *sessionAccountGate) {
	if gate.refs == 0 && len(gate.leases) == 0 && !gate.quarantined && c.accounts[key] == gate {
		delete(c.accounts, key)
	}
}

func (g *sessionGuard) release() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.mutation.Release()
		g.gate.permit <- struct{}{}
		c := g.coordinator
		c.mu.Lock()
		g.gate.refs--
		c.evictIdleLocked(g.key, g.gate)
		c.mu.Unlock()
	})
}

func (g *sessionGuard) quarantine() {
	if g == nil {
		return
	}
	g.coordinator.mu.Lock()
	g.gate.quarantined = true
	if g.creating && g.gate.pendingCreation == nil {
		g.gate.pendingCreation = g.coordinator.pendingCreations.PushBack(g.key)
	}
	g.coordinator.mu.Unlock()
	g.termination = authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE
	_ = g.coordinator.authority.QuarantineAccount(g.gate.account)
}

// register must run while the gate is held, after authoritative admission.
// The derived context closes a stream immediately on local revocation.
func (g *sessionGuard) register(ctx context.Context, owner storage.SessionOwner) (context.Context, *sessionLease) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &sessionLease{owner: owner, gate: g.gate, coordinator: g.coordinator, cancel: func() { cancel(context.Canceled) }, cancelCause: cancel}
	g.coordinator.mu.Lock()
	g.gate.leases[lease] = struct{}{}
	g.coordinator.mu.Unlock()
	return context.WithValue(leaseCtx, sessionLeaseContextKey{}, lease), lease
}

func (l *sessionLease) terminate(reason authv1.AuthorizationTerminationReason) {
	if l == nil || l.cancelCause == nil {
		return
	}
	l.cancelCause(authorizationTerminationCause{reason: reason})
}

func (l *sessionLease) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.cancel()
		c := l.coordinator
		c.mu.Lock()
		delete(l.gate.leases, l)
		key, _ := l.gate.account.Value()
		c.evictIdleLocked(key, l.gate)
		c.mu.Unlock()
	})
}

// cancelLeases runs only after a confirmed revoking commit or when its outcome
// is unknown and the account is quarantined. It never waits for handlers under
// the account gate. A confirmed rollback leaves existing leases untouched.
func (g *sessionGuard) cancelLeases(ctx context.Context, session *contracts.SessionID) {
	var self *sessionLease
	if ctx != nil {
		self, _ = ctx.Value(sessionLeaseContextKey{}).(*sessionLease)
	}
	var selected []*sessionLease
	g.coordinator.mu.Lock()
	for lease := range g.gate.leases {
		if lease == self {
			continue
		}
		if session != nil && lease.owner.Session != *session {
			continue
		}
		selected = append(selected, lease)
	}
	g.coordinator.mu.Unlock()
	reason := g.termination
	if reason == authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_UNSPECIFIED {
		reason = authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED
	}
	for _, lease := range selected {
		lease.terminate(reason)
	}
}
