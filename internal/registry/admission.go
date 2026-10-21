package registry

import (
	"context"
	"reflect"
	"sync/atomic"

	"github.com/damione1/Homeward/internal/storage"
)

// The admission token is separate from Repository.mu, which protects only
// short in-memory reads/writes. It remains owned across SQL and projection so
// an ordinary rename/discovery cannot slip between activation and publication.
func (r *Repository) acquire(ctx context.Context) error {
	if r == nil || r.admission == nil || nilContext(ctx) {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-r.admission:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Repository) release() { r.admission <- struct{}{} }

func nilContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// ActivationGate is a single-owner reservation. Quarantine retains this same
// gate until SQL outcome and projection are known; releasing it cannot be used
// as evidence that a commit did or did not happen.
type ActivationGate struct {
	repo  *Repository
	phase atomic.Uint32 // activationPhase; zero is invalid
}

type activationPhase uint32

const (
	activationInvalid activationPhase = iota
	activationHeld
	activationApplying
	activationPending
	activationPublishing
	activationResolved
	activationReleased
)

func (r *Repository) BeginActivation(ctx context.Context) (*ActivationGate, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	dirty, attached := r.projectionState()
	if dirty || !attached {
		r.release()
		return nil, ErrProjectionUnavailable
	}
	gate := &ActivationGate{repo: r}
	gate.phase.Store(uint32(activationHeld))
	return gate, nil
}

func (g *ActivationGate) Release() error {
	if g == nil || g.repo == nil {
		return ErrInvalidInput
	}
	for {
		phase := activationPhase(g.phase.Load())
		switch phase {
		case activationHeld, activationResolved:
			if !g.phase.CompareAndSwap(uint32(phase), uint32(activationReleased)) {
				continue
			}
			g.repo.release()
			return nil
		case activationApplying, activationPending, activationPublishing:
			return ErrProjectionUnavailable
		case activationInvalid, activationReleased:
			return ErrInvalidInput
		default:
			return ErrInvalidInput
		}
	}
}

// ResolveNotCommitted releases the pending projection obligation only when
// the caller has a definite non-commit from the exact ConfigTx attempt.
func (g *ActivationGate) ResolveNotCommitted(outcome storage.TransactionOutcome) error {
	if g == nil || g.repo == nil || outcome != storage.TransactionNotCommitted ||
		!g.phase.CompareAndSwap(uint32(activationPending), uint32(activationResolved)) {
		return ErrInvalidInput
	}
	return nil
}

func (r *Repository) projectionState() (bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projectionDirty, r.projection != nil
}
