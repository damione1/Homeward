package integrations

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/apitypes"
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
	// Lifecycle signal only: never used to dispatch or authorize an effect.
	proofDrain     context.Context
	stopProofDrain context.CancelFunc
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
	outcome, err = store.RecoverCommandAPITerminals(ctx)
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
// not cancel an admitted effect or driver. It ends read-only API proof waits,
// which persist their last native evidence before releasing the runtime guard.
// A timed-out Close retains the DB guard until the last persistence attempt ends.
func (s *DurableCommandService) Close(ctx context.Context) error {
	if s == nil || nilInterface(ctx) || s.router == nil || s.store == nil || s.closed == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	if s.gate.state != gateClosing {
		s.gate.state = gateClosing
		s.gate.pause = nil
		s.gateChangedLocked()
		if s.stopProofDrain != nil {
			s.stopProofDrain()
		}
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

// DurableCommandStatus carries persisted evidence and a separate native-owner
// reconciliation failure. Neither is inferred from UI equality or live status.
type DurableCommandStatus struct {
	State           storage.IntentState
	Response        *actions.CommandResponse
	ErrorKind       storage.IntentError
	Metadata        storage.CommandAPIMetadata
	HasMetadata     bool
	AcceptedAt      time.Time
	APITerminal     *actions.CommandResponse
	APIAcknowledged *actions.CommandResponse
	// True only after this exact native API occurrence finished its SQL attempts.
	APIReconciliationUnavailable bool
}

func (s *DurableCommandService) Status(ctx context.Context, id domain.CommandID) (DurableCommandStatus, bool, error) {
	if s == nil || ctx == nil || s.store == nil || s.router == nil {
		return DurableCommandStatus{}, false, ErrInvalidInput
	}
	flight := s.router.host.sampleAPICommandFlight(id)
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
	metadata, hasMetadata := record.APIMetadata()
	terminal, err := apiTerminalResponse(record)
	if err != nil {
		return DurableCommandStatus{}, false, err
	}
	acknowledged, err := apiAcknowledgedResponse(id, record)
	if err != nil {
		return DurableCommandStatus{}, false, err
	}
	return DurableCommandStatus{State: record.State(), Response: response, ErrorKind: record.ErrorKind(), Metadata: metadata, HasMetadata: hasMetadata, AcceptedAt: record.AcceptedAt(), APITerminal: terminal, APIAcknowledged: acknowledged, APIReconciliationUnavailable: present && flight.unavailable(id, s.epoch, target, record.Fingerprint())}, true, nil
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

// Admit returns as soon as a new external intention is durably accepted. Only
// the winning occurrence continues on the retained authenticated credential.
// The caller supplies a credential-bound, deadline-limited continuation context;
// completion releases that credential on every path, including deduplicated and
// rejected requests. All admission checks and SQL acceptance use the original
// HTTP context. Neither cancellation of HTTP nor reads can redispatch an intent.
func (s *DurableCommandService) Admit(ctx, continuation context.Context, actor domain.ActorID, request *actions.CommandRequest, metadata storage.CommandAPIMetadata, completion func()) (*actions.CommandResponse, error) {
	if ctx == nil || continuation == nil || completion == nil || request == nil || !metadata.Capability().Valid() {
		if completion != nil {
			completion()
		}
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		completion()
		return nil, err
	}
	if err := continuation.Err(); err != nil {
		completion()
		return nil, err
	}
	// Caller-owned protobuf memory cannot race the single native occurrence.
	if proto.Size(request) > maxVolatileIntentBytes {
		completion()
		return nil, ErrCommandIntentTooLarge
	}
	request = proto.Clone(request).(*actions.CommandRequest)
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	admitted := make(chan result, 1)
	go func() {
		defer completion()
		notified := false
		response, err := s.dispatchContinuing(ctx, actor, request, nil, func() context.Context {
			notified = true
			admitted <- result{response: &actions.CommandResponse{CommandId: request.CommandId, Status: actions.ActionStatus_ACTION_STATUS_ACCEPTED}}
			return continuation
		}, &metadata)
		if !notified {
			admitted <- result{response: response, err: err}
		}
	}()
	select {
	case value := <-admitted:
		return value.response, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *DurableCommandService) dispatch(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest, preparedRoute *preparedCommandRoute) (*actions.CommandResponse, error) {
	return s.dispatchContinuing(ctx, actor, request, preparedRoute, nil, nil)
}

func (s *DurableCommandService) dispatchContinuing(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest, preparedRoute *preparedCommandRoute, continuation func() context.Context, metadata *storage.CommandAPIMetadata) (response *actions.CommandResponse, resultErr error) {
	if s == nil || nilInterface(ctx) || request == nil || s.router == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx = preparedAuthorizationContext(ctx, preparedRoute)
	internal, err := s.router.internalFromContext(ctx)
	if err != nil || (internal != nil && actor != (domain.ActorID{})) ||
		(preparedRoute != nil && preparedRoute.internal() != internal) || (metadata != nil && internal != nil) {
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
		if metadata != nil {
			return replayAPICommand(id, record, *metadata)
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
	} else if metadata != nil {
		record, created, outcome, err = s.store.AcceptAPICommandIntent(ctx, id, actor, s.epoch, fingerprint, target, *metadata)
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
		if metadata != nil {
			return replayAPICommand(id, record, *metadata)
		}
		return replayDurable(id, record, fingerprint)
	}
	// A durable new reservation is the only handoff point. Existing records
	// returned above never acquire another dispatch occurrence or continuation.
	var apiFlight *commandAPIFlight
	defer func() {
		if metadata != nil && resultErr != nil {
			s.finalizeAbandonedAPICommand(id)
		}
		s.router.host.finishAPICommandFlight(apiFlight, resultErr != nil)
		if response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE || errors.Is(resultErr, ErrCommandOutcomeIndeterminate) || errors.Is(resultErr, ErrDurableCommitUnknown) {
			_ = s.router.host.observation.AddFirstAttemptIndeterminate(obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE)
		}
	}()
	if metadata != nil {
		apiFlight, err = s.router.host.beginAPICommandFlight(id, s.epoch, target, fingerprint)
		if err != nil {
			return nil, err
		}
	}
	if continuation != nil {
		ctx = continuation()
	}
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
	if metadata != nil && terminal == storage.IntentResult {
		final := response
		if response.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED || response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED || response.Status == actions.ActionStatus_ACTION_STATUS_ACCEPTED {
			final = s.awaitAPICommandProof(ctx, id, target, response)
		}
		proof, proofErr := proto.Marshal(final)
		if proofErr != nil {
			return response, ErrCommandOutcomeIndeterminate
		}
		proofCtx, proofCancel := context.WithTimeout(context.Background(), 5*time.Second)
		outcome, proofErr := s.store.AppendCommandAPITerminal(proofCtx, id, proof)
		proofCancel()
		s.router.host.observeDurableTransaction(outcome, proofErr)
		if outcome != storage.TransactionCommitted || proofErr != nil {
			return response, ErrCommandOutcomeIndeterminate
		}
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

// API idempotence compares the original public intention, not a newly resolved
// native epoch/route. An existing reservation is read only, including accepted
// and dispatching records; it never enters the continuation branch again.
func replayAPICommand(id domain.CommandID, record storage.IntentRecord, expected storage.CommandAPIMetadata) (*actions.CommandResponse, error) {
	actual, present := record.APIMetadata()
	if !present || actual != expected {
		return nil, ErrCommandIDConflict
	}
	return storedCommandResponse(id, record)
}

func apiTerminalResponse(record storage.IntentRecord) (*actions.CommandResponse, error) {
	encoded := record.APITerminalResponse()
	if len(encoded) == 0 {
		return nil, nil
	}
	response := &actions.CommandResponse{}
	if proto.Unmarshal(encoded, response) != nil {
		return nil, ErrDurableLedgerUnavailable
	}
	return response, nil
}

// apiProofContext narrows an already retained credential context with the
// service's shutdown signal. The lifecycle context carries no authority and is
// never passed to a driver or a SQL persistence attempt. Pausing admission for
// configuration activation does not cancel proof tracking.
func (s *DurableCommandService) apiProofContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if s.gate.state == gateClosing {
		s.mu.Unlock()
		cancel()
		return ctx, cancel
	}
	if s.proofDrain == nil {
		s.proofDrain, s.stopProofDrain = context.WithCancel(context.Background())
	}
	stop := context.AfterFunc(s.proofDrain, cancel)
	s.mu.Unlock()
	return ctx, func() { stop(); cancel() }
}

// Without an admitted ACK or terminal native proof, stopping observation leaves
// the effect explicitly uncertain. ACCEPTED and DISPATCHED cannot prove success.
func stoppedAPICommandProof(last *actions.CommandResponse) *actions.CommandResponse {
	if last.Status == actions.ActionStatus_ACTION_STATUS_ACCEPTED || last.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		return &actions.CommandResponse{CommandId: last.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
	}
	return last
}

// Only the existing native causal/protocol tracker can establish confirmation.
// This read-only wait never resends, infers equality from UI data, or outlives
// the retained credential/deadline or service drain. Final SQL proof remains
// append-once and is attempted after this read-only wait has ended.
func (s *DurableCommandService) awaitAPICommandProof(ctx context.Context, id domain.CommandID, target authorization.CommandTarget, acknowledged *actions.CommandResponse) *actions.CommandResponse {
	ctx, cancel := s.apiProofContext(ctx)
	defer cancel()
	subscription, err := s.router.host.SubscribeAction(id)
	if err != nil {
		return stoppedAPICommandProof(acknowledged)
	}
	defer subscription.Close()
	ackAttempted := false
	for {
		snapshot, err := subscription.Next(ctx)
		if err != nil {
			return stoppedAPICommandProof(acknowledged)
		}
		if snapshot.Command != id {
			return &actions.CommandResponse{CommandId: acknowledged.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
		}
		if snapshot.Status == actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED && !ackAttempted {
			ackAttempted = true
			proof := &actions.CommandResponse{CommandId: acknowledged.CommandId, Status: snapshot.Status}
			encoded, encodeErr := proto.Marshal(proof)
			if encodeErr != nil {
				return &actions.CommandResponse{CommandId: acknowledged.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
			}
			// This is already attempted native work. HTTP cancellation cannot
			// erase its evidence; the independent SQL write is bounded to 5s.
			proofCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			outcome, proofErr := s.store.AppendCommandAPIAcknowledgement(proofCtx, id, target, encoded)
			cancel()
			s.router.host.observeDurableTransaction(outcome, proofErr)
			// A failed ACK append must not stop the causal tracker. This local
			// fact is not durable status: only a committed ACK or terminal SQL
			// append can expose durable evidence. No ACK retry is scheduled.
			acknowledged = proof
		}
		if terminalAction(snapshot.Status) {
			return &actions.CommandResponse{CommandId: acknowledged.CommandId, Status: snapshot.Status}
		}
	}
}

// RecordedAPICommand reconciles a public intention before live definition and
// deadline checks. It requires the historical native read authorization and an
// exact public fingerprint; no reconstruction of a dispatcher is involved.
func (s *DurableCommandService) RecordedAPICommand(ctx context.Context, id domain.CommandID, expected storage.CommandAPIMetadata) (bool, error) {
	if s == nil || s.store == nil || s.router == nil || ctx == nil || !expected.Capability().Valid() {
		return false, ErrInvalidInput
	}
	record, found, err := s.store.CommandIntent(ctx, id)
	if err != nil {
		return false, ErrDurableLedgerUnavailable
	}
	if !found {
		return false, nil
	}
	if record.OriginKind() != storage.IntentOriginExternalActor {
		return false, ErrCommandAuthorizationDenied
	}
	target, present, err := record.AuthorizationTarget()
	if err != nil {
		return false, ErrCommandAuthorizationDenied
	}
	if err := s.router.authorizeStored(ctx, record.Fingerprint(), target, present); err != nil {
		return false, err
	}
	actual, present := record.APIMetadata()
	if !present || actual != expected {
		return false, ErrCommandIDConflict
	}
	return true, nil
}

// APICommandNames is a bounded candidate scan of the native ledger. Current
// historical read permits filter every candidate before it crosses the port.
func (s *DurableCommandService) APICommandNames(ctx context.Context, actor domain.ActorID, home apitypes.HomeName, after *apitypes.OperationName, limit uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	if s == nil || s.store == nil {
		return nil, nil, ErrInvalidInput
	}
	names, next, err := s.store.ListAPICommandNames(ctx, actor, home, after, limit, cutoff)
	if err != nil {
		return nil, nil, ErrDurableLedgerUnavailable
	}
	admitted := make([]apitypes.OperationName, 0, len(names))
	for _, name := range names {
		id, err := domain.ParseCommandID(strings.TrimPrefix(name.ID(), "cmd-"))
		if err != nil {
			return nil, nil, ErrDurableLedgerUnavailable
		}
		read, err := s.AuthorizeActionRead(ctx, id)
		if errors.Is(err, ErrCommandAuthorizationDenied) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		ready := read.Ready()
		read.Close()
		if ready {
			admitted = append(admitted, name)
		}
	}
	return admitted, next, nil
}

func (s *DurableCommandService) APICommandsByRequest(ctx context.Context, actor domain.ActorID, home apitypes.HomeName, key apitypes.RequestID, cutoff time.Time) ([]apitypes.OperationName, error) {
	if s == nil || s.store == nil {
		return nil, ErrInvalidInput
	}
	names, err := s.store.FindAPICommandsByRequest(ctx, actor, home, key, cutoff)
	if err != nil {
		return nil, ErrDurableLedgerUnavailable
	}
	admitted := make([]apitypes.OperationName, 0, len(names))
	for _, name := range names {
		id, err := domain.ParseCommandID(strings.TrimPrefix(name.ID(), "cmd-"))
		if err != nil {
			return nil, ErrDurableLedgerUnavailable
		}
		read, err := s.AuthorizeActionRead(ctx, id)
		if errors.Is(err, ErrCommandAuthorizationDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ready := read.Ready()
		read.Close()
		if ready {
			admitted = append(admitted, name)
		}
	}
	return admitted, nil
}

// An admitted occurrence that cannot continue leaves a durable terminal fact
// whenever SQL is reachable. This never changes an existing final proof and
// never retries a physical effect. Failed persistence remains for recovery.
func (s *DurableCommandService) finalizeAbandonedAPICommand(id domain.CommandID) {
	key, _ := id.Value()
	cancelled, _ := proto.Marshal(&actions.CommandResponse{CommandId: key, Status: actions.ActionStatus_ACTION_STATUS_CANCELLED})
	uncertain, _ := proto.Marshal(&actions.CommandResponse{CommandId: key, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	moved, outcome, err := s.store.MoveCommandIntent(ctx, id, storage.IntentAccepted, storage.IntentResult, cancelled, storage.IntentCanceled)
	s.router.host.observeDurableTransaction(outcome, err)
	if moved {
		return
	}
	moved, outcome, err = s.store.MoveCommandIntent(ctx, id, storage.IntentDispatching, storage.IntentIndeterminate, uncertain, storage.IntentOutcomeIndeterminate)
	s.router.host.observeDurableTransaction(outcome, err)
	if moved {
		return
	}
	outcome, err = s.store.AppendCommandAPITerminal(ctx, id, uncertain)
	s.router.host.observeDurableTransaction(outcome, err)
}

// Only a strictly admitted ACK for this exact native command may refine pending
// durable progress. The immutable terminal proof, when present, takes precedence.
func apiAcknowledgedResponse(id domain.CommandID, record storage.IntentRecord) (*actions.CommandResponse, error) {
	encoded := record.APIAcknowledgedResponse()
	if len(encoded) == 0 {
		return nil, nil
	}
	key, err := id.Value()
	if err != nil {
		return nil, ErrDurableLedgerUnavailable
	}
	response := &actions.CommandResponse{}
	if proto.Unmarshal(encoded, response) != nil || response.CommandId != key || response.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED || response.Error != nil || len(response.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrDurableLedgerUnavailable
	}
	return response, nil
}
