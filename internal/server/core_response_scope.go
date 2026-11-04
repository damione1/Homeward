package server

import (
	"context"
	"sync"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/view"
)

// Subscribe before materialization. The native Authority owns invalidation;
// this adapter transfers lifetime only, including the final HTTP flush.
func (h *homeResourceHandler) beginReadResponse(ctx context.Context) (context.Context, func(), error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return ctx, nil, err
	}
	changes, err := h.authority.Subscribe(ctx, principal)
	if err != nil {
		return ctx, nil, err
	}
	lifetime, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-lifetime.Done():
		case <-changes.Changes():
			cancel(authorization.ErrDenied)
		}
	}()
	var once sync.Once
	release := func() { once.Do(func() { cancel(context.Canceled); <-done; changes.Close() }) }
	ready := func() bool {
		if lifetime.Err() != nil {
			return false
		}
		_, err := h.authority.Current(principal)
		return err == nil
	}
	retained, err := retainPublicResponse(ctx, lifetime, ready, release)
	if err != nil {
		release()
		return ctx, nil, err
	}
	return lifetime, func() {
		if !retained {
			release()
		}
	}, nil
}

// Existing pagination storage stays owned by view.Service. Its cleanup must
// not retire the capture while Connect is still serializing/sending the page.
func releaseReadResponse(ctx context.Context, release func()) {
	retained, err := retainPublicResponse(ctx, ctx, func() bool { return ctx.Err() == nil }, release)
	if err != nil || !retained {
		release()
	}
}

func (h *homeResourceHandler) retainReadCapture(ctx context.Context, token string) error {
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return err
	}
	lifetime, close, err := h.views.ResponseContext(ctx, actor, token)
	if err != nil {
		return err
	}
	ready := func() bool {
		if lifetime.Err() != nil {
			return false
		}
		_, err := h.views.ReadFirstPage(lifetime, actor, token, 1)
		return err == nil
	}
	retained, err := retainPublicResponse(ctx, lifetime, ready, close)
	if err != nil || !retained {
		close()
	}
	if err == nil && ctx.Err() != nil {
		return view.ErrViewChanged
	}
	return err
}
