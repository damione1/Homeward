package integrations

import (
	"context"
	"errors"
	"maps"
	"sort"

	"github.com/damione1/Homeward/internal/domain"
)

var ErrConfigurationDeclarationsChanged = errors.New("integration declarations changed during configuration")

const configurationWithdrawalLimit = 16384

// Nonzero-sized allocations have distinct identity while any Discover keeps
// the previous token alive. A token is never reused or cleared at Release:
// even an entire reservation inside Resolve invalidates its old result.
type configurationDeclarationToken struct{ marker byte }

// ConfigurationDeclarationsLease reserves declaration mutations only. It holds
// no mutex across compilation or SQL. State, health and technical retirement
// fences remain live; Confirm rejects a retired identity before SQL commits.
// The activation owner retains this lease for an unknown COMMIT and releases
// it before joining pilots, which may themselves be waiting in Discover.
type ConfigurationDeclarationsLease struct {
	host                      *Manager
	done                      chan struct{}
	owners                    map[domain.EntityID]Binding
	retiring, retiringSources int
	withdrawals               map[domain.EntityID]struct{}
	invalid, released         bool
	releaseErr                error
}

func (h *Manager) ReserveConfigurationDeclarations(ctx context.Context) (*ConfigurationDeclarationsLease, error) {
	if h == nil || h.active == nil || h.catalog == nil || nilInterface(ctx) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.admission != admissionOpen || h.retirementExhausted {
		return nil, ErrAdmissionsClosed
	}
	if h.configurationDeclarations != nil {
		return nil, ErrSupervisorState
	}
	if h.configurationDeclarationsClosed == nil {
		h.configurationDeclarationsClosed = make(chan struct{})
	}
	lease := &ConfigurationDeclarationsLease{host: h, done: make(chan struct{}),
		owners: maps.Clone(h.owners), retiring: len(h.retiring), retiringSources: len(h.retiringSources),
		withdrawals: make(map[domain.EntityID]struct{})}
	h.configurationDeclarationsToken = &configurationDeclarationToken{marker: 1}
	h.configurationDeclarations = lease
	return lease, nil
}

// Confirm does no I/O and never waits on a discovery callback. The same Host
// mutex that protects the final discovery mutation protects this confirmation.
func (l *ConfigurationDeclarationsLease) Confirm(ctx context.Context) error {
	if l == nil || l.host == nil || nilInterface(ctx) {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h := l.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if l.released || h.configurationDeclarations != l {
		return ErrSupervisorState
	}
	if h.admission != admissionOpen || h.retirementExhausted || l.invalid ||
		len(l.withdrawals) != 0 || len(h.retiring) != l.retiring || len(h.retiringSources) != l.retiringSources ||
		!maps.Equal(l.owners, h.owners) {
		return ErrConfigurationDeclarationsChanged
	}
	for entity, binding := range l.owners {
		if _, fenced := h.retiring[entity]; fenced {
			return ErrConfigurationDeclarationsChanged
		}
		if _, fenced := h.retiringSources[retiringSource{binding.instance, binding.key}]; fenced {
			return ErrConfigurationDeclarationsChanged
		}
		session := h.active[binding.instance]
		if session == nil || !session.live() || session.bindings[entity] != binding {
			return ErrConfigurationDeclarationsChanged
		}
	}
	return ctx.Err()
}

// Release flushes every deferred projection withdrawal before waking Discover.
// A failed flush is returned and invalidates the existing projection; callers
// must keep command admissions quarantined. Repeated Release returns that same
// result. No pilot, registry resolver or SQL operation runs under this mutex.
func (l *ConfigurationDeclarationsLease) Release() error {
	if l == nil || l.host == nil {
		return ErrInvalidInput
	}
	h := l.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if l.released {
		return l.releaseErr
	}
	if h.configurationDeclarations != l {
		return ErrSupervisorState
	}
	entities := make([]domain.EntityID, 0, len(l.withdrawals))
	for entity := range l.withdrawals {
		entities = append(entities, entity)
	}
	sort.Slice(entities, func(i, j int) bool {
		left, _ := entities[i].Value()
		right, _ := entities[j].Value()
		return left < right
	})
	var failures []error
	if l.invalid {
		failures = append(failures, ErrConfigurationDeclarationsChanged)
	}
	for _, entity := range entities {
		if err := h.withdrawConfigurationDeclarationLocked(context.Background(), entity); err != nil {
			failures = append(failures, err)
		}
	}
	l.releaseErr = errors.Join(failures...)
	if l.releaseErr != nil {
		if projection, ok := h.sink.(ProjectionSink); ok {
			projection.InvalidateRegistry()
		}
	}
	l.released, h.configurationDeclarations = true, nil
	close(l.done)
	return l.releaseErr
}

// Never called from a catalogue callback. A discovery waits on this lease
// only after releasing Host.mu, then re-resolves SQL and
// revalidates its live session, source fences and identity before mutation.
func (s *Session) waitConfigurationDeclarations(ctx context.Context) error {
	if s == nil || s.host == nil || nilInterface(ctx) {
		return ErrInvalidInput
	}
	h := s.host
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.mu.Lock()
		if !s.live() {
			h.mu.Unlock()
			return ErrInactiveSession
		}
		if h.admission != admissionOpen || h.retirementExhausted {
			h.mu.Unlock()
			return ErrAdmissionsClosed
		}
		lease := h.configurationDeclarations
		if lease == nil {
			h.mu.Unlock()
			return nil
		}
		done, closed, sessionDone := lease.done, h.configurationDeclarationsClosed, s.done
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sessionDone:
			return ErrInactiveSession
		case <-closed:
			return ErrAdmissionsClosed
		case <-done:
		}
	}
}

func (h *Manager) closeConfigurationDeclarationsLocked() {
	if h.configurationDeclarationsClosed != nil {
		select {
		case <-h.configurationDeclarationsClosed:
		default:
			close(h.configurationDeclarationsClosed)
		}
	}
}

func (h *Manager) withdrawConfigurationDeclarationLocked(ctx context.Context, entity domain.EntityID) error {
	return h.catalog.WithdrawDriverCapabilities(entity, func() error {
		if projection, ok := h.sink.(DiscoveryWithdrawalSink); ok {
			return projection.WithdrawDiscovery(ctx, entity)
		}
		return nil
	})
}

func (h *Manager) deferConfigurationWithdrawalLocked(entity domain.EntityID) (bool, error) {
	lease := h.configurationDeclarations
	if lease == nil {
		return false, nil
	}
	if _, exists := lease.withdrawals[entity]; exists {
		return true, nil
	}
	if len(lease.withdrawals) == configurationWithdrawalLimit {
		lease.invalid = true
		h.retirementExhausted = true
		h.closeConfigurationDeclarationsLocked()
		return true, ErrConfigurationDeclarationsChanged
	}
	lease.withdrawals[entity] = struct{}{}
	return true, nil
}
