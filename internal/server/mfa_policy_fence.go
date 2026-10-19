package server

import (
	"context"
	"sort"
	"sync"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/config"
)

type mfaPolicyFenceState uint8

const (
	mfaPolicyFenceInvalid mfaPolicyFenceState = iota
	mfaPolicyFenceOpen
	mfaPolicyFenceChanging
	mfaPolicyFenceUnknown
	mfaPolicyFenceClosed
)

// This fence orders transient admissions in the one server process. SQL and
// authorization.Authority remain authoritative. No SQL, crypto, user callback
// or drain wait runs under mu; tickets never span an admitted request/stream.
type mfaPolicyFence struct {
	mu       sync.Mutex
	state    mfaPolicyFenceState
	version  authmfa.ExpectedPolicyVersion
	limit    int
	active   int
	changed  chan struct{}
	current  *mfaPolicyChange
	targets  []contracts.AccountID
	done     chan struct{}
	doneOnce sync.Once
}

type mfaPolicyTicket struct {
	*mfaPolicyTicketState
}

type mfaPolicyTicketState struct {
	fence *mfaPolicyFence
	once  sync.Once
}

type mfaPolicyChangeKind uint8

const (
	mfaPolicyChangeInvalid mfaPolicyChangeKind = iota
	mfaPolicyChangeReplace
	mfaPolicyChangeReconcile
)

// The pointer is the ownership token. A stale change cannot reopen a newer
// fence or publish an unverified policy. The bounded context is shared with
// the following SQL and publication, rather than granting each phase 5s anew.
type mfaPolicyChange struct {
	fence  *mfaPolicyFence
	kind   mfaPolicyChangeKind
	ctx    context.Context
	cancel context.CancelFunc
}

func newMFAPolicyFence(version authmfa.ExpectedPolicyVersion, settings config.PublicSettings) (*mfaPolicyFence, error) {
	if _, _, err := version.Values(); err != nil || settings.MaxConcurrentRequests() <= 0 {
		return nil, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	return &mfaPolicyFence{state: mfaPolicyFenceOpen, version: version,
		limit: settings.MaxConcurrentRequests(), changed: make(chan struct{}), done: make(chan struct{})}, nil
}

func (f *mfaPolicyFence) signalLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *mfaPolicyFence) completeDrainLocked() {
	if f.state == mfaPolicyFenceClosed && f.active == 0 && f.current == nil {
		f.doneOnce.Do(func() { close(f.done) })
	}
}

// tryEnter never waits. An admin bearer admission can call it while its
// account gate is held, immediately before registering the short-lived lease.
func (f *mfaPolicyFence) tryEnter(expected authmfa.ExpectedPolicyVersion) (*mfaPolicyTicket, error) {
	if f == nil {
		return nil, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	if _, _, err := expected.Values(); err != nil {
		return nil, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case mfaPolicyFenceOpen:
		if f.version != expected {
			return nil, authmfa.NewError(authmfa.ErrorConflict)
		}
		if f.active >= f.limit {
			return nil, authmfa.NewError(authmfa.ErrorBusy)
		}
		f.active++
		return &mfaPolicyTicket{&mfaPolicyTicketState{fence: f}}, nil
	case mfaPolicyFenceChanging:
		return nil, authmfa.NewError(authmfa.ErrorBusy)
	case mfaPolicyFenceUnknown, mfaPolicyFenceClosed, mfaPolicyFenceInvalid:
		return nil, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	return nil, authmfa.NewError(authmfa.ErrorIntegrity)
}

func (t *mfaPolicyTicket) release() {
	if t == nil || t.mfaPolicyTicketState == nil || t.fence == nil {
		return
	}
	t.once.Do(func() {
		f := t.fence
		f.mu.Lock()
		f.active--
		f.signalLocked()
		f.completeDrainLocked()
		f.mu.Unlock()
	})
}

func validMFAPolicyContext(ctx context.Context) bool {
	return ctx != nil && !typedNilPublic(ctx) && ctx.Err() == nil
}

func (f *mfaPolicyFence) beginReplace(ctx context.Context, expected authmfa.ExpectedPolicyVersion) (*mfaPolicyChange, error) {
	if f == nil || !validMFAPolicyContext(ctx) {
		return nil, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if _, _, err := expected.Values(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	switch f.state {
	case mfaPolicyFenceOpen:
		if f.version != expected {
			f.mu.Unlock()
			return nil, authmfa.NewError(authmfa.ErrorConflict)
		}
	case mfaPolicyFenceChanging:
		f.mu.Unlock()
		return nil, authmfa.NewError(authmfa.ErrorBusy)
	case mfaPolicyFenceUnknown, mfaPolicyFenceClosed, mfaPolicyFenceInvalid:
		f.mu.Unlock()
		return nil, authmfa.NewError(authmfa.ErrorUnavailable)
	default:
		f.mu.Unlock()
		return nil, authmfa.NewError(authmfa.ErrorIntegrity)
	}
	bounded, cancel := context.WithTimeout(ctx, authmfa.PolicyMutationTimeout)
	change := &mfaPolicyChange{fence: f, kind: mfaPolicyChangeReplace, ctx: bounded, cancel: cancel}
	f.state, f.current = mfaPolicyFenceChanging, change
	f.signalLocked()
	f.mu.Unlock()
	for {
		f.mu.Lock()
		if f.state != mfaPolicyFenceChanging || f.current != change {
			f.mu.Unlock()
			_ = change.abandon()
			return nil, authmfa.NewError(authmfa.ErrorUnavailable)
		}
		if f.active == 0 {
			f.mu.Unlock()
			if err := bounded.Err(); err != nil {
				_ = change.rollback()
				return nil, err
			}
			return change, nil
		}
		wake := f.changed
		f.mu.Unlock()
		select {
		case <-wake:
		case <-bounded.Done():
			// No SQL has started: cancellation while draining is a known
			// rollback. It must not manufacture an uncertain DB mutation.
			_ = change.rollback()
			return nil, bounded.Err()
		}
	}
}

// beginReconcile elects one reader of primary state. It preserves the bounded
// cohort from the uncertain write and cannot be used as a second SetPolicy.
func (f *mfaPolicyFence) beginReconcile(ctx context.Context) (*mfaPolicyChange, []contracts.AccountID, error) {
	if f == nil || !validMFAPolicyContext(ctx) {
		return nil, nil, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case mfaPolicyFenceUnknown:
		bounded, cancel := context.WithTimeout(ctx, authmfa.PolicyMutationTimeout)
		change := &mfaPolicyChange{fence: f, kind: mfaPolicyChangeReconcile, ctx: bounded, cancel: cancel}
		f.state, f.current = mfaPolicyFenceChanging, change
		f.signalLocked()
		return change, append([]contracts.AccountID(nil), f.targets...), nil
	case mfaPolicyFenceChanging:
		return nil, nil, authmfa.NewError(authmfa.ErrorBusy)
	case mfaPolicyFenceOpen:
		return nil, nil, authmfa.NewError(authmfa.ErrorConflict)
	case mfaPolicyFenceClosed, mfaPolicyFenceInvalid:
		return nil, nil, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	return nil, nil, authmfa.NewError(authmfa.ErrorIntegrity)
}

func admittedMFAPolicyTargets(targets []contracts.AccountID) ([]contracts.AccountID, error) {
	if len(targets) > authmfa.PolicyAffectedAccountLimit {
		return nil, authmfa.NewError(authmfa.ErrorBusy)
	}
	owned := append([]contracts.AccountID(nil), targets...)
	for _, target := range owned {
		if _, err := target.Value(); err != nil {
			return nil, authmfa.NewError(authmfa.ErrorIntegrity)
		}
	}
	sort.Slice(owned, func(i, j int) bool { left, _ := owned[i].Value(); right, _ := owned[j].Value(); return left < right })
	for i := 1; i < len(owned); i++ {
		if owned[i] == owned[i-1] {
			return nil, authmfa.NewError(authmfa.ErrorIntegrity)
		}
	}
	return owned, nil
}

// setUnknown is called before releasing the change after an unknown commit or
// incomplete publication. Only the explicit, admitted cohort is retained.
func (c *mfaPolicyChange) setUnknown(targets []contracts.AccountID) error {
	owned, err := admittedMFAPolicyTargets(targets)
	if err != nil {
		_ = c.abandon() // do not reopen on an invalid internal result
		return err
	}
	return c.finish(mfaPolicyFenceUnknown, authmfa.ExpectedPolicyVersion{}, owned)
}

func (c *mfaPolicyChange) publish(version authmfa.ExpectedPolicyVersion) error {
	if c == nil || c.ctx == nil || c.fence == nil {
		return authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	c.fence.mu.Lock()
	current := c.fence.current == c
	c.fence.mu.Unlock()
	if !current {
		return authmfa.NewError(authmfa.ErrorConflict)
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if _, _, err := version.Values(); err != nil {
		return err
	}
	return c.finish(mfaPolicyFenceOpen, version, nil)
}

// rollback is valid only before SQL or after a confirmed not-committed result.
func (c *mfaPolicyChange) rollback() error {
	if c == nil || c.fence == nil || c.kind != mfaPolicyChangeReplace {
		return authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	f := c.fence
	f.mu.Lock()
	version := f.version
	f.mu.Unlock()
	return c.finish(mfaPolicyFenceOpen, version, nil)
}

// abandon keeps the current cohort and closes sensitive admissions when the
// owner exits without a confirmed resolution. Safe as a deferred cleanup.
func (c *mfaPolicyChange) abandon() error {
	return c.finish(mfaPolicyFenceUnknown, authmfa.ExpectedPolicyVersion{}, nil)
}

func (c *mfaPolicyChange) finish(next mfaPolicyFenceState, version authmfa.ExpectedPolicyVersion, targets []contracts.AccountID) error {
	if c == nil || c.fence == nil || c.cancel == nil {
		return authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	switch next {
	case mfaPolicyFenceOpen:
		if _, _, err := version.Values(); err != nil {
			return err
		}
	case mfaPolicyFenceUnknown:
	default:
		return authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	f := c.fence
	f.mu.Lock()
	if f.current != c {
		f.mu.Unlock()
		return authmfa.NewError(authmfa.ErrorConflict)
	}
	if f.state != mfaPolicyFenceChanging && f.state != mfaPolicyFenceClosed {
		f.mu.Unlock()
		return authmfa.NewError(authmfa.ErrorIntegrity)
	}
	if next == mfaPolicyFenceOpen {
		nextRevision, _, _ := version.Values()
		currentRevision, _, _ := f.version.Values()
		if nextRevision < currentRevision || nextRevision == currentRevision && version != f.version {
			f.mu.Unlock()
			return authmfa.NewError(authmfa.ErrorIntegrity)
		}
		f.version, f.targets = version, nil
	} else if targets != nil {
		f.targets = targets
	}
	if f.state != mfaPolicyFenceClosed {
		f.state = next
	}
	f.current = nil
	f.signalLocked()
	f.completeDrainLocked()
	f.mu.Unlock()
	c.cancel()
	return nil
}

func (f *mfaPolicyFence) closeAdmissions() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.state != mfaPolicyFenceInvalid && f.changed != nil {
		f.state = mfaPolicyFenceClosed
		f.signalLocked()
		f.completeDrainLocked()
	}
	f.mu.Unlock()
}

func (f *mfaPolicyFence) Done() <-chan struct{} {
	if f == nil {
		return nil
	}
	return f.done
}
