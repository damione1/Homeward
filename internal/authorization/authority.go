package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"sort"
	"sync"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
)

const MaxPermits = 4096
const MaxSubscriptions = 1024

type accountEntry struct {
	barrier     sync.RWMutex
	state       AccountState
	quarantined bool
	permits     map[*Permit]struct{}
	subscribers map[*Subscription]struct{}
	removed     bool
}

// Authority is the memory projection of committed account/policy facts. SQL
// hydration and reconciliation run outside this package and outside Host locks.
// Publish must be called under the same account gate used by authentication
// and account mutations. It publishes state before signaling subscribers.
type Authority struct {
	mu            sync.RWMutex
	accounts      map[string]*accountEntry
	bytes         int
	reservedBytes int
	reservations  map[string]*policyReservation
	permits       int
	subscriptions int
}

func NewAuthority() *Authority {
	return &Authority{accounts: make(map[string]*accountEntry), reservations: make(map[string]*policyReservation)}
}

// Mutation holds one account's memory admissions across SQL and its known or
// unknown commit outcome. Callers already hold the shared account gate. They
// publish or quarantine before Release, and never acquire this under Host.
type Mutation struct {
	authority *Authority
	entry     *accountEntry
	key       string
	released  bool // protected by authority.mu
	once      sync.Once
}

func (a *Authority) BeginMutation(account contracts.AccountID) (*Mutation, error) {
	if a == nil {
		return nil, ErrInvalid
	}
	key, err := account.Value()
	if err != nil {
		return nil, ErrInvalid
	}
	a.mu.RLock()
	entry := a.accounts[key]
	a.mu.RUnlock()
	if entry != nil {
		entry.barrier.Lock()
	}
	return &Mutation{authority: a, entry: entry, key: key}, nil
}
func (m *Mutation) Release() {
	if m == nil || m.authority == nil {
		return
	}
	m.once.Do(func() {
		m.authority.mu.Lock()
		m.released = true
		if reservation := m.authority.reservations[m.key]; reservation != nil && reservation.owner == m && !reservation.quarantined {
			m.authority.releaseReservationLocked(m.key)
		}
		m.authority.mu.Unlock()
		if m.entry != nil {
			m.entry.barrier.Unlock()
		}
	})
}
func (a *Authority) readEntry(key string) (*accountEntry, func()) {
	a.mu.RLock()
	entry := a.accounts[key]
	a.mu.RUnlock()
	if entry == nil {
		return nil, func() {}
	}
	entry.barrier.RLock()
	return entry, entry.barrier.RUnlock
}

// Hydrate installs a complete bounded projection before any admissions open.
// A partially validated set is never published.
func (a *Authority) Hydrate(states []AccountState) error {
	return a.HydrateFrom(func(yield func(AccountState) error) error {
		for _, state := range states {
			if err := yield(state); err != nil {
				return err
			}
		}
		return nil
	})
}

// HydrateFrom lets a trusted repository stream bounded pages without retaining
// every exact source byte. Only the complete, successfully read projection is
// published, including the loader's final transaction outcome.
func (a *Authority) HydrateFrom(load func(func(AccountState) error) error) error {
	if a == nil || load == nil {
		return ErrInvalid
	}
	entries := make(map[string]*accountEntry)
	cost := 0
	err := load(func(state AccountState) error {
		id, err := state.Account()
		if err != nil {
			return err
		}
		key, _ := id.Value()
		if _, duplicate := entries[key]; duplicate {
			return ErrInvalid
		}
		cost += state.policy.cost
		if cost > MaxAuthorityBytes {
			return ErrCapacity
		}
		entries[key] = newAccountEntry(state)
		return nil
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.permits != 0 || a.subscriptions != 0 || len(a.reservations) != 0 {
		return ErrInvalid
	}
	a.accounts, a.bytes = entries, cost
	return nil
}

func newAccountEntry(state AccountState) *accountEntry {
	return &accountEntry{state: state, permits: make(map[*Permit]struct{}), subscribers: make(map[*Subscription]struct{})}
}

func (a *Authority) Publish(state AccountState) error {
	if a == nil {
		return ErrInvalid
	}
	id, err := state.Account()
	if err != nil {
		return err
	}
	key, _ := id.Value()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accounts == nil {
		return ErrInvalid
	}
	entry := a.accounts[key]
	oldCost := 0
	if entry != nil {
		oldCost = entry.state.policy.cost
		if state.revocation < entry.state.revocation || state.version.revision < entry.state.version.revision ||
			(state.version.revision == entry.state.version.revision && (state.version.hash != entry.state.version.hash || state.policy.semantic != entry.state.policy.semantic)) {
			return ErrConflict
		}
	}
	reserved := a.reservedBytes
	if reservation := a.reservations[key]; reservation != nil {
		reserved -= reservation.growth
	}
	if state.policy.cost > MaxAuthorityBytes-a.bytes-reserved+oldCost {
		return ErrCapacity
	}
	if entry != nil && entry.removed {
		return ErrUnavailable
	}
	a.releaseReservationLocked(key)
	if entry == nil {
		entry = newAccountEntry(state)
		a.accounts[key] = entry
		a.bytes += state.policy.cost
		return nil
	}
	changed := entry.quarantined || entry.state.revocation != state.revocation || entry.state.role != state.role || entry.state.status != state.status || entry.state.policy.semantic != state.policy.semantic || entry.state.rightsRevision != state.rightsRevision
	previousRevocation := entry.state.revocation
	a.bytes += state.policy.cost - oldCost
	entry.state, entry.quarantined = state, false
	if !changed {
		return nil
	}
	for permit := range entry.permits {
		if previousRevocation != state.revocation || Evaluate(permit.principal, state, permit.permission, permit.object) != nil {
			permit.revoked = true
			permit.cancel(ErrDenied)
		}
	}
	for sub := range entry.subscribers {
		sub.signal()
	}
	return nil
}

func (a *Authority) Quarantine(principal Principal) error {
	if a == nil || !principal.valid() {
		return ErrInvalid
	}
	return a.QuarantineAccount(principal.account)
}
func (a *Authority) QuarantineAccount(account contracts.AccountID) error {
	if a == nil {
		return ErrInvalid
	}
	key, err := account.Value()
	if err != nil {
		return ErrInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if reservation := a.reservations[key]; reservation != nil {
		reservation.quarantined = true
	}
	entry := a.accounts[key]
	if entry == nil {
		return ErrUnavailable
	}
	entry.quarantined = true
	for permit := range entry.permits {
		permit.revoked = true
		permit.cancel(ErrUnavailable)
	}
	for sub := range entry.subscribers {
		sub.signal()
	}
	return nil
}

// RemoveAccount closes existing grants before forgetting a deleted account.
// Active consumers keep a closed tombstone only until their normal release.
func (a *Authority) RemoveAccount(account contracts.AccountID) error {
	if a == nil {
		return ErrInvalid
	}
	key, err := account.Value()
	if err != nil {
		return ErrInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseReservationLocked(key)
	entry := a.accounts[key]
	if entry == nil {
		return nil
	}
	entry.removed, entry.quarantined = true, true
	for permit := range entry.permits {
		permit.revoked = true
		permit.cancel(ErrDenied)
	}
	for sub := range entry.subscribers {
		sub.signal()
	}
	// A closed tombstone retains only its barrier and consumers. Keeping its
	// compiled policy after releasing its budget would defeat the memory bound.
	a.bytes -= entry.state.policy.cost
	entry.state = AccountState{}
	a.evictRemoved(key, entry)
	return nil
}
func (a *Authority) evictRemoved(key string, entry *accountEntry) {
	if entry.removed && len(entry.permits) == 0 && len(entry.subscribers) == 0 {
		delete(a.accounts, key)
	}
}

// Current is an immutable effective-account snapshot bound to one principal.
// Binding includes the credential ceiling even if visible EntityIDs coincide.
type Current struct {
	principal Principal
	state     AccountState
	binding   [sha256.Size]byte
}

func (a *Authority) Current(principal Principal) (Current, error) {
	if a == nil || !principal.valid() {
		return Current{}, ErrDenied
	}
	key, _ := principal.account.Value()
	_, release := a.readEntry(key)
	defer release()
	a.mu.RLock()
	defer a.mu.RUnlock()
	entry := a.accounts[key]
	if entry == nil || entry.quarantined {
		return Current{}, ErrUnavailable
	}
	if entry.state.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return Current{}, ErrDenied
	}
	return Current{principal, entry.state, currentBinding(principal, entry.state)}, nil
}

// WithCurrent admits a bounded memory-only read against one committed policy.
// Its callback may acquire generation/Host/catalogue/sequencer locks, in that
// order. It must never perform SQL, network I/O, or call this authority again.
func (a *Authority) WithCurrent(ctx context.Context, principal Principal, fn func(Current) error) error {
	if a == nil || nilContext(ctx) || !principal.valid() || fn == nil {
		return ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key, _ := principal.account.Value()
	_, release := a.readEntry(key)
	defer release()
	a.mu.RLock()
	defer a.mu.RUnlock()
	entry := a.accounts[key]
	if entry == nil || entry.quarantined {
		return ErrUnavailable
	}
	if entry.state.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return ErrDenied
	}
	return fn(Current{principal, entry.state, currentBinding(principal, entry.state)})
}
func (c Current) Authorize(permission Permission, object Object) error {
	if c.binding == ([sha256.Size]byte{}) {
		return ErrDenied
	}
	return Evaluate(c.principal, c.state, permission, object)
}

// AdmitRoute is only the coarse registered-procedure ceiling. Handlers still
// evaluate each actual object; an authorized empty view can return no objects.
func (c Current) AdmitRoute(permission Permission) error {
	if c.binding == ([sha256.Size]byte{}) || c.state.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE || !c.principal.permits(permission) {
		return ErrDenied
	}
	if _, _, err := permission.Values(); err != nil {
		return ErrDenied
	}
	if configurable(permission) != objectInvalid {
		if c.state.role == authv1.AccountRole_ACCOUNT_ROLE_USER && permission.family == authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS && permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER {
			return ErrDenied
		}
		return nil
	}
	object, err := AdministrativeObject(permission.family)
	if err != nil {
		return ErrDenied
	}
	if !intrinsic(c.state.role, permission, object) {
		return ErrDenied
	}
	return nil
}
func (c Current) Binding() (uint64, [sha256.Size]byte, error) {
	if c.binding == ([sha256.Size]byte{}) {
		return 0, [sha256.Size]byte{}, ErrInvalid
	}
	return c.state.rightsRevision, c.binding, nil
}

func currentBinding(principal Principal, state AccountState) [sha256.Size]byte {
	hash := sha256.New()
	write := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write(value)
	}
	write([]byte("homeward-authorization-binding-v1"))
	account, _ := state.account.Value()
	write([]byte(account))
	write([]byte(principal.credential))
	write([]byte{byte(principal.kind)})
	var number [8]byte
	for _, value := range []uint64{state.revocation, state.rightsRevision, uint64(state.role), uint64(state.status)} {
		binary.BigEndian.PutUint64(number[:], value)
		write(number[:])
	}
	write(state.policy.semantic[:])
	permissions := scopeValues(principal.scopes)
	sort.Slice(permissions, func(i, j int) bool {
		if permissions[i].family != permissions[j].family {
			return permissions[i].family < permissions[j].family
		}
		return permissions[i].operation < permissions[j].operation
	})
	for _, permission := range permissions {
		binary.BigEndian.PutUint64(number[:], uint64(permission.family))
		write(number[:])
		binary.BigEndian.PutUint64(number[:], uint64(permission.operation))
		write(number[:])
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

type Subscription struct {
	authority *Authority
	account   string
	changes   chan struct{}
	once      sync.Once
}

func (s *Subscription) Changes() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.changes
}
func (s *Subscription) signal() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}
func (s *Subscription) Close() {
	if s == nil || s.authority == nil {
		return
	}
	s.once.Do(func() {
		a := s.authority
		a.mu.Lock()
		defer a.mu.Unlock()
		if entry := a.accounts[s.account]; entry != nil {
			delete(entry.subscribers, s)
			a.subscriptions--
			a.evictRemoved(s.account, entry)
		}
	})
}
func (a *Authority) Subscribe(ctx context.Context, principal Principal) (*Subscription, error) {
	if a == nil || nilContext(ctx) || !principal.valid() {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, _ := principal.account.Value()
	_, release := a.readEntry(key)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.accounts[key]
	if entry == nil || entry.quarantined {
		return nil, ErrUnavailable
	}
	if a.subscriptions >= MaxSubscriptions {
		return nil, ErrCapacity
	}
	sub := &Subscription{authority: a, account: key, changes: make(chan struct{}, 1)}
	entry.subscribers[sub] = struct{}{}
	a.subscriptions++
	return sub, nil
}

// Permit is checked again at final admission and immediately before delayed
// transport emission. Once revoked it never becomes valid after regranting.
type Permit struct {
	authority  *Authority
	account    string
	entry      *accountEntry
	principal  Principal
	permission Permission
	object     Object
	ctx        context.Context
	cancel     context.CancelCauseFunc
	revoked    bool // protected by authority.mu
	released   bool // protected by authority.mu
}

func (a *Authority) Permit(ctx context.Context, principal Principal, permission Permission, object Object) (*Permit, error) {
	if a == nil || nilContext(ctx) || !principal.valid() {
		return nil, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, _ := principal.account.Value()
	_, release := a.readEntry(key)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.accounts[key]
	if entry == nil || entry.quarantined {
		return nil, ErrUnavailable
	}
	if err := Evaluate(principal, entry.state, permission, object); err != nil {
		return nil, err
	}
	if a.permits >= MaxPermits {
		return nil, ErrCapacity
	}
	permitCtx, cancel := context.WithCancelCause(ctx)
	permit := &Permit{authority: a, account: key, entry: entry, principal: principal, permission: permission, object: object, ctx: permitCtx, cancel: cancel}
	entry.permits[permit] = struct{}{}
	a.permits++
	return permit, nil
}
func (p *Permit) Context() context.Context {
	if p == nil {
		return nil
	}
	return p.ctx
}
func (p *Permit) Ready() bool {
	if p == nil || p.authority == nil || p.entry == nil {
		return false
	}
	a := p.authority
	p.entry.barrier.RLock()
	defer p.entry.barrier.RUnlock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	return p.readyLocked()
}
func (p *Permit) readyLocked() bool {
	if p.revoked || p.released || p.ctx == nil || p.ctx.Err() != nil {
		return false
	}
	entry := p.authority.accounts[p.account]
	return entry != nil && !entry.quarantined && Evaluate(p.principal, entry.state, p.permission, p.object) == nil
}

// WithCurrent linearizes a bounded memory-only admission callback against
// Publish. It must not perform SQL, network, or driver I/O. Lock order is
// authorization -> generation -> Host -> catalogue -> sequencer.
func (p *Permit) WithCurrent(fn func() error) error {
	if p == nil || p.authority == nil || p.entry == nil || fn == nil {
		return ErrDenied
	}
	a := p.authority
	p.entry.barrier.RLock()
	defer p.entry.barrier.RUnlock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !p.readyLocked() {
		return ErrDenied
	}
	return fn()
}
func (p *Permit) Release() {
	if p == nil || p.authority == nil {
		return
	}
	a := p.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.released {
		return
	}
	p.released = true
	p.cancel(context.Canceled)
	if entry := a.accounts[p.account]; entry != nil {
		if _, exists := entry.permits[p]; exists {
			delete(entry.permits, p)
			a.permits--
			a.evictRemoved(p.account, entry)
		}
	}
}

func nilContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
