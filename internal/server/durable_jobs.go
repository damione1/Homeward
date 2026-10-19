package server

import (
	"context"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/storage"
)

// DurableJobCommandInput borrows the current configuration/epoch authorities.
// Supplying this input opts into real-session grants; system-only backup jobs
// need no command handler or durable command runtime of their own.
type DurableJobCommandInput struct {
	Generation integrations.GenerationAuthority
	Epoch      integrations.CommandEpochAuthority
}

type DurableJobsInput struct {
	Commands  *DurableJobCommandInput
	Backup    jobs.BackupHandler
	Clock     jobs.Clock
	Timer     jobs.Timer
	Configure func(context.Context, DurableJobsServices) error
}

// DurableJobsComposition is admitted trusted runtime configuration. It exposes
// no RPC, household schedule, default handler or implicit durable script wait.
// Its native-session grant is not an admission surface for a scoped/MCP caller
// that happens to know a SessionID; that ceiling-preserving grant is out of scope.
type DurableJobsComposition struct {
	generation integrations.GenerationAuthority
	epoch      integrations.CommandEpochAuthority
	backup     jobs.BackupHandler
	clock      jobs.Clock
	timer      jobs.Timer
	configure  func(context.Context, DurableJobsServices) error
}

func NewDurableJobsComposition(input DurableJobsInput) (*DurableJobsComposition, error) {
	if nilReactiveInterface(input.Clock) || nilReactiveInterface(input.Timer) ||
		(input.Commands == nil && input.Backup == nil) || (input.Backup != nil && nilReactiveInterface(input.Backup)) {
		return nil, integrations.ErrInvalidInput
	}
	composition := &DurableJobsComposition{backup: input.Backup, clock: input.Clock, timer: input.Timer, configure: input.Configure}
	if input.Commands != nil {
		if nilReactiveInterface(input.Commands.Generation) || nilReactiveInterface(input.Commands.Epoch) {
			return nil, integrations.ErrInvalidInput
		}
		composition.generation, composition.epoch = input.Commands.Generation, input.Commands.Epoch
	}
	return composition, nil
}

// DurableJobsServices is the same service set the server owns. A future command
// façade may borrow Commands rather than constructing a second NHA-024 runtime.
// A composition that does construct another is rejected by Host's existing
// ErrDurableRuntimeBusy guard before a second DB guard can be acquired.
type DurableJobsServices struct {
	repository *jobs.Repository
	commands   *integrations.DurableCommandService
	scheduler  *jobs.Scheduler
}

func (s DurableJobsServices) Schedule(ctx context.Context, definition jobs.Definition, occurrence jobs.OccurrenceID, key jobs.IdempotencyKey) (jobs.CreateResult, storage.TransactionOutcome, error) {
	if s.scheduler == nil {
		return jobs.CreateUnspecified, storage.TransactionNotCommitted, integrations.ErrInvalidInput
	}
	return s.scheduler.Schedule(ctx, definition, occurrence, key)
}
func (s DurableJobsServices) Get(ctx context.Context, occurrence jobs.OccurrenceID) (jobs.Record, bool, error) {
	if s.repository == nil {
		return jobs.Record{}, false, integrations.ErrInvalidInput
	}
	return s.repository.Get(ctx, occurrence)
}
func (s DurableJobsServices) Commands() (*integrations.DurableCommandService, bool) {
	return s.commands, s.commands != nil
}
func (s DurableJobsServices) Status() jobs.SchedulerStatus { return s.scheduler.Status() }

type durableJobsRuntime struct {
	scheduler *jobs.Scheduler
	services  DurableJobsServices
}

func newDurableJobsRuntime(ctx context.Context, store *storage.Store, host *integrations.Manager, sessions *sessionCoordinator, composition *DurableJobsComposition) (*durableJobsRuntime, error) {
	if composition == nil {
		return nil, nil
	}
	if nilReactiveInterface(ctx) || store == nil || host == nil || sessions == nil || sessions.store != store || sessions.authority == nil || nilReactiveInterface(composition.clock) || nilReactiveInterface(composition.timer) {
		return nil, integrations.ErrInvalidInput
	}
	repository, err := jobs.NewRepository(store)
	if err != nil {
		return nil, err
	}
	services := DurableJobsServices{repository: repository}
	var command *jobs.CommandHandler
	if composition.generation != nil {
		services.commands, err = integrations.NewDurableCommandServiceWithClock(ctx, host, composition.generation, store, composition.clock)
		if err != nil {
			return nil, err
		}
		command, err = jobs.NewCommandHandler(services.commands, &jobSessionAdmission{store, sessions}, composition.epoch, composition.clock)
		if err != nil {
			return nil, err
		}
	}
	executor, err := jobs.NewExecutor(command, composition.backup, composition.clock)
	if err != nil {
		return nil, err
	}
	scheduler, err := jobs.NewScheduler(repository, executor, composition.clock, composition.timer)
	if err != nil {
		return nil, err
	}
	services.scheduler = scheduler
	runtime := &durableJobsRuntime{scheduler, services}
	if composition.configure != nil {
		if err := composition.configure(ctx, services); err != nil {
			scheduler.CloseAdmissions()
			return runtime, err
		}
	}
	return runtime, nil
}

func (r *durableJobsRuntime) Start(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.scheduler.Start(ctx)
}
func (r *durableJobsRuntime) CloseAdmissions() {
	if r != nil {
		r.scheduler.CloseAdmissions()
	}
}
func (r *durableJobsRuntime) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.scheduler.Done()
}
func (r *durableJobsRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.scheduler.Close(ctx)
}

type jobSessionAdmission struct {
	store       *storage.Store
	coordinator *sessionCoordinator
}

// Admit takes the existing 050/141 account gate before SQL and releases it
// before NHA-024. No SQL or callback runs under Host/generation/permit locks.
// A true persisted SessionID is checked again on every execution/restart; its
// session expiry and revocation remain limits on this first grant variant.
func (a *jobSessionAdmission) Admit(ctx context.Context, grant jobs.SessionGrant) (*jobs.AdmissionLease, error) {
	if a == nil || a.store == nil || a.coordinator == nil || nilReactiveInterface(ctx) {
		return nil, errSessionUnauthenticated
	}
	actor, session, err := grant.Values()
	if err != nil {
		return nil, errSessionUnauthenticated
	}
	text, err := actor.Value()
	if err != nil {
		return nil, errSessionUnauthenticated
	}
	account, err := contracts.ParseAccountID(text)
	if err != nil {
		return nil, errSessionUnauthenticated
	}
	sqlCtx, sqlCancel := context.WithTimeout(ctx, jobs.SQLTimeout)
	defer sqlCancel()
	guard, err := a.coordinator.enter(sqlCtx, account)
	if err != nil {
		return nil, err
	}
	defer guard.release()
	owner := storage.SessionOwner{Account: account, Session: session}
	current, err := a.store.AdmitSessionGrant(sqlCtx, owner, a.coordinator.clock.Now())
	if err != nil || !a.coordinator.clock.Now().Before(current.ExpiresAt()) {
		return nil, errSessionUnauthenticated
	}
	if err := guard.refreshAuthorization(sqlCtx); err != nil {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
		return nil, errSessionUnauthenticated
	}
	principal, err := authorization.NativePrincipal(account, session)
	if err != nil {
		return nil, errSessionUnauthenticated
	}
	leaseCtx, lease := guard.register(ctx, owner)
	leaseCtx, err = authorization.WithPrincipal(leaseCtx, principal)
	if err != nil {
		lease.release()
		return nil, err
	}
	timer := a.coordinator.clock.At(current.ExpiresAt(), func() {
		lease.terminate(authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED)
	})
	if timer == nil || typedNilPublic(timer) || leaseCtx.Err() != nil {
		if timer != nil && !typedNilPublic(timer) {
			timer.Stop()
		}
		lease.release()
		return nil, errSessionUnauthenticated
	}
	admitted, err := jobs.NewAdmissionLease(leaseCtx, func() { timer.Stop(); lease.release() })
	if err != nil {
		timer.Stop()
		lease.release()
		return nil, err
	}
	return admitted, nil
}
