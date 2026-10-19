package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Outcome uint8

const (
	OutcomeUnspecified Outcome = iota
	Completed
	Refused
	Uncertain
)

func (o Outcome) state() State {
	switch o {
	case Completed:
		return Succeeded
	case Refused:
		return Rejected
	case Uncertain, OutcomeUnspecified:
		return Indeterminate
	default:
		return Indeterminate
	}
}

// Attempt is immutable occurrence data, not a driver admission. A handler
// cannot obtain one from a zero Claim or supply a different idempotency key.
type Attempt struct{ record Record }

func (a Attempt) Record() Record { return a.record }
func (a Attempt) valid() bool    { return (Claim{a.record}).valid() }

// ExecutionPort serves both command and nonphysical system work. Returning
// Completed means this handler completed its durable dispatch contract; it
// never upgrades a command's ACK to physical confirmation.
type ExecutionPort interface {
	Execute(context.Context, Attempt) (Outcome, error)
}

// AdmissionLease is produced only after the current grant has been checked.
// The composing authority owns credential revocation and expiry cancellation.
type AdmissionLease struct {
	ctx       context.Context
	principal authorization.Principal
	release   func()
	once      sync.Once
}

func NewAdmissionLease(ctx context.Context, release func()) (*AdmissionLease, error) {
	if nilValue(ctx) || release == nil {
		return nil, failure(Invalid)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, failure(Invalid)
	}
	if _, err := principal.Actor(); err != nil || !principal.Native() {
		return nil, failure(Invalid)
	}
	return &AdmissionLease{ctx: ctx, principal: principal, release: release}, nil
}
func (a *AdmissionLease) Close() {
	if a != nil && a.release != nil {
		a.once.Do(a.release)
	}
}

func (a *AdmissionLease) Context() (context.Context, error) {
	if a == nil || nilValue(a.ctx) || a.release == nil {
		return nil, failure(Invalid)
	}
	return a.ctx, nil
}

// CommandAdmission is a trusted, current-credential boundary. SessionGrant is
// the first grant variant; future grant kinds must be admitted explicitly at
// their own authority, rather than fabricating a native session for ActorID.
type CommandAdmission interface {
	Admit(context.Context, SessionGrant) (*AdmissionLease, error)
}

type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

type CommandHandler struct {
	service   *integrations.DurableCommandService
	admission CommandAdmission
	epoch     integrations.CommandEpochAuthority
	clock     Clock
}

func NewCommandHandler(service *integrations.DurableCommandService, admission CommandAdmission, epoch integrations.CommandEpochAuthority, clock Clock) (*CommandHandler, error) {
	if service == nil || nilValue(admission) || nilValue(epoch) || nilValue(clock) {
		return nil, failure(Invalid)
	}
	return &CommandHandler{service, admission, epoch, clock}, nil
}

func (h *CommandHandler) valid() bool {
	return h != nil && h.service != nil && !nilValue(h.admission) && !nilValue(h.epoch) && !nilValue(h.clock)
}

func (h *CommandHandler) execute(ctx context.Context, attempt Attempt) (Outcome, error) {
	if !h.valid() || !attempt.valid() || nilValue(ctx) {
		return Refused, failure(Invalid)
	}
	definition := attempt.record.definition
	grant, wire, err := definition.work.SessionCommand()
	if err != nil || !h.clock.Now().Before(definition.expires) {
		return Refused, failure(Invalid)
	}
	lease, err := h.admission.Admit(ctx, grant)
	if err != nil {
		return Refused, err
	}
	if lease == nil {
		return Refused, failure(Invalid)
	}
	defer lease.Close()
	actor, actorErr := lease.principal.Actor()
	if actorErr != nil || actor != grant.owner || nilValue(lease.ctx) || lease.ctx.Err() != nil {
		return Refused, failure(Invalid)
	}
	epoch, err := h.epoch.CurrentAdmissionEpoch()
	if err != nil || epoch == "" || !h.clock.Now().Before(definition.expires) {
		return Refused, failure(Unavailable)
	}
	// All other intent fields, including config/expected revisions and stable
	// entity target, stay unchanged. Current catalogue/owner/state checks and
	// authorization permit fencing remain inside NHA-024's real route.
	wire.CommandId, _ = commandID(attempt.record.occurrence).Value()
	wire.AdmissionEpoch = epoch
	wire.ExpiresAt = timestamppb.New(definition.expires)
	response, err := h.service.Dispatch(lease.ctx, actor, wire)
	if response == nil {
		if errors.Is(err, integrations.ErrCommandAuthorizationDenied) || errors.Is(err, integrations.ErrInvalidInput) {
			return Refused, err
		}
		// An error without a ledger response is not evidence that no effect ran.
		return Uncertain, err
	}
	switch response.Status {
	case actions.ActionStatus_ACTION_STATUS_DISPATCHED,
		actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED,
		actions.ActionStatus_ACTION_STATUS_CONFIRMED:
		return Completed, err
	case actions.ActionStatus_ACTION_STATUS_FAILED, actions.ActionStatus_ACTION_STATUS_CANCELLED:
		return Refused, err
	case actions.ActionStatus_ACTION_STATUS_ACCEPTED, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, actions.ActionStatus_ACTION_STATUS_UNSPECIFIED:
		return Uncertain, err
	default:
		return Uncertain, failure(Invalid)
	}
}

// BackupHandler is an explicitly internal system authority. Its implementation
// must atomically deduplicate by the supplied key; an error after a possible
// effect returns Uncertain. The scheduler itself never retries dispatching work.
type BackupHandler interface {
	ExecuteBackup(context.Context, BackupKey, IdempotencyKey) (Outcome, error)
}

type Executor struct {
	command *CommandHandler
	backup  BackupHandler
	clock   Clock
}

func NewExecutor(command *CommandHandler, backup BackupHandler, clock Clock) (*Executor, error) {
	if nilValue(clock) || (command == nil && backup == nil) || (command != nil && !command.valid()) || (backup != nil && nilValue(backup)) {
		return nil, failure(Invalid)
	}
	return &Executor{command, backup, clock}, nil
}

func (e *Executor) Execute(ctx context.Context, attempt Attempt) (Outcome, error) {
	if e == nil || nilValue(e.clock) || nilValue(ctx) || ctx.Err() != nil || !attempt.valid() {
		return Refused, failure(Invalid)
	}
	if !e.clock.Now().Before(attempt.record.definition.expires) {
		return Refused, failure(Invalid)
	}
	switch attempt.record.definition.work.kind {
	case SessionCommand:
		if e.command == nil {
			return Refused, failure(Invalid)
		}
		return e.command.execute(ctx, attempt)
	case SystemBackup:
		if e.backup == nil {
			return Refused, failure(Invalid)
		}
		key, err := attempt.record.definition.work.Backup()
		if err != nil {
			return Refused, err
		}
		outcome, err := e.backup.ExecuteBackup(ctx, key, attempt.record.key)
		switch outcome {
		case Completed, Refused, Uncertain:
			return outcome, err
		case OutcomeUnspecified:
			return Uncertain, failure(Invalid)
		default:
			return Uncertain, failure(Invalid)
		}
	case WorkUnspecified:
		return Refused, failure(Invalid)
	default:
		return Refused, failure(Invalid)
	}
}
