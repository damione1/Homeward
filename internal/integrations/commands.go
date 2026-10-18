package integrations

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

// CommandDriver receives only immutable, fully admitted commands. Its method
// may perform I/O, so Host calls it after releasing all admission locks.
type CommandDriver interface {
	Dispatch(context.Context, AdmittedCommand) error
}

type AdmittedCommand struct {
	id      domain.CommandID
	actor   domain.ActorID
	entity  domain.EntityID
	request *actions.CommandRequest
}

func (c AdmittedCommand) Identity() (domain.CommandID, domain.ActorID, domain.EntityID, error) {
	if _, err := c.id.Value(); err != nil {
		return domain.CommandID{}, domain.ActorID{}, domain.EntityID{}, ErrInvalidInput
	}
	if _, err := c.actor.Value(); err != nil {
		return domain.CommandID{}, domain.ActorID{}, domain.EntityID{}, ErrInvalidInput
	}
	if _, err := c.entity.Value(); err != nil {
		return domain.CommandID{}, domain.ActorID{}, domain.EntityID{}, ErrInvalidInput
	}
	return c.id, c.actor, c.entity, nil
}

func (c AdmittedCommand) Request() (*actions.CommandRequest, error) {
	if _, _, _, err := c.Identity(); err != nil || c.request == nil {
		return nil, ErrInvalidInput
	}
	return proto.Clone(c.request).(*actions.CommandRequest), nil
}

type commandOwnerSlot struct {
	device   domain.DeviceID
	schema   string
	endpoint string
}
type commandEndpointSlot struct {
	device   domain.DeviceID
	endpoint string
}

// DesignateCommandOwner records an explicit, audited choice for two bindings
// of one known DeviceID and capability slot. It grants neither session
// ownership nor a capability. Without a designation, ambiguous slots close.
func (h *Manager) DesignateCommandOwner(device domain.DeviceID, schema, endpoint string, entity domain.EntityID) error {
	if h == nil || schema == "" || endpoint == "" {
		return ErrInvalidInput
	}
	if _, err := device.Value(); err != nil {
		return err
	}
	if _, err := entity.Value(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	binding, ok := h.owners[entity]
	if !ok || binding.device != device {
		return ErrForeignBinding
	}
	descriptions, err := h.catalog.Descriptions(entity)
	if err != nil {
		return err
	}
	found := false
	for _, description := range descriptions {
		if description.SchemaId == schema && description.Endpoint == endpoint {
			found = true
			break
		}
	}
	if !found {
		return ErrCommandRouteUnavailable
	}
	slot := commandOwnerSlot{device, schema, endpoint}
	if h.inflight[commandEndpointSlot{device, endpoint}] {
		return ErrCommandRouteBusy
	}
	h.commandOwners[slot] = entity
	return nil
}

// AttachCommandDriver is called by trusted composition, never via SessionHost.
// A replacement session must attach its own driver after discovery.
func (h *Manager) AttachCommandDriver(session *Session, driver CommandDriver) error {
	if h == nil || session == nil || session.host != h || nilInterface(driver) {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !session.live() {
		return ErrInactiveSession
	}
	if session.commandDriver != nil {
		return ErrInvalidInput
	}
	session.commandDriver = driver
	return nil
}

// GenerationAuthority holds the active configuration revision stable through
// its callback. It must be bounded and memory-only, with no SQL, network, or
// driver calls. Runtime configuration commits must take the same authority.
type GenerationAuthority interface {
	WithCurrent(context.Context, func(uint64) error) error
}

type CommandRouter struct {
	host       *Manager
	generation GenerationAuthority
	projection CommandProjection
	clock      CommandClock
}

// CommandClock.Now is bounded and memory-only; admission calls it under Host
// lock to check the deadline at the dispatch lease point.
type CommandClock interface{ Now() time.Time }
type realCommandClock struct{}

func (realCommandClock) Now() time.Time { return time.Now() }

func NewCommandRouter(host *Manager, generation GenerationAuthority) (*CommandRouter, error) {
	return NewCommandRouterWithClock(host, generation, realCommandClock{})
}

func NewCommandRouterWithClock(host *Manager, generation GenerationAuthority, clock CommandClock) (*CommandRouter, error) {
	if host == nil || nilInterface(generation) || nilInterface(clock) {
		return nil, ErrInvalidInput
	}
	projection, ok := host.sink.(CommandProjection)
	if !ok || nilInterface(projection) {
		return nil, ErrInvalidInput
	}
	return &CommandRouter{host: host, generation: generation, projection: projection, clock: clock}, nil
}

func commandFailure(code actions.ErrorCode, path string) *actions.CommandResponse {
	return &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_FAILED, Error: &actions.ContractError{Code: code, FieldPath: path, Message: "command admission rejected"}}
}

func (r *CommandRouter) Dispatch(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest) (*actions.CommandResponse, error) {
	if r == nil || r.host == nil {
		return nil, ErrInvalidInput
	}
	r.host.mu.Lock()
	ledger := r.host.volatile
	if ledger == nil {
		// A service cannot be attached after an unledgered dispatch started.
		r.host.unprotectedCommandDispatch = true
	}
	r.host.mu.Unlock()
	if ledger != nil {
		return ledger.dispatch(r, ctx, actor, request)
	}
	if request != nil {
		if id, err := domain.ParseCommandID(request.CommandId); err == nil {
			if _, actorErr := actor.Value(); actorErr == nil {
				if err := r.host.claimCommandPolicy(id, commandPolicyVolatile); err != nil {
					return nil, err
				}
			}
		}
	}
	return r.dispatchOnce(ctx, actor, request)
}

func (r *CommandRouter) dispatchOnce(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest) (*actions.CommandResponse, error) {
	if r == nil || r.host == nil || ctx == nil || request == nil {
		return nil, ErrInvalidInput
	}
	wire := proto.Clone(request).(*actions.CommandRequest)
	if contractErr := contracts.RejectUnknownCommandFields(wire); contractErr != nil {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_FAILED, Error: contractErr}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := actor.Value(); err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "actor"), nil
	}
	id, err := domain.ParseCommandID(wire.CommandId)
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command_id"), nil
	}
	if wire.ExpiresAt == nil || wire.ExpiresAt.CheckValid() != nil || !wire.ExpiresAt.AsTime().After(r.clock.Now()) {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
	}
	var entity domain.EntityID
	var alias *domain.HAAlias
	if wire.Target == nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"), nil
	}
	switch target := wire.Target.Reference.(type) {
	case *wireregistry.EntityRef_EntityId:
		if target != nil {
			entity, err = domain.ParseEntityID(target.EntityId)
		} else {
			err = ErrInvalidInput
		}
	case *wireregistry.EntityRef_HaAlias:
		if target != nil {
			var parsed domain.HAAlias
			parsed, err = domain.ParseHAAlias(target.HaAlias)
			if err == nil {
				alias = &parsed
				entity, err = r.projection.ResolveCommandAlias(parsed)
			}
		} else {
			err = ErrInvalidInput
		}
	default:
		err = ErrInvalidInput
	}
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"), nil
	}
	entityText, _ := entity.Value()
	resolved := proto.Clone(wire).(*actions.CommandRequest)
	resolved.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: entityText}}
	prepared, contractErr := r.host.catalog.PrepareCapabilityCommand(resolved)
	if contractErr != nil {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_FAILED, Error: contractErr}, nil
	}
	var admitted AdmittedCommand
	var driver CommandDriver
	var leased commandEndpointSlot
	defer func() {
		if leased.device == (domain.DeviceID{}) {
			return
		}
		r.host.mu.Lock()
		delete(r.host.inflight, leased)
		r.host.mu.Unlock()
	}()
	var rejected *actions.ContractError
	var routeFailure error
	err = r.generation.WithCurrent(ctx, func(revision uint64) error {
		if revision == 0 {
			return ErrCommandRouteUnavailable
		}
		h := r.host
		h.mu.Lock()
		defer h.mu.Unlock()
		if !wire.ExpiresAt.AsTime().After(r.clock.Now()) {
			rejected = commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at").Error
			return nil
		}
		binding, exists := h.owners[entity]
		if !exists {
			rejected = commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "target").Error
			return nil
		}
		session := h.active[binding.instance]
		if session == nil || !session.live() || session.bindings[entity] != binding || nilInterface(session.commandDriver) {
			rejected = commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "target").Error
			return nil
		}
		contractErr, routeErr := h.catalog.CommitPreparedCapabilityCommand(prepared, wire.AdmissionEpoch, revision, func(description *wireregistry.CapabilityDescription) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !wire.ExpiresAt.AsTime().After(r.clock.Now()) {
				return errCommandExpired
			}
			slot := commandOwnerSlot{binding.device, description.SchemaId, description.Endpoint}
			endpointSlot := commandEndpointSlot{binding.device, description.Endpoint}
			if h.inflight[endpointSlot] {
				return ErrCommandRouteBusy
			}
			selected := h.commandOwners[slot]
			if err := r.projection.AdmitCommandRoute(CommandRoute{Entity: entity, Alias: alias, Binding: binding, Description: description, Designated: selected, ExpectedRevision: wire.ExpectedRevision, Epoch: wire.AdmissionEpoch}); err != nil {
				return err
			}
			// Slot lease is reserved before the Host lock is released.
			h.inflight[endpointSlot] = true
			leased = endpointSlot
			return nil
		})
		if contractErr != nil {
			rejected = contractErr
			return nil
		}
		if routeErr != nil {
			switch {
			case errors.Is(routeErr, ErrCommandRouteBusy), errors.Is(routeErr, context.Canceled), errors.Is(routeErr, context.DeadlineExceeded):
				routeFailure = routeErr
			case errors.Is(routeErr, errCommandExpired):
				rejected = commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at").Error
			case errors.Is(routeErr, ErrCommandEpochChanged):
				rejected = commandFailure(actions.ErrorCode_ERROR_CODE_STALE_EPOCH, "admission_epoch").Error
			case errors.Is(routeErr, ErrCommandRevisionChanged):
				rejected = commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expected_revision").Error
			default:
				rejected = commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "target").Error
			}
			return nil
		}
		// Lease linearization: route validation succeeded under Host→catalogue→
		// sequencer locks. Later revocation cannot retroactively cancel it.
		admitted = AdmittedCommand{id: id, actor: actor, entity: entity, request: resolved}
		driver = session.commandDriver
		return nil
	})
	if err != nil {
		return nil, err
	}
	if routeFailure != nil {
		return nil, routeFailure
	}
	if rejected != nil {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_FAILED, Error: rejected}, nil
	}
	if driver == nil {
		return nil, ErrCommandRouteUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !wire.ExpiresAt.AsTime().After(r.clock.Now()) {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
	}
	dispatchCtx, cancel := context.WithDeadline(ctx, wire.ExpiresAt.AsTime())
	defer cancel()
	if err := driver.Dispatch(dispatchCtx, admitted); err != nil {
		// Once the driver was called, a transport error cannot prove the absence
		// of a physical effect. Confirmation belongs to a later ticket.
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}, err
	}
	return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}, nil
}

var errCommandExpired = errors.New("command deadline passed during admission")
