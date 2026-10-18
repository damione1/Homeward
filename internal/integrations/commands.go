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
// may perform I/O, so Host calls it after releasing all admission locks. It
// must honor cancellation before later steps and must not detach unsupervised
// work after Dispatch returns.
type CommandDriver interface {
	Dispatch(context.Context, AdmittedCommand) error
}

var ErrSupervisedPilotPanic = errors.New("supervised pilot panicked during command dispatch")

type AdmittedCommand struct {
	id       domain.CommandID
	actor    domain.ActorID
	entity   domain.EntityID
	request  *actions.CommandRequest
	progress *actionEntry
	tracker  *actionTracker
	host     *Manager
	binding  Binding
	light    lightTarget
}

// Causality is a command-scoped token. A driver attaches it to a subsequent
// StateObservation; a value match without this token cannot confirm an action.
func (c AdmittedCommand) Causality() (domain.Causality, error) {
	if _, _, _, err := c.Identity(); err != nil || c.progress == nil {
		return domain.Causality{}, ErrInvalidInput
	}
	return domain.NewCausality(nil, &c.progress.correlation)
}

func (c AdmittedCommand) Acknowledge() error {
	if _, _, _, err := c.Identity(); err != nil || c.progress == nil || c.tracker == nil || c.host == nil {
		return ErrInvalidInput
	}
	c.host.mu.Lock()
	defer c.host.mu.Unlock()
	if !c.liveOwner() {
		return ErrInactiveSession
	}
	return c.tracker.acknowledge(c.progress)
}

func (c AdmittedCommand) ConfirmProtocol(qualifier ProtocolConfirmation) error {
	if _, _, _, err := c.Identity(); err != nil || c.progress == nil || c.tracker == nil || c.host == nil || nilInterface(qualifier) {
		return ErrInvalidInput
	}
	// Protocol parsing may be slow; run it before taking Host or tracker locks.
	if err := qualifier.QualifyProtocolConfirmation(c); err != nil {
		return err
	}
	c.host.mu.Lock()
	defer c.host.mu.Unlock()
	if !c.liveOwner() {
		return ErrInactiveSession
	}
	return c.tracker.protocol(c.progress)
}

// Called with Host lock held. The lease remains historically valid, but a
// replaced owner may no longer issue new acknowledgements or confirmations.
func (c AdmittedCommand) liveOwner() bool {
	session := c.host.active[c.binding.instance]
	_, sourceRetiring := c.host.retiringSources[retiringSource{c.binding.instance, c.binding.key}]
	_, entityRetiring := c.host.retiring[c.entity]
	return !c.host.retirementExhausted && !sourceRetiring && !entityRetiring && session != nil && session.live() && session.id == c.binding.session && session.bindings[c.entity] == c.binding && c.host.owners[c.entity] == c.binding
}

// ReadyForTransport rechecks the Host fence immediately before an external
// adapter emits a queued command. A packet already in flight remains uncertain.
func (c AdmittedCommand) ReadyForTransport() bool {
	if c.host == nil {
		return false
	}
	c.host.mu.Lock()
	defer c.host.mu.Unlock()
	return c.host.admission == admissionOpen && c.liveOwner() && (c.light.profile == nil || c.host.lightProfiles[c.entity] == c.light.profile)
}

// MQTTLightEmission returns the encoded brightness captured during route
// admission only if the driver's current discovery profile is the same one.
// The driver still calls ReadyForTransport immediately before writing.
func (c AdmittedCommand) MQTTLightEmission(current LightEncoding) (uint32, bool, error) {
	if c.light.profile == nil || !validLightEncoding(current) || c.light.profile.encoding != current {
		return 0, false, ErrCommandRouteUnavailable
	}
	return c.light.brightness, c.light.hasBrightness, nil
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
	if pointSource, ok := host.sink.(ActionPointSource); !ok || nilInterface(pointSource) {
		return nil, ErrInvalidInput
	}
	if receiptSink, ok := host.sink.(ActionReceiptSink); !ok || nilInterface(receiptSink) {
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
	var actionPoint ActionPoint
	correlation, err := newActionCorrelation()
	if err != nil {
		return nil, err
	}
	var driver CommandDriver
	var managed *taskGroup
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
		if h.admission != admissionOpen || h.retirementExhausted {
			return ErrAdmissionsClosed
		}
		if !wire.ExpiresAt.AsTime().After(r.clock.Now()) {
			rejected = commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at").Error
			return nil
		}
		binding, exists := h.owners[entity]
		if !exists {
			rejected = commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "target").Error
			return nil
		}
		if _, fenced := h.retiringSources[retiringSource{binding.instance, binding.key}]; fenced {
			rejected = commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "target").Error
			return nil
		}
		if _, fenced := h.retiring[entity]; fenced {
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
			pointSource := r.projection.(ActionPointSource) // checked at construction
			var pointErr error
			actionPoint, pointErr = pointSource.CommandActionPoint(entity)
			if pointErr != nil {
				return pointErr
			}
			var target lightTarget
			if description.SchemaId == contracts.LightSchema {
				profile := h.lightProfiles[entity]
				if profile != nil && profile.endpoint == description.Endpoint {
					target = targetForLight(resolved, profile)
				}
			}
			admitted = AdmittedCommand{id: id, actor: actor, entity: entity, request: resolved, tracker: h.actions, host: h, binding: binding, light: target}
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
			case errors.Is(routeErr, ErrCommandRouteBusy), errors.Is(routeErr, ErrActionProgressFull), errors.Is(routeErr, context.Canceled), errors.Is(routeErr, context.DeadlineExceeded):
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
		driver = session.commandDriver
		managed = session.work
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
	progress, err := r.host.actions.acceptTarget(id, entity, admitted.binding, wire, admitted.light, actionPoint, correlation, wire.ExpiresAt.AsTime().Sub(r.clock.Now()))
	if err != nil {
		return nil, err
	}
	admitted.progress = progress
	if managed != nil {
		release, err := managed.AdmitCommand()
		if err != nil {
			r.host.actions.abort(progress)
			return nil, err
		}
		defer release()
	}
	dispatchCtx, cancel := context.WithDeadline(ctx, wire.ExpiresAt.AsTime())
	if managed != nil {
		hookDone := make(chan struct{})
		go func() {
			defer close(hookDone)
			select {
			case <-managed.ctx.Done():
				cancel()
			case <-dispatchCtx.Done():
			}
		}()
		defer func() { cancel(); <-hookDone }()
		if err := managed.ctx.Err(); err != nil {
			r.host.actions.abort(progress)
			return nil, err
		}
	} else {
		defer cancel()
	}
	if !wire.ExpiresAt.AsTime().After(r.clock.Now()) {
		r.host.actions.abort(progress)
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
	}
	if !r.host.actions.begin(dispatchCtx, cancel, admitted.progress) {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_CANCELLED}, nil
	}
	defer r.host.actions.finish(admitted.progress)
	defer func() {
		if panicValue := recover(); panicValue != nil {
			r.host.actions.uncertain(admitted.progress)
			panic(panicValue)
		}
	}()
	var dispatchErr error
	if managed != nil {
		dispatchErr = dispatchManaged(managed, driver, dispatchCtx, admitted)
	} else {
		dispatchErr = driver.Dispatch(dispatchCtx, admitted)
	}
	if dispatchErr != nil {
		r.host.actions.uncertain(admitted.progress)
		// Once the driver was called, a transport error cannot prove the absence
		// of a physical effect. Keep the live progression indeterminate.
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}, dispatchErr
	}
	return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_DISPATCHED}, nil
}

// This is the managed task boundary. An external RPC goroutine need not know
// about pilot panics; the router preserves indeterminate progress and the
// supervisor sees failure. Legacy unsupervised sessions retain repanic.
func dispatchManaged(group *taskGroup, driver CommandDriver, ctx context.Context, command AdmittedCommand) (err error) {
	defer func() {
		if recover() != nil {
			group.Fail()
			err = ErrSupervisedPilotPanic
		}
	}()
	return driver.Dispatch(ctx, command)
}

var errCommandExpired = errors.New("command deadline passed during admission")
