package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

type CloudRenewalKind uint8

const (
	CloudRenewalUnspecified CloudRenewalKind = iota
	CloudRenewalLeader
	CloudRenewalJoined
)

// CloudCredential is a session-bound observation of one encrypted revision.
// The caller passes this same observation when it decides the token expired;
// a concurrent rotation then returns the newer value without another refresh.
type CloudCredential struct {
	handle   storage.SecretHandle
	value    storage.SecretValue
	revision storage.SecretRevision
	session  string
}

func (c CloudCredential) String() string               { return "[REDACTED CLOUD CREDENTIAL]" }
func (c CloudCredential) GoString() string             { return c.String() }
func (c CloudCredential) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }
func (c CloudCredential) Value() (storage.SecretValue, error) {
	raw, err := c.value.Bytes()
	if err != nil {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	return storage.NewSecretValue(raw)
}

func (c *CloudClient) ReadCredential(ctx context.Context, handle storage.SecretHandle) (CloudCredential, error) {
	if !c.valid() || ctx == nil {
		return CloudCredential{}, &CloudError{kind: CloudInvalid}
	}
	value, revision, err := c.secrets.readVersioned(ctx, handle)
	if err != nil {
		c.degrade(CloudCredentialFailed)
		return CloudCredential{}, c.credentialError(err)
	}
	return CloudCredential{handle: handle, value: value, revision: revision, session: c.session.id}, nil
}

// CloudRenewal provides either the exclusive lease with the old credential or
// the newly read credential after another caller completed the same refresh.
type CloudRenewal struct {
	kind  CloudRenewalKind
	value storage.SecretValue
	lease *CloudRenewalLease
}

func (r CloudRenewal) Kind() CloudRenewalKind { return r.kind }
func (r CloudRenewal) String() string         { return "[REDACTED CLOUD RENEWAL]" }
func (r CloudRenewal) GoString() string       { return r.String() }
func (r CloudRenewal) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

func (r CloudRenewal) Value() (storage.SecretValue, error) {
	if r.kind != CloudRenewalLeader && r.kind != CloudRenewalJoined {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	raw, err := r.value.Bytes()
	if err != nil {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	return storage.NewSecretValue(raw)
}

func (r CloudRenewal) Lease() (*CloudRenewalLease, error) {
	if r.kind != CloudRenewalLeader || r.lease == nil {
		return nil, &CloudError{kind: CloudInvalid}
	}
	return r.lease, nil
}

type cloudRefreshState struct {
	active        bool
	done          chan struct{}
	generation    uint64
	revision      storage.SecretRevision
	expiresAt     time.Time
	cooldownUntil time.Time
	blocked       bool
	completed     bool
	waiters       int
}

// CloudRenewalLease makes the remote effect and local CAS separate, explicit
// steps. Exchange sends at most once. Provider-specific decoding runs on the
// bounded response in the caller, without an unkillable callback goroutine.
type CloudRenewalLease struct {
	mu         sync.Mutex
	client     *CloudClient
	handle     storage.SecretHandle
	revision   storage.SecretRevision
	state      *cloudRefreshState
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	started    bool
	exchanged  bool
	entered    bool
	committing bool
	closed     bool
}

func (l *CloudRenewalLease) String() string   { return "[REDACTED CLOUD RENEWAL LEASE]" }
func (l *CloudRenewalLease) GoString() string { return l.String() }
func (l *CloudRenewalLease) MarshalJSON() ([]byte, error) {
	return json.Marshal(l.String())
}

func (c *CloudClient) BeginRenewal(ctx context.Context, observed CloudCredential) (CloudRenewal, error) {
	if !c.valid() || ctx == nil || c.policy.refresh == nil || observed.session == "" || observed.session != c.session.id {
		return CloudRenewal{}, &CloudError{kind: CloudInvalid}
	}
	handle := observed.handle
	value, revision, err := c.secrets.readVersioned(ctx, handle)
	if err != nil {
		c.degrade(CloudCredentialFailed)
		return CloudRenewal{}, c.credentialError(err)
	}
	return c.beginRenewalAfterRead(ctx, observed, value, revision)
}

// beginRenewalAfterRead is the admission boundary after the scoped SQL
// snapshot. Keeping it separate makes a delayed snapshot testable against a
// leader that commits before this caller reaches refreshMu.
func (c *CloudClient) beginRenewalAfterRead(ctx context.Context, observed CloudCredential, value storage.SecretValue, revision storage.SecretRevision) (CloudRenewal, error) {
	handle := observed.handle
	if revision != observed.revision {
		return CloudRenewal{kind: CloudRenewalJoined, value: value}, nil
	}
	p := c.policy
	p.refreshMu.Lock()
	state := p.refresh[handle]
	if state == nil {
		if len(p.refresh) >= 1024 {
			p.refreshMu.Unlock()
			return CloudRenewal{}, &CloudError{kind: CloudQueueFull}
		}
		state = &cloudRefreshState{}
		p.refresh[handle] = state
	}
	now := p.clock.Now()
	if state.active && !now.Before(state.expiresAt) {
		// An abandoned remote exchange is ambiguous. Do not launch another
		// one for the same stored credential merely because its lease timed out.
		state.active = false
		state.blocked = true
		state.cooldownUntil = now.Add(p.limits.value.RefreshCooldown)
		close(state.done)
	}
	// A caller may have read r1 before a concurrent leader durably changed it
	// to r2, then been delayed before taking refreshMu. A terminal success or
	// a state that has already advanced past its snapshot must reread before
	// it can ever send another remote refresh using r1.
	if (state.completed && state.revision == revision) || state.revision.NewerThan(revision) {
		p.refreshMu.Unlock()
		return c.joinAdvancedRenewal(ctx, handle, revision)
	}
	if state.active {
		if state.waiters >= p.limits.value.MaxRefreshWaiters {
			p.refreshMu.Unlock()
			return CloudRenewal{}, &CloudError{kind: CloudQueueFull}
		}
		state.waiters++
		done := state.done
		expiresAt := state.expiresAt
		generation := state.generation
		p.refreshMu.Unlock()
		return c.waitRenewal(ctx, handle, revision, state, done, expiresAt, generation)
	}
	if state.blocked && state.revision == revision {
		p.refreshMu.Unlock()
		return CloudRenewal{}, &CloudError{kind: CloudCredentialFailed}
	}
	if state.revision == revision && now.Before(state.cooldownUntil) {
		retryAt := state.cooldownUntil
		p.refreshMu.Unlock()
		return CloudRenewal{}, &CloudError{kind: CloudCredentialFailed, retryAt: retryAt}
	}
	state.active = true
	state.generation++
	state.done = make(chan struct{})
	state.revision = revision
	state.expiresAt = now.Add(p.limits.value.RefreshBudget)
	state.cooldownUntil = time.Time{}
	state.blocked = false
	state.completed = false
	generation := state.generation
	p.refreshMu.Unlock()
	budget, cancel := context.WithTimeout(ctx, p.limits.value.RefreshBudget)
	lease := &CloudRenewalLease{client: c, handle: handle, revision: revision, state: state, generation: generation, ctx: budget, cancel: cancel}
	return CloudRenewal{kind: CloudRenewalLeader, value: value, lease: lease}, nil
}

func (c *CloudClient) joinAdvancedRenewal(ctx context.Context, handle storage.SecretHandle, observed storage.SecretRevision) (CloudRenewal, error) {
	value, latest, err := c.secrets.readVersioned(ctx, handle)
	if err != nil {
		return CloudRenewal{}, c.credentialError(err)
	}
	if latest != observed {
		return CloudRenewal{kind: CloudRenewalJoined, value: value}, nil
	}
	// A reported durable success with the old revision still visible cannot
	// authorize another remote effect. The caller must reconcile externally.
	return CloudRenewal{}, &CloudError{kind: CloudCredentialFailed}
}

func (c *CloudClient) waitRenewal(ctx context.Context, handle storage.SecretHandle, observed storage.SecretRevision, state *cloudRefreshState, done <-chan struct{}, expiresAt time.Time, generation uint64) (CloudRenewal, error) {
	p := c.policy
	remaining := expiresAt.Sub(p.clock.Now())
	if remaining < 0 {
		remaining = 0
	}
	timer := p.timer.NewTimer(remaining)
	if nilInterface(timer) || timer.C() == nil {
		p.refreshMu.Lock()
		state.waiters--
		p.refreshMu.Unlock()
		return CloudRenewal{}, &CloudError{kind: CloudInvalid}
	}
	select {
	case <-done:
	case <-timer.C():
	case <-ctx.Done():
	case <-c.session.Done():
	}
	timer.Stop()
	p.refreshMu.Lock()
	state.waiters--
	if state.active && state.generation == generation && !p.clock.Now().Before(state.expiresAt) {
		state.active = false
		state.blocked = true
		state.cooldownUntil = p.clock.Now().Add(p.limits.value.RefreshCooldown)
		close(state.done)
	}
	blocked := state.blocked
	retryAt := state.cooldownUntil
	p.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return CloudRenewal{}, &CloudError{kind: CloudBudgetExhausted}
	}
	if !c.live() {
		return CloudRenewal{}, &CloudError{kind: CloudSessionInactive}
	}
	value, latest, err := c.secrets.readVersioned(ctx, handle)
	if err != nil {
		return CloudRenewal{}, c.credentialError(err)
	}
	if latest != observed {
		return CloudRenewal{kind: CloudRenewalJoined, value: value}, nil
	}
	if blocked {
		return CloudRenewal{}, &CloudError{kind: CloudCredentialFailed}
	}
	return CloudRenewal{}, &CloudError{kind: CloudCredentialFailed, retryAt: retryAt}
}

// Exchange sends exactly one renewal request. The caller builds it from
// CloudRenewal.Value using its provider contract and decodes only the bounded response.
// A lost response after entry is ambiguous even if the remote token rotated.
func (l *CloudRenewalLease) Exchange(request CloudRequest) (CloudResponse, error) {
	if l == nil || l.client == nil || !l.client.requestAllowed(request) {
		return CloudResponse{}, &CloudError{kind: CloudInvalid}
	}
	l.mu.Lock()
	if l.closed || l.started || l.ctx.Err() != nil {
		l.mu.Unlock()
		return CloudResponse{}, &CloudError{kind: CloudBudgetExhausted}
	}
	l.started = true
	l.mu.Unlock()
	entered := false
	response, _, err := l.client.send(l.ctx, request, l.admitted, &entered)
	l.mu.Lock()
	l.entered = entered
	l.exchanged = err == nil
	l.mu.Unlock()
	if err != nil {
		l.client.degrade(CloudCredentialFailed)
		l.finish(false, entered)
		return CloudResponse{}, l.client.credentialError(err)
	}
	return response, nil
}

func (l *CloudRenewalLease) admitted() bool {
	if l == nil || l.ctx.Err() != nil || !l.client.live() {
		return false
	}
	p := l.client.policy
	p.refreshMu.Lock()
	active := l.state.active && l.state.generation == l.generation
	p.refreshMu.Unlock()
	return active
}

func (l *CloudRenewalLease) Commit(value storage.SecretValue) error {
	if l == nil || l.client == nil {
		return &CloudError{kind: CloudInvalid}
	}
	l.mu.Lock()
	allowed := !l.closed && !l.committing && l.exchanged && l.ctx.Err() == nil
	if allowed {
		l.committing = true
	}
	l.mu.Unlock()
	if !allowed {
		return &CloudError{kind: CloudBudgetExhausted}
	}
	if !l.admitted() {
		l.finish(false, true)
		if !l.client.live() {
			return &CloudError{kind: CloudSessionInactive}
		}
		return &CloudError{kind: CloudBudgetExhausted}
	}
	err := l.client.secrets.rotateIfRevision(l.ctx, l.handle, l.revision, value)
	if err == nil {
		l.finish(true, false)
		return nil
	}
	cloudErr := l.client.credentialError(err)
	if cloudErr.Kind() == CloudCredentialConflict {
		// A newer manual rotation is already authoritative. Waiting callers
		// may reread it instead of issuing another remote exchange.
		l.finish(true, false)
	} else {
		l.finish(false, true)
	}
	l.client.degrade(cloudErr.Kind())
	return cloudErr
}

// Close releases an unfinished lease. A begun but unfinished remote exchange
// blocks further automatic refresh of that stored revision until it changes.
func (l *CloudRenewalLease) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	started := l.started
	entered := l.entered
	l.mu.Unlock()
	l.finish(false, started || entered)
}

func (l *CloudRenewalLease) finish(success, ambiguous bool) {
	if l == nil || l.client == nil {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	p := l.client.policy
	p.refreshMu.Lock()
	state := l.state
	if state.active && state.generation == l.generation {
		state.active = false
		state.blocked = ambiguous
		state.completed = success
		if !success {
			state.cooldownUntil = p.clock.Now().Add(p.limits.value.RefreshCooldown)
		}
		close(state.done)
	}
	p.refreshMu.Unlock()
}

func (c *CloudClient) credentialError(err error) *CloudError {
	if err == nil {
		return nil
	}
	if cloudErr, ok := err.(*CloudError); ok {
		return cloudErr
	}
	if errors.Is(err, ErrCredentialCommittedRetired) {
		return &CloudError{kind: CloudCredentialCommittedRetired}
	}
	if errors.Is(err, ErrCredentialCommittedUnobserved) {
		return &CloudError{kind: CloudCredentialCommittedUnobserved}
	}
	if errors.Is(err, ErrInactiveSession) || errors.Is(err, ErrForeignSecretHandle) {
		return &CloudError{kind: CloudSessionInactive}
	}
	var storageErr *storage.Error
	if errors.As(err, &storageErr) {
		switch storageErr.Kind() {
		case storage.SecretConflict:
			return &CloudError{kind: CloudCredentialConflict}
		case storage.SecretCommitUnknown:
			return &CloudError{kind: CloudCredentialCommitUnknown}
		}
	}
	return &CloudError{kind: CloudCredentialFailed}
}
