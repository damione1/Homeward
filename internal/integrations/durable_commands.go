package integrations

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

var (
	ErrDurableLedgerUnavailable = errors.New("durable command ledger unavailable")
	ErrDurableCommitUnknown     = errors.New("durable command commit outcome unknown")
	ErrDurableRuntimeBusy       = errors.New("durable command runtime already active")
)

// DurableCommandService is selected explicitly by trusted composition for
// intentions requiring durable audit. It is not a fallback from the volatile
// route. Construct it before admitting durable traffic and Close it on shutdown.
type DurableCommandService struct {
	mu       sync.Mutex
	router   *CommandRouter
	store    *storage.Store
	runtime  *storage.CommandRuntime
	epoch    string
	active   int
	gate     durableGate
	closed   chan struct{}
	closeErr error
}

func NewDurableCommandService(ctx context.Context, host *Manager, generation GenerationAuthority, store *storage.Store) (*DurableCommandService, error) {
	return NewDurableCommandServiceWithClock(ctx, host, generation, store, realCommandClock{})
}

func NewDurableCommandServiceWithClock(ctx context.Context, host *Manager, generation GenerationAuthority, store *storage.Store, clock CommandClock) (*DurableCommandService, error) {
	return newDurableCommandService(ctx, host, generation, store, clock, DurableGateLimits{})
}

// NewDurableCommandServiceWithClockAndGate is the Run-owned composition path.
// The limits are admitted before acquiring Host or the SQL runtime.
func NewDurableCommandServiceWithClockAndGate(ctx context.Context, host *Manager, generation GenerationAuthority, store *storage.Store, clock CommandClock, limits DurableGateLimits) (*DurableCommandService, error) {
	if !limits.valid() {
		return nil, ErrInvalidInput
	}
	return newDurableCommandService(ctx, host, generation, store, clock, limits)
}

func newDurableCommandService(ctx context.Context, host *Manager, generation GenerationAuthority, store *storage.Store, clock CommandClock, limits DurableGateLimits) (*DurableCommandService, error) {
	if nilInterface(ctx) || store == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := host.reserveDurable(); err != nil {
		return nil, err
	}
	defer host.abortDurable()
	router, err := NewCommandRouterWithClock(host, generation, clock)
	if err != nil {
		return nil, err
	}
	authority, ok := router.projection.(CommandEpochAuthority)
	if !ok || nilInterface(authority) {
		return nil, ErrInvalidInput
	}
	epoch, err := authority.CurrentAdmissionEpoch()
	if err != nil || epoch == "" {
		return nil, ErrInvalidInput
	}
	runtime, err := store.AcquireCommandRuntime(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var storageErr *storage.Error
		if errors.As(err, &storageErr) && storageErr.Kind() == storage.CommandRuntimeBusy {
			return nil, ErrDurableRuntimeBusy
		}
		host.observeDurableTransaction(storage.TransactionNotCommitted, err)
		return nil, ErrDurableLedgerUnavailable
	}
	uncertain := &actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
	encoded, err := proto.Marshal(uncertain)
	if err != nil {
		return nil, errors.Join(ErrInvalidInput, err, runtime.Close())
	}
	recovered, outcome, err := store.RecoverCommandIntentsCounted(ctx, runtime, encoded)
	// Recovery's guard Ping may wrap a caller cancellation as Unavailable.
	// Cancellation before COMMIT is no outage fact; an unknown COMMIT must
	// remain visible even when that same caller context has expired.
	if outcome != storage.TransactionNotCommitted || ctx.Err() == nil {
		host.observeDurableTransaction(outcome, err)
	}
	if outcome != storage.TransactionCommitted || err != nil {
		return nil, errors.Join(durableTransactionError(outcome), err, runtime.Close())
	}
	_ = host.observation.AddDurableRecoveredIndeterminate(recovered)
	service := &DurableCommandService{router: router, store: store, runtime: runtime, epoch: epoch, closed: make(chan struct{}), gate: durableGate{state: gateOpen, changed: make(chan struct{}), limits: limits}}
	router.internal = &InternalAutomationPort{service: service}
	if err := host.completeDurable(service); err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return service, nil
}

// Done closes after admission waiters and occurrences have left, each active
// dispatch's last result-persistence attempt has ended, and the runtime release
// attempt has ended. Call Close with a live context to obtain the recorded
// release result; Done alone does not prove release.
func (s *DurableCommandService) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.closed
}

// Close rejects new admissions immediately and wakes paused waiters. It does
// not cancel an admitted occurrence or driver. A timed-out Close retains the
// DB guard until their last dispatch and result-persistence attempt return.
func (s *DurableCommandService) Close(ctx context.Context) error {
	if s == nil || nilInterface(ctx) || s.router == nil || s.store == nil || s.closed == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	if s.gate.state != gateClosing {
		s.gate.state = gateClosing
		s.gate.pause = nil
		s.gateChangedLocked()
	}
	runtime := s.finishIfDrainedLocked()
	done := s.closed
	s.mu.Unlock()
	if runtime != nil {
		go s.finishClose(runtime)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		return s.closeResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *DurableCommandService) closeResult() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

func (s *DurableCommandService) end() {
	s.mu.Lock()
	s.active--
	s.gateChangedLocked()
	runtime := s.finishIfDrainedLocked()
	s.mu.Unlock()
	if runtime != nil {
		go s.finishClose(runtime)
	}
}

func (s *DurableCommandService) finishClose(runtime *storage.CommandRuntime) {
	var err error
	if runtime == nil {
		err = ErrInvalidInput
	} else {
		err = runtime.Close()
	}
	s.mu.Lock()
	s.closeErr = err
	s.mu.Unlock()
	close(s.closed)
}

// DurableCommandStatus is a persisted state, never proof of a physical effect.
type DurableCommandStatus struct {
	State     storage.IntentState
	Response  *actions.CommandResponse
	ErrorKind storage.IntentError
}

func (s *DurableCommandService) Status(ctx context.Context, id domain.CommandID) (DurableCommandStatus, bool, error) {
	if s == nil || ctx == nil || s.store == nil || s.router == nil {
		return DurableCommandStatus{}, false, ErrInvalidInput
	}
	record, found, err := s.store.CommandIntent(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			s.router.host.observeDurableTransaction(storage.TransactionNotCommitted, err)
		}
		return DurableCommandStatus{}, false, ErrDurableLedgerUnavailable
	}
	if !found {
		if s.router.host.commandAuthority() != nil {
			return DurableCommandStatus{}, false, ErrCommandAuthorizationDenied
		}
		return DurableCommandStatus{}, false, nil
	}
	if record.OriginKind() != storage.IntentOriginExternalActor {
		return DurableCommandStatus{}, false, ErrCommandAuthorizationDenied
	}
	target, present, targetErr := record.AuthorizationTarget()
	if targetErr != nil {
		return DurableCommandStatus{}, false, ErrCommandAuthorizationDenied
	}
	if err := s.router.authorizeStored(ctx, record.Fingerprint(), target, present); err != nil {
		return DurableCommandStatus{}, false, err
	}
	response, err := storedCommandResponse(id, record)
	if err != nil {
		return DurableCommandStatus{}, false, err
	}
	return DurableCommandStatus{State: record.State(), Response: response, ErrorKind: record.ErrorKind()}, true, nil
}

func (s *DurableCommandService) Dispatch(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest) (*actions.CommandResponse, error) {
	return s.dispatch(ctx, actor, request, nil)
}

func (s *DurableCommandService) CaptureLight(ctx context.Context, alias domain.HAAlias) (LightCapture, error) {
	if s == nil || s.router == nil {
		return LightCapture{}, ErrInvalidInput
	}
	return s.router.CaptureLight(ctx, alias)
}

func (s *DurableCommandService) DispatchPrepared(ctx context.Context, prepared PreparedLight) (*actions.CommandResponse, error) {
	if s == nil || s.router == nil || prepared.route == nil || prepared.route.capture.router != s.router {
		return nil, ErrInvalidInput
	}
	wire, err := prepared.Request()
	if err != nil || prepared.actor != prepared.route.actor {
		return nil, ErrInvalidInput
	}
	return s.dispatch(ctx, prepared.actor, wire, preparedLightCommand(prepared.route))
}

func (s *DurableCommandService) CaptureFan(ctx context.Context, alias domain.HAAlias) (FanCapture, error) {
	if s == nil || s.router == nil {
		return FanCapture{}, ErrInvalidInput
	}
	return s.router.CaptureFan(ctx, alias)
}

func (s *DurableCommandService) CaptureDirect(ctx context.Context, alias domain.HAAlias, kind DirectKind) (DirectCapture, error) {
	if s == nil || s.router == nil {
		return DirectCapture{}, ErrInvalidInput
	}
	return s.router.CaptureDirect(ctx, alias, kind)
}

func (s *DurableCommandService) DispatchPreparedDirect(ctx context.Context, prepared PreparedDirect) (*actions.CommandResponse, error) {
	if s == nil || s.router == nil || prepared.route == nil || prepared.route.capture.router != s.router {
		return nil, ErrInvalidInput
	}
	wire, err := prepared.Request()
	if err != nil || prepared.actor != prepared.route.actor {
		return nil, ErrInvalidInput
	}
	return s.dispatch(ctx, prepared.actor, wire, preparedDirectCommand(prepared.route))
}

func (s *DurableCommandService) DispatchPreparedFan(ctx context.Context, prepared PreparedFan) (*actions.CommandResponse, error) {
	if s == nil || s.router == nil || prepared.route == nil || prepared.route.capture.router != s.router {
		return nil, ErrInvalidInput
	}
	wire, err := prepared.Request()
	if err != nil || prepared.actor != prepared.route.actor {
		return nil, ErrInvalidInput
	}
	return s.dispatch(ctx, prepared.actor, wire, preparedFanCommand(prepared.route))
}

// RecordedPrepared* consult the exact prepared envelope without claiming an
// ID or admitting a new effect. They remain usable after scene invalidation.
func (s *DurableCommandService) RecordedPreparedLight(ctx context.Context, prepared PreparedLight) (*actions.CommandResponse, bool, error) {
	if s == nil || s.router == nil || prepared.route == nil || prepared.route.capture.router != s.router {
		return nil, false, ErrInvalidInput
	}
	wire, err := prepared.Request()
	if err != nil || prepared.actor != prepared.route.actor {
		return nil, false, ErrInvalidInput
	}
	return s.recordedPrepared(preparedAuthorizationContext(ctx, preparedLightCommand(prepared.route)), prepared.actor, wire, preparedLightCommand(prepared.route))
}

func (s *DurableCommandService) RecordedPreparedFan(ctx context.Context, prepared PreparedFan) (*actions.CommandResponse, bool, error) {
	if s == nil || s.router == nil || prepared.route == nil || prepared.route.capture.router != s.router {
		return nil, false, ErrInvalidInput
	}
	wire, err := prepared.Request()
	if err != nil || prepared.actor != prepared.route.actor {
		return nil, false, ErrInvalidInput
	}
	return s.recordedPrepared(preparedAuthorizationContext(ctx, preparedFanCommand(prepared.route)), prepared.actor, wire, preparedFanCommand(prepared.route))
}

func (s *DurableCommandService) RecordedPreparedDirect(ctx context.Context, prepared PreparedDirect) (*actions.CommandResponse, bool, error) {
	if s == nil || s.router == nil || prepared.route == nil || prepared.route.capture.router != s.router {
		return nil, false, ErrInvalidInput
	}
	wire, err := prepared.Request()
	if err != nil || prepared.actor != prepared.route.actor {
		return nil, false, ErrInvalidInput
	}
	return s.recordedPrepared(preparedAuthorizationContext(ctx, preparedDirectCommand(prepared.route)), prepared.actor, wire, preparedDirectCommand(prepared.route))
}

func (s *DurableCommandService) recordedPrepared(ctx context.Context, actor domain.ActorID, wire *actions.CommandRequest, route *preparedCommandRoute) (*actions.CommandResponse, bool, error) {
	if s == nil || ctx == nil || wire == nil || s.store == nil || s.router == nil {
		return nil, false, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	id, err := domain.ParseCommandID(wire.CommandId)
	if err != nil {
		return nil, false, ErrInvalidInput
	}
	internal, err := s.router.internalFromContext(ctx)
	if err != nil || route == nil || route.internal() != internal || (internal != nil && actor != (domain.ActorID{})) {
		return nil, false, ErrCommandAuthorizationDenied
	}
	var fingerprint []byte
	var originKey storage.InternalAutomationOriginKey
	if internal != nil {
		var sealed storage.InternalAutomationFingerprint
		originKey, sealed, err = internal.fingerprint(id, wire)
		fingerprint = sealed.Bytes()
	} else {
		actorText, actorErr := actor.Value()
		if actorErr != nil {
			return nil, false, ErrInvalidInput
		}
		fingerprint, err = commandFingerprint(actorText, wire)
	}
	if err != nil {
		return nil, false, ErrInvalidInput
	}
	record, found, err := s.store.CommandIntent(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			s.router.host.observeDurableTransaction(storage.TransactionNotCommitted, err)
		}
		return nil, false, ErrDurableLedgerUnavailable
	}
	if !found {
		if internal == nil {
			if err := s.router.authorizeMissingRecord(ctx, actor, wire); err != nil {
				return nil, false, err
			}
		}
		return nil, false, nil
	}
	if internal != nil {
		storedKey, present := record.InternalOriginKey()
		if !present || storedKey != originKey {
			return nil, false, ErrCommandAuthorizationDenied
		}
	} else if record.OriginKind() != storage.IntentOriginExternalActor {
		return nil, false, ErrCommandAuthorizationDenied
	}
	target, present, targetErr := record.AuthorizationTarget()
	if targetErr != nil {
		return nil, false, ErrCommandAuthorizationDenied
	}
	if internal != nil && (!present || target != route.authorizationTarget()) {
		return nil, false, ErrCommandAuthorizationDenied
	}
	if internal == nil {
		if err := s.router.authorizeStored(ctx, record.Fingerprint(), target, present); err != nil {
			return nil, false, err
		}
	}
	response, err := replayDurable(id, record, fingerprint)
	return response, true, err
}

func (s *DurableCommandService) dispatch(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest, preparedRoute *preparedCommandRoute) (response *actions.CommandResponse, resultErr error) {
	if s == nil || nilInterface(ctx) || request == nil || s.router == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx = preparedAuthorizationContext(ctx, preparedRoute)
	internal, err := s.router.internalFromContext(ctx)
	if err != nil || (internal != nil && actor != (domain.ActorID{})) ||
		(preparedRoute != nil && preparedRoute.internal() != internal) {
		return nil, ErrCommandAuthorizationDenied
	}
	runtime, err := s.begin(ctx, internal)
	if err != nil {
		return nil, err
	}
	defer s.end()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var target authorization.CommandTarget
	if internal != nil {
		target, err = s.router.resolvedAuthorizationTarget(request)
	} else {
		target, err = s.router.authorizeRequest(ctx, actor, request, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	}
	if err != nil {
		return nil, err
	}
	if internal != nil && preparedRoute != nil && target != preparedRoute.authorizationTarget() {
		return nil, ErrCommandAuthorizationDenied
	}
	// Bound attacker-controlled wire size before cloning or deterministic
	// encoding. The encoded fingerprint below has its own exact bound.
	if proto.Size(request) > maxVolatileIntentBytes {
		return nil, ErrCommandIntentTooLarge
	}
	wire := proto.Clone(request).(*actions.CommandRequest)
	if contractErr := contracts.RejectUnknownCommandFields(wire); contractErr != nil {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_FAILED, Error: contractErr}, nil
	}
	id, err := domain.ParseCommandID(wire.CommandId)
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command_id"), nil
	}
	var fingerprint []byte
	var originKey storage.InternalAutomationOriginKey
	var internalFingerprint storage.InternalAutomationFingerprint
	if internal != nil {
		originKey, internalFingerprint, err = internal.fingerprint(id, wire)
		fingerprint = internalFingerprint.Bytes()
	} else {
		actorText, actorErr := actor.Value()
		if actorErr != nil {
			return commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "actor"), nil
		}
		fingerprint, err = commandFingerprint(actorText, wire)
	}
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command"), nil
	}
	if len(fingerprint) > maxVolatileIntentBytes {
		return nil, ErrCommandIntentTooLarge
	}
	if err := s.router.host.claimCommandPolicy(id, commandPolicyDurable); err != nil {
		return nil, err
	}
	if err := runtime.Ping(ctx); err != nil {
		if ctx.Err() == nil {
			s.router.host.observeDurableTransaction(storage.TransactionNotCommitted, err)
		}
		return nil, ErrDurableLedgerUnavailable
	}
	// Lookup precedes the current-epoch check: an old ID may be consulted, but
	// it must never become a fresh dispatch in the new epoch.
	record, found, err := s.store.CommandIntent(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			s.router.host.observeDurableTransaction(storage.TransactionNotCommitted, err)
		}
		return nil, ErrDurableLedgerUnavailable
	}
	if found {
		if internal != nil {
			storedKey, present := record.InternalOriginKey()
			if !present || storedKey != originKey {
				return nil, ErrCommandAuthorizationDenied
			}
		} else if record.OriginKind() != storage.IntentOriginExternalActor {
			return nil, ErrCommandAuthorizationDenied
		}
		stored, present, targetErr := record.AuthorizationTarget()
		if targetErr != nil {
			return nil, ErrCommandAuthorizationDenied
		}
		if internal != nil && (!present || stored != target) {
			return nil, ErrCommandAuthorizationDenied
		}
		if internal == nil {
			if err := s.router.authorizeStored(ctx, record.Fingerprint(), stored, present); err != nil {
				return nil, err
			}
		}
		return replayDurable(id, record, fingerprint)
	}
	authority := s.router.projection.(CommandEpochAuthority)
	current, err := authority.CurrentAdmissionEpoch()
	if err != nil || current == "" || current != s.epoch || wire.AdmissionEpoch != s.epoch {
		return commandFailure(actions.ErrorCode_ERROR_CODE_STALE_EPOCH, "admission_epoch"), nil
	}
	if wire.ExpiresAt == nil || wire.ExpiresAt.CheckValid() != nil || !wire.ExpiresAt.AsTime().After(s.router.clock.Now()) {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
	}
	if wire.Target == nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "target"), nil
	}
	var created bool
	var outcome storage.TransactionOutcome
	if internal != nil {
		record, created, outcome, err = s.store.AcceptInternalAutomationIntent(ctx, id, originKey, s.epoch, internalFingerprint, target)
	} else if _, _, targetErr := target.Values(); targetErr == nil {
		record, created, outcome, err = s.store.AcceptAuthorizedCommandIntent(ctx, id, actor, s.epoch, fingerprint, target)
	} else {
		record, created, outcome, err = s.store.AcceptCommandIntent(ctx, id, actor, s.epoch, fingerprint)
	}
	s.router.host.observeDurableTransaction(outcome, err)
	if outcome != storage.TransactionCommitted || err != nil {
		return nil, durableTransactionError(outcome)
	}
	if !created {
		if internal != nil {
			storedKey, present := record.InternalOriginKey()
			if !present || storedKey != originKey {
				return nil, ErrCommandAuthorizationDenied
			}
		} else if record.OriginKind() != storage.IntentOriginExternalActor {
			return nil, ErrCommandAuthorizationDenied
		}
		stored, present, targetErr := record.AuthorizationTarget()
		if targetErr != nil {
			return nil, ErrCommandAuthorizationDenied
		}
		if internal != nil && (!present || stored != target) {
			return nil, ErrCommandAuthorizationDenied
		}
		if internal == nil {
			if err := s.router.authorizeStored(ctx, record.Fingerprint(), stored, present); err != nil {
				return nil, err
			}
		}
		return replayDurable(id, record, fingerprint)
	}
	defer func() {
		if response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE || errors.Is(resultErr, ErrCommandOutcomeIndeterminate) || errors.Is(resultErr, ErrDurableCommitUnknown) {
			_ = s.router.host.observation.AddFirstAttemptIndeterminate(obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE)
		}
	}()
	// The dispatching commit deliberately precedes every final route fence.
	// A crash here may overstate uncertainty, but can never cause a resend.
	moved, outcome, err := s.store.MoveCommandIntent(ctx, id, storage.IntentAccepted, storage.IntentDispatching, nil, storage.IntentNoError)
	s.router.host.observeDurableTransaction(outcome, err)
	if outcome != storage.TransactionCommitted || err != nil {
		return nil, durableTransactionError(outcome)
	}
	if !moved {
		return nil, ErrDurableLedgerUnavailable
	}
	if err := runtime.Ping(ctx); err != nil {
		if ctx.Err() == nil {
			s.router.host.observeDurableTransaction(storage.TransactionNotCommitted, err)
		}
		return nil, ErrDurableLedgerUnavailable
	}
	response, dispatchErr := s.router.dispatchOnce(withExpectedAuthorizationTarget(ctx, target), actor, wire, preparedRoute)
	if response == nil {
		response = commandFailure(actions.ErrorCode_ERROR_CODE_UNSPECIFIED, "command")
		response.CommandId = wire.CommandId
	}
	terminal := storage.IntentResult
	if response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		terminal = storage.IntentIndeterminate
	}
	response = boundedVolatileResponse(response)
	encoded, err := proto.Marshal(response)
	if err != nil {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}, ErrCommandOutcomeIndeterminate
	}
	// Client cancellation cannot prevent recording an effect already attempted.
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errorKind := classifyDurableError(dispatchErr, response)
	moved, outcome, err = s.store.MoveCommandIntent(finishCtx, id, storage.IntentDispatching, terminal, encoded, errorKind)
	s.router.host.observeDurableTransaction(outcome, err)
	if outcome != storage.TransactionCommitted || err != nil || !moved {
		return &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}, ErrCommandOutcomeIndeterminate
	}
	return response, durableRecordedError(errorKind)
}

func classifyDurableError(err error, response *actions.CommandResponse) storage.IntentError {
	if response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		return storage.IntentOutcomeIndeterminate
	}
	if err == nil {
		return storage.IntentNoError
	}
	switch {
	case errors.Is(err, ErrCommandRouteBusy):
		return storage.IntentRouteBusy
	case errors.Is(err, ErrCommandRouteUnavailable):
		return storage.IntentRouteUnavailable
	case errors.Is(err, context.Canceled):
		return storage.IntentCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return storage.IntentDeadline
	default:
		return storage.IntentOtherError
	}
}

func durableRecordedError(kind storage.IntentError) error {
	switch kind {
	case storage.IntentNoError:
		return nil
	case storage.IntentRouteBusy:
		return ErrCommandRouteBusy
	case storage.IntentRouteUnavailable:
		return ErrCommandRouteUnavailable
	case storage.IntentCanceled:
		return context.Canceled
	case storage.IntentDeadline:
		return context.DeadlineExceeded
	case storage.IntentOtherError:
		return ErrCommandResultUnavailable
	case storage.IntentOutcomeIndeterminate:
		return ErrCommandOutcomeIndeterminate
	case storage.IntentErrorUnknown:
		return ErrDurableLedgerUnavailable
	default:
		return ErrDurableLedgerUnavailable
	}
}

func replayDurable(id domain.CommandID, record storage.IntentRecord, fingerprint []byte) (*actions.CommandResponse, error) {
	if !bytes.Equal(record.Fingerprint(), fingerprint) {
		return nil, ErrCommandIDConflict
	}
	response, err := storedCommandResponse(id, record)
	if err != nil {
		return nil, err
	}
	if record.State() == storage.IntentDispatching {
		return response, ErrCommandOutcomeIndeterminate
	}
	return response, durableRecordedError(record.ErrorKind())
}

func storedCommandResponse(id domain.CommandID, record storage.IntentRecord) (*actions.CommandResponse, error) {
	idText, err := id.Value()
	if err != nil {
		return nil, ErrInvalidInput
	}
	switch record.State() {
	case storage.IntentAccepted:
		return &actions.CommandResponse{CommandId: idText, Status: actions.ActionStatus_ACTION_STATUS_ACCEPTED}, nil
	case storage.IntentDispatching, storage.IntentIndeterminate:
		if record.State() == storage.IntentDispatching {
			return &actions.CommandResponse{CommandId: idText, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}, nil
		}
	case storage.IntentResult:
		// Decode below.
	case storage.IntentUnknown:
		return nil, ErrDurableLedgerUnavailable
	default:
		return nil, ErrDurableLedgerUnavailable
	}
	response := &actions.CommandResponse{}
	if err := proto.Unmarshal(record.Response(), response); err != nil {
		return nil, ErrDurableLedgerUnavailable
	}
	response.CommandId = idText
	if record.State() == storage.IntentIndeterminate {
		response.Status = actions.ActionStatus_ACTION_STATUS_INDETERMINATE
	}
	return response, nil
}

func durableTransactionError(outcome storage.TransactionOutcome) error {
	switch outcome {
	case storage.TransactionIndeterminate:
		return ErrDurableCommitUnknown
	case storage.TransactionNotCommitted:
		return ErrDurableLedgerUnavailable
	case storage.TransactionCommitted:
		return ErrDurableLedgerUnavailable
	case storage.TransactionOutcomeUnspecified:
		return ErrDurableLedgerUnavailable
	default:
		return ErrDurableLedgerUnavailable
	}
}
