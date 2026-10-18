package integrations

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

var (
	ErrCommandIDConflict           = errors.New("command ID already belongs to another intention in this epoch")
	ErrVolatileLedgerFull          = errors.New("volatile command ledger is full")
	ErrCommandIntentTooLarge       = errors.New("command intention exceeds the volatile ledger limit")
	ErrVolatileServiceAttached     = errors.New("a volatile command service is already attached to this Host")
	ErrCommandOutcomeIndeterminate = errors.New("command outcome is indeterminate")
	ErrCommandResultUnavailable    = errors.New("command result failed before driver entry")
)

const (
	maxVolatileEntries       = 65536
	maxVolatileIntentBytes   = 64 << 10
	maxVolatileRetainedBytes = 128 << 20
	volatileResponseReserve  = 1024
	volatileEntryOverhead    = 512
)

// VolatileCommandLimits bounds the number and retained bytes of intentions.
// Entries, including expired and pending ones, remain until this Host ends.
type VolatileCommandLimits struct {
	entries       int
	intentBytes   int
	retainedBytes int
}

func DefaultVolatileCommandLimits() VolatileCommandLimits {
	limits, _ := NewVolatileCommandLimits(16384, 64<<10, 64<<20)
	return limits
}

func NewVolatileCommandLimits(entries, intentBytes, retainedBytes int) (VolatileCommandLimits, error) {
	if entries < 1 || entries > maxVolatileEntries || intentBytes < 1 || intentBytes > maxVolatileIntentBytes ||
		retainedBytes < intentBytes+volatileResponseReserve+volatileEntryOverhead || retainedBytes > maxVolatileRetainedBytes {
		return VolatileCommandLimits{}, ErrInvalidInput
	}
	return VolatileCommandLimits{entries: entries, intentBytes: intentBytes, retainedBytes: retainedBytes}, nil
}

type volatileState uint8

const (
	volatilePending volatileState = iota + 1
	volatileCompleted
	volatileIndeterminate
)

type volatileEntry struct {
	fingerprint []byte
	done        chan struct{}
	state       volatileState
	response    *actions.CommandResponse
	err         error
}

type volatileCommandLedger struct {
	mu       sync.Mutex
	epoch    string
	limits   VolatileCommandLimits
	retained int
	entries  map[domain.CommandID]*volatileEntry
}

// VolatileCommandService is the server-facing command API for this Host epoch.
// Its Dispatch and every CommandRouter attached to the same Host share one
// ledger. The future runtime must construct this service before serving calls.
type VolatileCommandService struct{ router *CommandRouter }

// VolatileCommandStatus is a snapshot of this Host's reserved memory budget.
// Full ledgers refuse new IDs; retained IDs are never silently evicted.
type VolatileCommandStatus struct {
	Entries          int
	RetainedBytes    int
	MaxEntries       int
	MaxRetainedBytes int
	MaxIntentBytes   int
}

func (s *VolatileCommandService) Status() (VolatileCommandStatus, error) {
	if s == nil || s.router == nil || s.router.host == nil {
		return VolatileCommandStatus{}, ErrInvalidInput
	}
	s.router.host.mu.Lock()
	ledger := s.router.host.volatile
	s.router.host.mu.Unlock()
	if ledger == nil {
		return VolatileCommandStatus{}, ErrInvalidInput
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return VolatileCommandStatus{len(ledger.entries), ledger.retained, ledger.limits.entries, ledger.limits.retainedBytes, ledger.limits.intentBytes}, nil
}

func NewVolatileCommandService(host *Manager, generation GenerationAuthority, limits VolatileCommandLimits) (*VolatileCommandService, error) {
	return NewVolatileCommandServiceWithClock(host, generation, realCommandClock{}, limits)
}

func NewVolatileCommandServiceWithClock(host *Manager, generation GenerationAuthority, clock CommandClock, limits VolatileCommandLimits) (*VolatileCommandService, error) {
	if limits == (VolatileCommandLimits{}) {
		return nil, ErrInvalidInput
	}
	if _, err := NewVolatileCommandLimits(limits.entries, limits.intentBytes, limits.retainedBytes); err != nil {
		return nil, err
	}
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
	ledger := &volatileCommandLedger{epoch: epoch, limits: limits, entries: make(map[domain.CommandID]*volatileEntry)}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.volatile != nil {
		return nil, ErrVolatileServiceAttached
	}
	if host.unprotectedCommandDispatch {
		return nil, ErrVolatileServiceAttached
	}
	host.volatile = ledger
	return &VolatileCommandService{router: router}, nil
}

func (s *VolatileCommandService) Dispatch(ctx context.Context, actor domain.ActorID, request *actions.CommandRequest) (*actions.CommandResponse, error) {
	if s == nil || s.router == nil {
		return nil, ErrInvalidInput
	}
	return s.router.Dispatch(ctx, actor, request)
}

func (l *volatileCommandLedger) dispatch(router *CommandRouter, ctx context.Context, actor domain.ActorID, request *actions.CommandRequest) (*actions.CommandResponse, error) {
	if ctx == nil || request == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority := router.projection.(CommandEpochAuthority) // checked at service construction
	current, err := authority.CurrentAdmissionEpoch()
	if err != nil || current == "" || current != l.epoch {
		return commandFailure(actions.ErrorCode_ERROR_CODE_STALE_EPOCH, "admission_epoch"), nil
	}
	if request.AdmissionEpoch == "" {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "admission_epoch"), nil
	}
	if request.AdmissionEpoch != current {
		return commandFailure(actions.ErrorCode_ERROR_CODE_STALE_EPOCH, "admission_epoch"), nil
	}
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command_id"), nil
	}
	// Reject oversized input before cloning or encoding it. The exact budget
	// check below also includes the trusted actor and length prefix.
	if proto.Size(request) > l.limits.intentBytes {
		l.mu.Lock()
		_, exists := l.entries[id]
		l.mu.Unlock()
		if exists {
			// An admitted envelope was below this limit. A larger one with
			// its ID is therefore necessarily a different intention.
			return nil, ErrCommandIDConflict
		}
		return nil, ErrCommandIntentTooLarge
	}
	wire := proto.Clone(request).(*actions.CommandRequest)
	actorText, err := actor.Value()
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "actor"), nil
	}
	fingerprint, err := commandFingerprint(actorText, wire)
	if err != nil {
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "command"), nil
	}
	cost := len(fingerprint) + len(wire.CommandId) + volatileResponseReserve + volatileEntryOverhead
	validDeadline := wire.ExpiresAt != nil && wire.ExpiresAt.CheckValid() == nil && wire.ExpiresAt.AsTime().After(router.clock.Now())
	if err := router.host.claimCommandPolicy(id, commandPolicyVolatile); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if entry := l.entries[id]; entry != nil {
		if !bytes.Equal(entry.fingerprint, fingerprint) {
			l.mu.Unlock()
			return nil, ErrCommandIDConflict
		}
		l.mu.Unlock()
		if !validDeadline {
			return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
		}
		deadline := time.NewTimer(time.Until(wire.ExpiresAt.AsTime()))
		defer deadline.Stop()
		select {
		case <-entry.done:
			return cloneVolatileResult(entry)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
		}
	}
	if !validDeadline {
		l.mu.Unlock()
		return commandFailure(actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expires_at"), nil
	}
	if len(fingerprint) > l.limits.intentBytes {
		l.mu.Unlock()
		return nil, ErrCommandIntentTooLarge
	}
	if len(l.entries) >= l.limits.entries || cost > l.limits.retainedBytes-l.retained {
		l.mu.Unlock()
		return nil, ErrVolatileLedgerFull
	}
	entry := &volatileEntry{fingerprint: fingerprint, done: make(chan struct{}), state: volatilePending}
	l.entries[id] = entry
	l.retained += cost
	l.mu.Unlock()

	return l.runFirst(entry, router, ctx, actor, wire)
}

// commandFingerprint is shared by volatile and durable admission. It uses the
// original request, including oneof and optional presence, and the trusted actor.
func commandFingerprint(actorText string, wire *actions.CommandRequest) ([]byte, error) {
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wire)
	if err != nil {
		return nil, err
	}
	fingerprint := binary.AppendUvarint(nil, uint64(len(actorText)))
	fingerprint = append(fingerprint, actorText...)
	return append(fingerprint, encoded...), nil
}

func (l *volatileCommandLedger) runFirst(entry *volatileEntry, router *CommandRouter, ctx context.Context, actor domain.ActorID, wire *actions.CommandRequest) (response *actions.CommandResponse, dispatchErr error) {
	// Completion is guaranteed even if an extension codec or pilot panics. A
	// panic is conservatively indeterminate; the original panic then propagates.
	defer func() {
		panicValue := recover()
		if panicValue != nil {
			response = &actions.CommandResponse{CommandId: wire.CommandId, Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
			dispatchErr = ErrCommandOutcomeIndeterminate
		}
		response = boundedVolatileResponse(response)
		dispatchErr = boundedVolatileError(response, dispatchErr)
		l.mu.Lock()
		entry.response, entry.err = response, dispatchErr
		if response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			entry.state = volatileIndeterminate
		} else {
			entry.state = volatileCompleted
		}
		close(entry.done)
		l.mu.Unlock()
		response, dispatchErr = cloneVolatileResult(entry)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	return router.dispatchOnce(ctx, actor, wire)
}

func cloneVolatileResult(entry *volatileEntry) (*actions.CommandResponse, error) {
	switch entry.state {
	case volatileCompleted, volatileIndeterminate:
		if entry.response == nil {
			return nil, entry.err
		}
		return proto.Clone(entry.response).(*actions.CommandResponse), entry.err
	case volatilePending:
		return nil, ErrInvalidInput
	default:
		return nil, ErrInvalidInput
	}
}

func boundedVolatileResponse(response *actions.CommandResponse) *actions.CommandResponse {
	if response == nil {
		return nil
	}
	if proto.Size(response) <= volatileResponseReserve {
		return proto.Clone(response).(*actions.CommandResponse)
	}
	bounded := &actions.CommandResponse{CommandId: response.CommandId, Status: response.Status}
	if response.Error != nil {
		bounded.Error = &actions.ContractError{Code: response.Error.Code, FieldPath: "command", Message: "command result detail exceeded ledger limit"}
	}
	return bounded
}

func boundedVolatileError(response *actions.CommandResponse, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrSupervisedPilotPanic) {
		return ErrSupervisedPilotPanic
	}
	if response != nil && response.Status == actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		return ErrCommandOutcomeIndeterminate
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrCommandRouteBusy, ErrCommandRouteUnavailable} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrCommandResultUnavailable
}
