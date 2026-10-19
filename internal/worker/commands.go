package worker

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	"github.com/damione1/Homeward/internal/integrations"
)

// Dispatch is called only by the Host's existing route/ledger. A transport
// completion proves that a worker accepted the message, not a physical action.
func (p *pilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if ctx == nil {
		return ErrUnavailable
	}
	id, _, entity, err := command.Identity()
	if err != nil {
		return err
	}
	idText, _ := id.Value()
	entityText, _ := entity.Value()
	request, err := command.Request()
	if err != nil {
		return err
	}
	if request.CommandId != idText {
		return ErrUnavailable
	}
	// Helper commands are native-only. Reject before creating a delivery or
	// touching the worker queue, including a mismatched helper wire branch.
	if request.CapabilitySchemaId == contracts.HelperSchema || request.GetArguments().GetHelper() != nil {
		return ErrUnavailable
	}
	cause, err := command.Causality()
	if err != nil {
		return err
	}
	_, correlationID, err := cause.Components()
	if err != nil || correlationID == nil {
		return ErrUnavailable
	}
	correlationText, err := correlationID.Value()
	if err != nil {
		return err
	}
	expires := time.Now().Add(2 * time.Minute)
	if request.ExpiresAt != nil {
		if err := request.ExpiresAt.CheckValid(); err != nil {
			return ErrUnavailable
		}
		if request.ExpiresAt.AsTime().Before(expires) {
			expires = request.ExpiresAt.AsTime()
		}
	}
	remaining := time.Until(expires)
	if remaining <= 0 {
		return ErrUnavailable
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	item := &delivery{result: make(chan error, 1), command: command, ctx: ctx, expires: expires}
	p.mu.Lock()
	if p.hostMinor >= 1 && !p.ready {
		p.mu.Unlock()
		return ErrUnavailable
	}
	if p.hostMinor == 0 && request.CapabilitySchemaId == contracts.FanSchema {
		p.mu.Unlock()
		return ErrUnavailable
	}
	if p.hostMinor < 3 {
		if light := request.GetArguments().GetSetLight(); light != nil && light.TransitionSeconds != nil {
			p.mu.Unlock()
			return ErrUnavailable
		}
	}
	handle := p.entityHandles[entityText]
	if len(handle) != 32 {
		p.mu.Unlock()
		return ErrUnavailable
	}
	item.frame = &wire.CommandsResponse{Request: request, Correlation: correlationText, BindingHandle: append([]byte(nil), handle...)}
	if err := p.frameLocked(item.frame); err != nil {
		p.mu.Unlock()
		return err
	}
	for key, entry := range p.correlations {
		if time.Now().After(entry.expires) {
			delete(p.correlations, key)
		}
	}
	if !p.active || !p.handshaken || p.leaseExpiredLocked() || !p.streaming || !p.capabilities[wire.Capability_CAPABILITY_COMMAND] {
		p.mu.Unlock()
		return ErrUnavailable
	}
	if len(p.pending) >= maxPending || len(p.correlations) >= maxCorrelations || len(p.queue) >= cap(p.queue) {
		p.mu.Unlock()
		return ErrCapacity
	}
	if _, exists := p.pending[idText]; exists {
		p.mu.Unlock()
		return ErrCapacity
	}
	p.pending[idText] = item
	p.correlations[correlationText] = correlation{entity: entityText, expires: expires}
	p.mu.Unlock()
	select {
	case p.queue <- item:
	case <-ctx.Done():
		p.dropPending(idText, correlationText, item)
		return ctx.Err()
	case <-p.ctx.Done():
		p.dropPending(idText, correlationText, item)
		return ErrUnavailable
	case <-timer.C:
		p.dropPending(idText, correlationText, item)
		return ErrUnavailable
	}
	select {
	case result := <-item.result:
		return result
	case <-ctx.Done():
		p.dropPending(idText, correlationText, item)
		return ctx.Err()
	case <-p.ctx.Done():
		p.dropPending(idText, correlationText, item)
		return ErrUnavailable
	case <-timer.C:
		p.dropPending(idText, correlationText, item)
		return ErrUnavailable
	}
}

func (p *pilot) dropPending(id, cause string, item *delivery) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending[id] == item {
		delete(p.pending, id)
		if !item.delivered {
			delete(p.correlations, cause)
		}
	}
}

func (p *pilot) Commands(ctx context.Context, r *connect.Request[wire.CommandsRequest], stream *connect.ServerStream[wire.CommandsResponse]) error {
	release, err := p.reserveRPC()
	if err != nil {
		return err
	}
	defer release()
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_COMMAND, r.Msg); err != nil {
		return err
	}
	p.mu.Lock()
	if p.streaming || p.streamEver {
		p.mu.Unlock()
		return connect.NewError(connect.CodeAlreadyExists, ErrUnavailable)
	}
	p.streaming = true
	p.streamEver = true
	close(p.streamReady)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.streaming = false
		active := p.active
		p.mu.Unlock()
		if active {
			p.transportOnce.Do(func() { close(p.transportLost) })
		}
	}()
	// Flush admission headers without a CommandsResponse. Ownership and its
	// cleanup are established above, and no network I/O occurs under p.mu.
	if err := stream.Send(nil); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.ctx.Done():
			return nil
		case item := <-p.queue:
			if !p.deliveryEligible(item) {
				p.dropPending(item.frame.Request.CommandId, item.frame.Correlation, item)
				select {
				case item.result <- ErrUnavailable:
				default:
				}
				continue
			}
			if err := p.frame(item.frame); err != nil {
				p.dropPending(item.frame.Request.CommandId, item.frame.Correlation, item)
				select {
				case item.result <- ErrCapacity:
				default:
				}
				continue
			}
			p.mu.Lock()
			current := p.pending[item.frame.Request.CommandId] == item && p.active && !p.leaseExpiredLocked() && p.ctx.Err() == nil &&
				item.ctx.Err() == nil && time.Now().Before(item.expires)
			if current {
				item.delivered = true
			}
			p.mu.Unlock()
			if !current {
				p.dropPending(item.frame.Request.CommandId, item.frame.Correlation, item)
				select {
				case item.result <- ErrUnavailable:
				default:
				}
				continue
			}
			if err := stream.Send(item.frame); err != nil {
				select {
				case item.result <- ErrUnavailable:
				default:
				}
				return err
			}
		}
	}
}

func (p *pilot) deliveryEligible(item *delivery) bool {
	return item != nil && item.ctx.Err() == nil && time.Now().Before(item.expires) && item.command.ReadyForTransport()
}

func (p *pilot) CompleteCommand(_ context.Context, r *connect.Request[wire.CompleteCommandRequest]) (*connect.Response[wire.CompleteCommandResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_COMMAND, r.Msg); err != nil {
		return nil, err
	}
	var result error
	acknowledged := false
	switch r.Msg.Kind {
	case wire.CommandResultKind_COMMAND_RESULT_KIND_ACCEPTED:
	case wire.CommandResultKind_COMMAND_RESULT_KIND_FAILED:
		result = ErrCommandFailed
	case wire.CommandResultKind_COMMAND_RESULT_KIND_ACKNOWLEDGED:
		acknowledged = true
	default:
		return nil, rpcInvalid()
	}
	p.mu.Lock()
	item, exists := p.pending[r.Msg.CommandId]
	if exists && p.hostMinor == 0 && item.frame.Request.CapabilitySchemaId == contracts.FanSchema {
		p.mu.Unlock()
		return nil, rpcInvalid()
	}
	if acknowledged && p.hostMinor < 1 {
		p.mu.Unlock()
		return nil, rpcInvalid()
	}
	if !exists || !item.delivered || !p.active || !p.deliveryEligible(item) {
		p.mu.Unlock()
		return nil, rpcUnavailable()
	}
	delete(p.pending, r.Msg.CommandId)
	p.mu.Unlock()
	if acknowledged {
		if err := item.command.Acknowledge(); err != nil {
			select {
			case item.result <- err:
			default:
			}
			return nil, rpcUnavailable()
		}
	}
	select {
	case item.result <- result:
	default:
		return nil, connect.NewError(connect.CodeInternal, errors.New("worker: result channel unavailable"))
	}
	return connect.NewResponse(&wire.CompleteCommandResponse{}), nil
}
