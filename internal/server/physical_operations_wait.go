package server

import (
	"context"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

const (
	// waitOperationCeiling bounds one standard wait; a client re-polls with
	// its own backoff. waitOperationSlice bounds one live wait before the
	// durable record is read again, since a durable terminal appended after
	// the confirmation window does not change the live tracker.
	waitOperationCeiling = 10 * time.Second
	waitOperationSlice   = time.Second
)

// WaitOperation is a native read-only wait on a physical command. It parks on
// the live action progression and re-reads the durable record when the
// progression changes or a slice elapses. It never redispatches.
func (h *coreRPCHandler) WaitOperation(ctx context.Context, request *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	if h == nil || request == nil {
		return nil, authorization.ErrDenied
	}
	name, err := apitypes.ParseOperationName(request.Name)
	if err != nil {
		return nil, authorization.ErrDenied
	}
	budget := waitOperationCeiling
	if request.Timeout != nil && request.Timeout.CheckValid() == nil && request.Timeout.AsDuration() < budget {
		budget = request.Timeout.AsDuration()
	}
	deadline := time.Now().Add(budget)
	for {
		operation, err := h.GetOperation(ctx, name)
		if err != nil || operation.Done {
			return operation, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return operation, nil
		}
		if !h.awaitPhysicalChange(ctx, name, min(remaining, waitOperationSlice)) {
			return operation, nil
		}
	}
}

// awaitPhysicalChange returns once the live progression changes or the slice
// elapses. False means the caller's context ended. Without a live
// progression (a restarted host) the slice alone paces the durable re-read.
func (h *coreRPCHandler) awaitPhysicalChange(ctx context.Context, name apitypes.OperationName, slice time.Duration) bool {
	timer := time.NewTimer(slice)
	defer timer.Stop()
	id, err := domain.ParseCommandID(strings.TrimPrefix(name.ID(), "cmd-"))
	if err != nil || h.commands == nil {
		return h.awaitTimer(ctx, timer.C)
	}
	read, err := h.commands.AuthorizeActionRead(ctx, id)
	if err != nil {
		return h.awaitTimer(ctx, timer.C)
	}
	defer read.Close()
	subscription, err := read.Subscribe()
	if err != nil {
		return h.awaitTimer(ctx, timer.C)
	}
	changed := make(chan struct{})
	go func() {
		defer close(changed)
		// The first delivery is the current snapshot; the second is a change,
		// a deadline expiry, or the close below.
		if _, err := subscription.Next(); err != nil {
			return
		}
		_, _ = subscription.Next()
	}()
	select {
	case <-changed:
		return true
	case <-timer.C:
		subscription.Close()
		<-changed
		return true
	case <-ctx.Done():
		subscription.Close()
		<-changed
		return false
	}
}

func (h *coreRPCHandler) awaitTimer(ctx context.Context, elapsed <-chan time.Time) bool {
	select {
	case <-elapsed:
		return true
	case <-ctx.Done():
		return false
	}
}
