package server

import (
	"context"
	"errors"
	"sync"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

// The transport owns lifetime only. Native owners create and revoke every
// object permit; no principal, permission or execution authority is minted here.
const maxPublicResponseLeases = 1024

type publicResponseScopeKey struct{}
type publicResponseLease struct {
	stop    func() bool
	done    chan struct{}
	release func()
}
type publicResponseScope struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelCauseFunc
	interrupt func()
	leases    []publicResponseLease
	closed    bool
}

func newPublicResponseScope(ctx context.Context, interrupt func()) (context.Context, *publicResponseScope) {
	lifetime, cancel := context.WithCancelCause(ctx)
	scope := &publicResponseScope{ctx: lifetime, cancel: cancel, interrupt: interrupt}
	return context.WithValue(lifetime, publicResponseScopeKey{}, scope), scope
}

// retainPublicResponse transfers cleanup only on (true,nil). A private direct
// owner call has no HTTP response scope and keeps its normal local cleanup.
// Call after materialization and final native admission, before returning the
// response. nativeCtx must outlive handler return, including its cancel owner.
func retainPublicResponse(ctx, nativeCtx context.Context, ready func() bool, release func()) (bool, error) {
	if ctx == nil {
		return false, authorization.ErrDenied
	}
	scope, ok := ctx.Value(publicResponseScopeKey{}).(*publicResponseScope)
	if !ok {
		return false, nil
	}
	if nativeCtx == nil || ready == nil || release == nil || !ready() {
		return false, authorization.ErrDenied
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed || scope.ctx.Err() != nil || nativeCtx.Err() != nil {
		return false, authorization.ErrDenied
	}
	if len(scope.leases) >= maxPublicResponseLeases {
		return false, authorization.ErrCapacity
	}
	done := make(chan struct{})
	stop := context.AfterFunc(nativeCtx, func() {
		defer close(done)
		scope.invalidate(context.Cause(nativeCtx))
	})
	scope.leases = append(scope.leases, publicResponseLease{stop: stop, done: done, release: release})
	return true, nil
}

func (scope *publicResponseScope) invalidate(cause error) {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return
	}
	scope.mu.Unlock()
	// Preserve native deadline/client cancellation. Native policy revocation
	// becomes the existing transport termination vocabulary, not a new permit.
	reason := auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED
	var termination authorizationTerminationCause
	switch {
	case errors.As(cause, &termination):
		reason = termination.reason
	case errors.Is(cause, authorization.ErrUnavailable):
		reason = auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		scope.cancel(cause)
		if scope.interrupt != nil {
			scope.interrupt()
		}
		return
	}
	scope.cancel(authorizationTerminationCause{reason: reason})
	if scope.interrupt != nil {
		scope.interrupt()
	}
}

func (scope *publicResponseScope) close() {
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return
	}
	scope.closed = true
	leases := scope.leases
	scope.leases = nil
	scope.mu.Unlock()
	// Stop/join callbacks before releasing permits: normal owner Release often
	// cancels its context and must not be mistaken for rights withdrawal.
	for _, lease := range leases {
		if !lease.stop() {
			<-lease.done
		}
	}
	for _, lease := range leases {
		lease.release()
	}
	scope.cancel(context.Canceled)
}
