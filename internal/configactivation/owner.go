package configactivation

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
)

type ownerPhase uint8

const (
	ownerInvalid ownerPhase = iota
	ownerOpen
	ownerCut
	ownerClosed
)

type publicationStep uint8

const (
	publicationReserved publicationStep = iota
	publicationTransferred
	publicationPilotsJoined
	publicationForwardingPaused
	publicationHost
	publicationRegistry
	publicationPolicies
	publicationOperational
	publicationScenes
	publicationTemplates
	publicationTriggers
	publicationPilots
	publicationScheduled
)

type operationalBasis struct {
	operational config.Config
	legacy      bundle.Candidate
}
type programResolution struct {
	revision storage.ConfigGenerationRevision
	manifest [32]byte
	image    ProgramImage
}

type activationTransfer struct {
	basis                 operationalBasis
	target                AdmittedGeneration
	forced                []domain.IntegrationInstanceID
	cohort                []contracts.AccountID
	reconciling           bool
	reservationsReady     bool
	triggersPrepared      bool
	operationalSuperseded bool
	resolution            *programResolution
	prepared              preparedActivation
	request               Request
	old                   sequencer.Snapshot
	operational           OperationalImage
	runs                  *automationruns.ImageReservation
	templates             *reactivetemplate.PlanReservation
	scenes                *scenes.CatalogueReservation
	pilots                *integrations.PilotReplacement
	policies              PolicyReservation
	declarations          *integrations.ConfigurationDeclarationsLease
	registry              *registry.ActivationGate
	pause                 *integrations.ActivationPause
	cut                   *sequencer.ConfigurationPublication
	outcome               storage.TransactionOutcome
	step                  publicationStep
	admitted              AdmittedGeneration
	forwardingPaused      bool
	runsPublished         bool
}

// Owner serializes the actual prepare/write/transfer/publication path. A
// pending transfer owns its gates until exact-attempt reconciliation. Run
// joins Done before closing Store; no timeout starts an orphan SQL worker.
type Owner struct {
	mu       sync.RWMutex
	input    OwnerInput
	replay   *storage.ConfigReplayRepository
	runtime  Runtime
	attached bool
	current  AdmittedGeneration
	phase    ownerPhase
	active   bool
	pending  *activationTransfer
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	done     chan struct{}
	subs     map[chan struct{}]struct{}
}

func NewOwner(input OwnerInput) (*Owner, error) {
	if input.Store == nil || !input.Startup.Valid() || !input.Current.Valid() || input.Registry == nil ||
		input.Projection == nil || input.Timeout <= 0 || input.Timeout > time.Minute {
		return nil, ErrInvalidStartup
	}
	if err := input.Startup.ConfirmBootstrapSource(); err != nil {
		return nil, err
	}
	replay, err := storage.NewConfigReplayRepository(input.Store, input.Current.secrets)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	o := &Owner{input: input, replay: replay, current: input.Current, phase: ownerOpen,
		ctx: ctx, cancel: cancel, done: make(chan struct{}), subs: make(map[chan struct{}]struct{})}
	go func() {
		<-ctx.Done()
		o.wg.Wait()
		o.mu.Lock()
		pending := o.pending
		o.pending = nil
		o.closeSubscriptionsLocked()
		o.mu.Unlock()
		if pending != nil {
			o.disposeClosed(pending)
		}
		close(o.done)
	}()
	return o, nil
}

// Attach is called once by Run, after the actual owners exist and before its
// trusted activation composition receives this Owner.
func (o *Owner) Attach(runtime Runtime) error {
	if o == nil || runtime.Host == nil || runtime.Supervisor == nil || absentPort(runtime.Policies) ||
		(runtime.Templates == nil) != absentPort(runtime.Forwarding) ||
		runtime.Forwarding != nil && absentPort(runtime.Forwarding) ||
		runtime.Operational != nil && absentPort(runtime.Operational) ||
		runtime.Publication != nil && absentPort(runtime.Publication) ||
		runtime.Runs != nil && (runtime.Commands == nil || runtime.Templates == nil) {
		return ErrInvalidStartup
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.phase != ownerOpen || o.attached || o.active || o.pending != nil {
		return ErrInvalidStartup
	}
	o.runtime, o.attached = runtime, true
	return nil
}

func (o *Owner) WithCurrent(ctx context.Context, visit func(uint64) error) error {
	if o == nil || absentPort(ctx) || visit == nil {
		return ErrInvalidStartup
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.phase != ownerOpen {
		return ErrActivationUnavailable
	}
	return visit(uint64(o.current.Revision().Number()))
}
func (o *Owner) CurrentConfig(ctx context.Context) (view.ConfigStamp, error) {
	if o == nil || absentPort(ctx) {
		return view.ConfigStamp{}, ErrInvalidStartup
	}
	if err := ctx.Err(); err != nil {
		return view.ConfigStamp{}, err
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.phase != ownerOpen {
		return view.ConfigStamp{}, ErrActivationUnavailable
	}
	return view.NewConfigStamp(uint64(o.current.Revision().Number()), o.current.ManifestSHA256())
}
func (o *Owner) SubscribeConfigChanges(ctx context.Context) (<-chan struct{}, func(), error) {
	if o == nil || absentPort(ctx) {
		return nil, nil, ErrInvalidStartup
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.phase != ownerOpen || len(o.subs) >= 1024 {
		return nil, nil, ErrActivationUnavailable
	}
	changed := make(chan struct{}, 1)
	o.subs[changed] = struct{}{}
	var once sync.Once
	cancel := func() { once.Do(func() { o.mu.Lock(); delete(o.subs, changed); o.mu.Unlock() }) }
	return changed, cancel, nil
}
func (o *Owner) notifyLocked() {
	for changed := range o.subs {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}
func (o *Owner) closeSubscriptionsLocked() {
	for changed := range o.subs {
		close(changed)
		delete(o.subs, changed)
	}
}

func (o *Owner) operation(ctx context.Context, reconcile bool) (context.Context, func(), error) {
	if o == nil || absentPort(ctx) {
		return nil, nil, ErrInvalidStartup
	}
	if o.input.Startup.MQTTReadOnlyObservation() {
		return nil, nil, integrations.ErrCommandAuthorizationDenied
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	o.mu.Lock()
	if o.phase == ownerClosed || !o.attached {
		o.mu.Unlock()
		return nil, nil, ErrActivationUnavailable
	}
	if o.active || (!reconcile && o.pending != nil) || (reconcile && o.pending == nil) {
		o.mu.Unlock()
		return nil, nil, ErrActivationBusy
	}
	o.active = true
	o.wg.Add(1)
	o.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, o.input.Timeout)
	stop := context.AfterFunc(o.ctx, cancel)
	finish := func() {
		stop()
		cancel()
		o.mu.Lock()
		o.active = false
		o.mu.Unlock()
		o.wg.Done()
	}
	return bounded, finish, nil
}

func (o *Owner) Activate(ctx context.Context, request Request) (Result, error) {
	if !request.admitted {
		return Result{outcome: RejectedBeforeCommit}, ErrInvalidStartup
	}
	bounded, finish, err := o.operation(ctx, false)
	if err != nil {
		return Result{outcome: RejectedBeforeCommit}, err
	}
	defer finish()
	return o.activate(ctx, bounded, request)
}

func (o *Owner) activate(ctx, bounded context.Context, request Request) (Result, error) {
	if err := o.input.Startup.ConfirmBootstrapSource(); err != nil {
		return Result{outcome: RestartRequired}, err
	}
	o.mu.RLock()
	current := o.current
	o.mu.RUnlock()
	var contextErr error
	if request.sourceReservations == nil {
		request, contextErr = o.reserveSourceContext(bounded, request, current)
		if contextErr != nil {
			return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, contextErr
		}
		reserved := request.sourceReservations
		defer func() { _ = o.releaseSourceContextReservations(reserved) }()
	}
	request, contextErr = o.admitEditableSourceContext(bounded, request, current)
	if contextErr != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, contextErr
	}
	prepared, err := prepareWorkspaceActivation(bounded, o.input.Store, o.input.Startup, current,
		request.definitions, request.author, request.source, request.policies)
	if err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	prepared, err = o.prepareEditableSources(bounded, current, prepared, request)
	if err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	prepared.confirmSourceFiles, prepared.confirmSourceVersions = request.confirmSourceFiles, request.confirmSourceVersions
	if prepared.confirmSourceFiles != nil {
		if err := prepared.confirmSourceFiles(); err != nil {
			return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
		}
	}
	if err := request.confirmWorkspacePrecondition(current, prepared); err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	t := &activationTransfer{prepared: prepared, request: request, outcome: storage.TransactionNotCommitted, basis: operationalBasis{current.operational, current.legacy}}
	o.adoptSourceContextReservations(t, request)
	if err = o.reserve(bounded, t); err != nil {
		o.releaseUnwritten(t)
		return Result{outcome: RejectedBeforeCommit, attempt: prepared.attempt}, err
	}
	if t.declarations != nil {
		prepared.confirmDeclarations = t.declarations.Confirm
		t.prepared.confirmDeclarations = prepared.confirmDeclarations
	}
	o.mu.Lock()
	if o.phase == ownerClosed {
		o.mu.Unlock()
		o.releaseUnwritten(t)
		return Result{outcome: RejectedBeforeCommit, attempt: prepared.attempt}, ErrActivationUnavailable
	}
	o.pending, o.phase = t, ownerCut
	o.mu.Unlock()
	t.cut, err = o.input.Projection.BeginConfigurationPublication()
	if err != nil {
		return o.rollbackKnown(t, RejectedBeforeCommit, err)
	}
	mutate := func(writer *registry.ConfigWriter) error {
		if t.operational.registry != nil {
			if err := t.operational.registry(writer); err != nil {
				return err
			}
		}
		if request.registry != nil {
			if err := request.registry(writer); err != nil {
				return err
			}
		}
		if prepared.sourceContext != nil {
			return writer.ConfirmCompilationIdentities(*prepared.sourceContext)
		}
		return nil
	}
	_, t.outcome, err = writePreparedActivation(bounded, o.input.Store, o.replay, t.registry, prepared, mutate)
	if t.outcome == storage.TransactionNotCommitted {
		return o.rollbackKnown(t, NotCommitted, err)
	}
	if t.outcome == storage.TransactionIndeterminate {
		if t.pause != nil {
			_ = o.runtime.Commands.Quarantine(t.pause)
		}
		if t.policies != nil {
			t.policies.Quarantine()
		}
		return Result{outcome: CommitUnknown, attempt: prepared.attempt, transaction: storage.TransactionIndeterminate}, err
	}
	if t.outcome != storage.TransactionCommitted {
		return Result{outcome: CommitUnknown, attempt: prepared.attempt, transaction: storage.TransactionIndeterminate}, ErrStartupIntegrity
	}
	// Caller cancellation after a known COMMIT cannot restore A. The bounded
	// owner attempt continues publication independently of the request bearer.
	publication, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.input.Timeout)
	stop := context.AfterFunc(o.ctx, cancel)
	defer cancel()
	defer stop()
	return o.reconcile(publication, t)
}

func (o *Owner) reserve(ctx context.Context, t *activationTransfer) error {
	var err error
	if !t.old.RegistryReady {
		t.old, err = o.input.Projection.CaptureBounded(sequencer.MaxJournalBytes)
		if err != nil || !t.old.RegistryReady {
			return errors.Join(ErrActivationUnavailable, err)
		}
	}
	next, err := t.prepared.source.OperationalConfig()
	if err != nil {
		return err
	}
	definitions := t.prepared.legacy
	if t.target.Valid() {
		next, definitions = t.target.operational, t.target.legacy
	}
	if t.request.sourceContext != nil && !t.target.Valid() {
		manifest, err := o.input.Registry.ScopeManifest()
		if err != nil {
			return err
		}
		captured, err := SourceContextFromSnapshot(t.old, manifest)
		if err != nil || captured.SHA256() != t.request.sourceContext.SHA256() {
			return ErrWorkspacePreconditionConflict
		}
	}
	program := t.request.program
	var baseTemplates reactivetemplate.Plan
	var triggerPlan automationtriggers.Plan
	if program != nil {
		triggerPlan = program.triggers
	} else if o.runtime.Runs != nil && !t.operational.admitted {
		triggerPlan, err = o.runtime.Runs.CloneTriggerPlan()
		if err != nil {
			return err
		}
	}
	if program != nil {
		baseTemplates = program.templates
	} else if o.runtime.Templates != nil {
		baseTemplates, err = o.runtime.Templates.PermanentPlan()
		if err != nil {
			return err
		}
	}
	sourceImage, err := o.composeSourceProgram(ctx, t, baseTemplates)
	if err != nil {
		return err
	}
	if sourceImage.program != nil {
		program = sourceImage.program
		triggerPlan = program.triggers
		baseTemplates = program.templates
	}
	if t.reconciling && program == nil && baseTemplates.Active() {
		baseTemplates, err = baseTemplates.ReplacePermanent(nil, nil, definitions.Groups())
		if err != nil {
			return err
		}
	}
	if !t.operational.admitted && !absentPort(o.runtime.Operational) {
		var committed *registry.CommittedBindings
		if t.reconciling {
			committed, err = t.registry.CommittedBindings(ctx, t.target.Revision(), t.target.ManifestSHA256())
			if err != nil {
				return err
			}
		}
		if sourceCompiler, ok := o.runtime.Operational.(SourceOperationalCompiler); ok && sourceImage.compilation.Valid() {
			t.operational, err = sourceCompiler.PrepareOperationalSources(ctx, t.basis.operational, next, t.basis.legacy, definitions, baseTemplates, triggerPlan, t.forced, committed, sourceImage.compilation, t.registry)
		} else {
			t.operational, err = o.runtime.Operational.PrepareOperational(ctx, t.basis.operational, next, t.basis.legacy, definitions, baseTemplates, triggerPlan, t.forced, committed)
		}
		if err != nil || !t.operational.admitted {
			return errors.Join(ErrOperationalComposition, err)
		}
	} else if absentPort(o.runtime.Operational) && !reflect.DeepEqual(t.basis.operational, next) {
		return ErrOperationalComposition
	}
	if t.pilots == nil && len(t.operational.next)+len(t.operational.remove) != 0 {
		t.pilots, err = o.runtime.Supervisor.ReserveReplacement(t.operational.next, t.operational.remove)
		if err != nil {
			return err
		}
	}
	if program != nil && (o.runtime.Runs == nil || o.runtime.Templates == nil) {
		return ErrInvalidStartup
	}
	if t.operational.triggers != nil {
		triggerPlan = *t.operational.triggers
	}
	if o.runtime.Runs != nil && t.runs == nil {
		var plan automationruns.Plan
		if program != nil {
			plan, triggerPlan = program.runs, program.triggers
		}
		if t.operational.triggers != nil {
			triggerPlan = *t.operational.triggers
		}
		t.runs, err = o.runtime.Runs.ReserveProgram(plan, triggerPlan)
		if err != nil {
			return err
		}
	}
	if triggerPlan.Valid() && !t.triggersPrepared {
		if err := t.runs.PrepareTriggerPublication(); err != nil {
			return err
		}
		t.triggersPrepared = true
	}
	var templates *reactivetemplate.Plan
	if program != nil {
		templates = &program.templates
	}
	if t.operational.templates != nil {
		templates = t.operational.templates
	}
	if templates == nil && t.reconciling && baseTemplates.Active() {
		templates = &baseTemplates
	}
	if templates != nil && t.templates == nil {
		if o.runtime.Templates == nil {
			return ErrOperationalComposition
		}
		t.templates, err = o.runtime.Templates.ReservePlan(*templates)
		if err != nil {
			return err
		}
	}
	if program != nil {
		if program.scenes != nil && t.scenes == nil {
			if o.runtime.Scenes == nil {
				return ErrInvalidStartup
			}
			t.scenes, err = o.runtime.Scenes.ReserveCatalogue(*program.scenes)
			if err != nil {
				return err
			}
		}
	}
	if sourceScenes, present := sourceImage.Scenes(); present && t.scenes == nil {
		if o.runtime.Scenes == nil {
			return ErrProgramCompositionRequired
		}
		t.scenes, err = o.runtime.Scenes.ReserveCatalogue(sourceScenes)
		if err != nil {
			return err
		}
	}
	if t.policies == nil {
		if t.reconciling && len(t.cohort) != 0 {
			t.policies, err = o.runtime.Policies.ReserveReconciliationPolicies(ctx, t.cohort)
		} else if !t.reconciling && len(t.prepared.policies) != 0 {
			t.policies, err = o.runtime.Policies.ReserveActivationPolicies(ctx, t.prepared.policies)
		}
		if err != nil {
			return err
		}
		if (t.reconciling && len(t.cohort) != 0 || !t.reconciling && len(t.prepared.policies) != 0) && absentPort(t.policies) {
			return ErrInvalidStartup
		}
	}
	if t.registry == nil {
		t.registry, err = o.input.Registry.BeginActivation(ctx)
		if err != nil {
			return err
		}
	}
	if t.runs != nil {
		if err := t.runs.PauseAdmissions(ctx, o.input.Timeout); err != nil {
			return err
		}
	}
	if o.runtime.Commands != nil && t.pause == nil {
		t.pause, err = o.runtime.Commands.PauseAndDrain(ctx)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.reservationsReady = true
	return nil
}

func (o *Owner) Reconcile(ctx context.Context) (Result, error) {
	bounded, finish, err := o.operation(ctx, true)
	if err != nil {
		return Result{outcome: CommitUnknown}, err
	}
	defer finish()
	o.mu.RLock()
	t := o.pending
	o.mu.RUnlock()
	return o.reconcile(bounded, t)
}

// ReconcileWithProgram supplies the immutable 042/115 IR seam for an exact
// newer winner. It never claims to replay Logic1 from today's SQL manifest.
func (o *Owner) ReconcileWithProgram(ctx context.Context, revision storage.ConfigGenerationRevision, manifest [32]byte, image ProgramImage) (Result, error) {
	if !revision.Valid() || manifest == ([32]byte{}) || !image.admitted {
		return Result{outcome: CommitUnknown}, ErrInvalidStartup
	}
	bounded, finish, err := o.operation(ctx, true)
	if err != nil {
		return Result{outcome: CommitUnknown}, err
	}
	defer finish()
	active, present, err := o.input.Store.ReadActiveConfig(bounded)
	if err != nil || !present || active.Revision() != revision || active.ManifestSHA256() != manifest {
		return Result{outcome: CommittedReconciling}, errors.Join(ErrStartupConflict, err)
	}
	o.mu.RLock()
	t := o.pending
	o.mu.RUnlock()
	t.resolution = &programResolution{revision: revision, manifest: manifest, image: image}
	return o.reconcile(bounded, t)
}

func (o *Owner) retarget(ctx context.Context, before *activationTransfer, target AdmittedGeneration) (*activationTransfer, error) {
	program := before.request.program
	if program != nil {
		resolution := before.resolution
		if resolution == nil || resolution.revision != target.Revision() || resolution.manifest != target.ManifestSHA256() {
			return before, ErrProgramCompositionRequired
		}
		program = &resolution.image
	}
	o.mu.RLock()
	visible := o.current
	o.mu.RUnlock()
	cohort, err := o.input.Store.ReadActivationPolicyAccounts(ctx, visible.Revision(), target.Revision(), target.ManifestSHA256())
	if err != nil {
		return before, err
	}
	accounts := make(map[contracts.AccountID]bool, len(cohort)+len(before.prepared.policies))
	for _, account := range cohort {
		accounts[account] = true
	}
	for _, change := range before.prepared.policies {
		accounts[change.account] = true
	}
	if len(accounts) > 16384 {
		return before, ErrInvalidStartup
	}
	cohort = cohort[:0]
	for account := range accounts {
		cohort = append(cohort, account)
	}
	if before.operational.projection != nil && !before.operationalSuperseded {
		forced, err := before.operational.projection.Supersede(ctx)
		if err != nil {
			return before, err
		}
		before.forced = append(before.forced, forced...)
		before.operationalSuperseded = true
	}
	if before.pilots != nil {
		forced, err := before.pilots.Supersede(ctx)
		if err != nil {
			return before, err
		}
		before.forced = append(before.forced, forced...)
		before.pilots = nil
	}
	forced := make(map[domain.IntegrationInstanceID]bool, len(before.forced))
	for _, instance := range before.forced {
		forced[instance] = true
	}
	if len(forced) > 1024 {
		return before, ErrOperationalComposition
	}
	before.forced = before.forced[:0]
	for instance := range forced {
		before.forced = append(before.forced, instance)
	}
	if before.runs != nil {
		if err := before.runs.DiscardTriggerPublication(ctx); err != nil {
			return before, err
		}
		if err := before.runs.ReleaseForReplacement(); err != nil {
			return before, err
		}
		before.runs = nil
	}
	if before.templates != nil {
		_ = before.templates.Release()
		before.templates = nil
	}
	if before.scenes != nil {
		_ = before.scenes.Release()
		before.scenes = nil
	}
	if before.policies != nil {
		before.policies.Quarantine()
		before.policies.Release()
		before.policies = nil
	}
	if err := before.registry.RetargetCommitted(ctx, target.Revision(), target.ManifestSHA256()); err != nil {
		return before, err
	}
	basis := before.basis
	if before.step >= publicationOperational {
		if before.target.Valid() {
			basis = operationalBasis{before.target.operational, before.target.legacy}
		} else {
			configuration, err := before.prepared.source.OperationalConfig()
			if err != nil {
				return before, err
			}
			basis = operationalBasis{configuration, before.prepared.legacy}
		}
	}
	request := before.request
	request.program = program
	next := &activationTransfer{declarations: before.declarations, prepared: before.prepared, request: request, old: before.old,
		basis: basis, target: target, admitted: target, forced: before.forced, cohort: cohort,
		reconciling: true, registry: before.registry, pause: before.pause, cut: before.cut,
		outcome: before.outcome, forwardingPaused: before.forwardingPaused, resolution: before.resolution}
	if next.forwardingPaused {
		next.step = publicationReserved
	}
	o.mu.Lock()
	if o.phase == ownerClosed || o.pending != before {
		o.mu.Unlock()
		return before, ErrActivationUnavailable
	}
	o.pending = next
	o.mu.Unlock()
	return next, nil
}

func (o *Owner) reconcile(ctx context.Context, t *activationTransfer) (Result, error) {
	result := Result{outcome: CommitUnknown, attempt: t.prepared.attempt, transaction: storage.TransactionIndeterminate}
	attempt, present, err := o.input.Store.ReadActivationAttempt(ctx, t.prepared.attempt)
	if err != nil {
		return result, err
	}
	active, found, err := o.input.Store.ReadActiveConfig(ctx)
	if err != nil || !found {
		return result, errors.Join(ErrStartupConflict, err)
	}
	if !present {
		if t.outcome == storage.TransactionCommitted {
			return Result{outcome: CommittedReconciling, attempt: t.prepared.attempt}, ErrStartupIntegrity
		}
		o.mu.RLock()
		old := o.current
		o.mu.RUnlock()
		t.outcome = storage.TransactionNotCommitted
		result.transaction = storage.TransactionNotCommitted
		if active.Revision() == old.Revision() && active.ManifestSHA256() == old.ManifestSHA256() {
			return o.rollbackKnown(t, NotCommitted, nil)
		}
		if active.Revision().Number() <= old.Revision().Number() {
			return result, ErrStartupIntegrity
		}
	} else {
		result.revision = attempt.Target()
		if attempt.Expected() != t.prepared.expected {
			return result, ErrStartupIntegrity
		}
		t.outcome, result.transaction = storage.TransactionCommitted, storage.TransactionCommitted
		if active.Revision().Number() < attempt.Target().Number() {
			return result, ErrStartupIntegrity
		}
	}
	result.outcome = CommittedReconciling
	admitted, err := readAdmitted(ctx, o.input.Store, o.replay, active.Revision(), active.ManifestSHA256(), o.input.Current.launch, o.input.Current.secrets)
	if err != nil {
		return result, err
	}
	result.revision = admitted.Revision()
	superseded := !present || admitted.Revision() != attempt.Target() || admitted.ManifestSHA256() != attempt.ManifestSHA256()
	if superseded && (!t.target.Valid() || t.target.Revision() != admitted.Revision() || t.target.ManifestSHA256() != admitted.ManifestSHA256()) {
		t, err = o.retarget(ctx, t, admitted)
		if err != nil {
			return result, err
		}
	} else {
		t.target, t.admitted = admitted, admitted
	}
	if !t.reservationsReady {
		if err := o.reserve(ctx, t); err != nil {
			return result, err
		}
	}
	records, err := t.registry.CommittedRecords(ctx, admitted.Revision(), admitted.ManifestSHA256())
	if err != nil {
		return result, err
	}
	if err := o.publish(ctx, t, records); err != nil {
		return result, err
	}
	result.outcome = Published
	return result, nil
}

func (o *Owner) publish(ctx context.Context, t *activationTransfer, records []integrations.RegistryRecord) error {
	if err := o.releaseSourceDeclarations(t); err != nil {
		return err
	}
	if t.step < publicationTransferred {
		o.mu.RLock()
		visibleDefinitions := o.current.legacy
		o.mu.RUnlock()
		if t.runs != nil {
			if err := t.runs.CancelChanged(ctx, changedAliases(t, records, visibleDefinitions)); err != nil {
				return err
			}
		}
		t.step = publicationTransferred
	}
	if t.step < publicationPilotsJoined {
		if t.operational.projection != nil {
			if err := t.operational.projection.Quiesce(ctx); err != nil {
				return err
			}
		}
		if t.pilots != nil {
			if err := t.pilots.Quiesce(ctx); err != nil {
				return err
			}
		}
		t.step = publicationPilotsJoined
	}
	if t.step < publicationForwardingPaused {
		if !t.forwardingPaused && !absentPort(o.runtime.Forwarding) {
			if err := o.runtime.Forwarding.PauseForActivation(ctx); err != nil {
				return err
			}
			t.forwardingPaused = true
		}
		t.step = publicationForwardingPaused
	}
	if t.step < publicationHost {
		if err := o.runtime.Host.PublishConfigurationRegistry(ctx, records); err != nil {
			return err
		}
		t.step = publicationHost
	}
	if t.step < publicationRegistry {
		if err := t.registry.PublishCommitted(ctx, t.admitted.Revision(), t.admitted.ManifestSHA256()); err != nil {
			return err
		}
		t.step = publicationRegistry
	}
	if t.step < publicationPolicies {
		if t.policies != nil {
			if err := t.policies.Publish(ctx); err != nil {
				return err
			}
		}
		t.step = publicationPolicies
	}
	if t.step < publicationOperational {
		if t.operational.projection != nil {
			if err := t.operational.projection.Publish(ctx); err != nil {
				return err
			}
		}
		t.step = publicationOperational
	}
	if t.step < publicationScenes {
		if t.scenes != nil {
			if err := t.scenes.Publish(); err != nil {
				return err
			}
		}
		t.step = publicationScenes
	}
	if t.step < publicationTemplates {
		if t.templates != nil {
			if err := t.templates.Publish(ctx); err != nil {
				return err
			}
		}
		t.step = publicationTemplates
	}
	if t.step < publicationTriggers {
		if t.triggersPrepared {
			if err := t.runs.PublishTriggers(); err != nil {
				return err
			}
		}
		t.step = publicationTriggers
	}
	if t.step < publicationPilots {
		if t.pilots != nil {
			if err := t.pilots.Publish(); err != nil {
				return err
			}
		}
		t.step = publicationPilots
	}
	if t.step < publicationScheduled {
		if t.triggersPrepared {
			if err := t.runs.ScheduleTriggerPublication(ctx); err != nil {
				return err
			}
		}
		t.step = publicationScheduled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	guard, err := o.input.Store.GuardConfigPublication(ctx, t.admitted.Revision(), t.admitted.ManifestSHA256())
	if err != nil {
		return err
	}
	defer guard.Release() // outside o.mu, including every failure path
	// Only bounded memory transitions run under this authority lock. In
	// particular SQL, 044 queue acknowledgement, drivers and joins are above.
	o.mu.Lock()
	if o.phase == ownerClosed || o.pending != t {
		o.mu.Unlock()
		return ErrActivationUnavailable
	}
	if err := ctx.Err(); err != nil {
		o.mu.Unlock()
		return err
	}
	if t.runs != nil && !t.runsPublished {
		if err := t.runs.Publish(); err != nil {
			o.mu.Unlock()
			return err
		}
		t.runsPublished = true
	}
	if t.cut != nil {
		if err := t.cut.Release(); err != nil {
			o.mu.Unlock()
			return err
		}
		t.cut = nil
	}
	if t.forwardingPaused {
		if err := o.runtime.Forwarding.ResumeAfterActivation(); err != nil {
			o.mu.Unlock()
			return err
		}
		t.forwardingPaused = false
	}
	if t.runs != nil {
		if err := t.runs.ResumeAdmissions(); err != nil {
			o.mu.Unlock()
			return err
		}
	}
	if t.pause != nil {
		if err := o.runtime.Commands.PublishNew(t.pause); err != nil {
			o.mu.Unlock()
			return err
		}
		t.pause = nil
	}
	o.current, o.phase, o.pending = t.admitted, ownerOpen, nil
	if o.runtime.Publication != nil {
		o.runtime.Publication.PublishConfigRevision(o.current.Revision())
	}
	o.notifyLocked()
	o.mu.Unlock()
	if err := t.registry.Release(); err != nil {
		return err
	}
	t.registry = nil
	if t.policies != nil {
		t.policies.Release()
		t.policies = nil
	}
	if t.operational.projection != nil {
		t.operational.projection.Release()
	}
	return nil
}

func changedAliases(t *activationTransfer, next []integrations.RegistryRecord, visible bundle.Candidate) []domain.HAAlias {
	changed := make(map[domain.HAAlias]bool)
	type resolution struct {
		device   domain.DeviceID
		entity   domain.EntityID
		instance domain.IntegrationInstanceID
		key      integrations.SourceKey
		retired  bool
	}
	oldAliases, newAliases := make(map[domain.HAAlias]resolution), make(map[domain.HAAlias]resolution)
	add := func(destination map[domain.HAAlias]resolution, record integrations.RegistryRecord) {
		device, entity, _ := record.IDs()
		instance, key, _ := record.Source()
		_, _, aliases, _ := record.Aliases()
		for _, alias := range aliases {
			destination[alias] = resolution{device, entity, instance, key, record.Retired()}
		}
	}
	instances := make(map[domain.IntegrationInstanceID]bool)
	for _, spec := range t.operational.next {
		id, _ := spec.Instance()
		instances[id] = true
	}
	for _, id := range t.operational.remove {
		instances[id] = true
	}
	for _, record := range t.old.Registry {
		add(oldAliases, record)
	}
	for _, record := range next {
		add(newAliases, record)
	}
	for alias, old := range oldAliases {
		current, present := newAliases[alias]
		if !present || old != current || instances[old.instance] || instances[current.instance] {
			changed[alias] = true
		}
	}
	for alias, current := range newAliases {
		if _, present := oldAliases[alias]; !present || instances[current.instance] {
			changed[alias] = true
		}
	}
	if t.scenes != nil {
		for _, alias := range t.scenes.ChangedAliases() {
			changed[alias] = true
		}
	}
	definitions := t.prepared.legacy
	if t.target.Valid() {
		definitions = t.target.legacy
	}
	oldGroups := make(map[domain.HAAlias]bundle.Group)
	for _, group := range visible.Groups() {
		oldGroups[group.ID()] = group
	}
	for _, group := range definitions.Groups() {
		before, present := oldGroups[group.ID()]
		if !present || before.All() != group.All() || !reflect.DeepEqual(before.Members(), group.Members()) {
			changed[group.ID()] = true
		}
		delete(oldGroups, group.ID())
	}
	for id := range oldGroups {
		changed[id] = true
	}
	out := make([]domain.HAAlias, 0, len(changed))
	for alias := range changed {
		out = append(out, alias)
	}
	return out
}

func (o *Owner) rollbackKnown(t *activationTransfer, outcome Outcome, cause error) (Result, error) {
	if err := o.releaseSourceDeclarations(t); err != nil {
		return Result{outcome: NotCommitted, attempt: t.prepared.attempt, transaction: storage.TransactionNotCommitted}, errors.Join(cause, err)
	}
	if t.registry != nil {
		_ = t.registry.ResolveNotCommitted(storage.TransactionNotCommitted)
		if err := t.registry.Release(); err != nil {
			return Result{outcome: CommitUnknown, attempt: t.prepared.attempt}, errors.Join(cause, err)
		}
		t.registry = nil
	}
	if t.cut != nil {
		if err := t.cut.Release(); err != nil {
			return Result{outcome: CommitUnknown, attempt: t.prepared.attempt}, errors.Join(cause, err)
		}
		t.cut = nil
	}
	o.mu.Lock()
	if o.phase != ownerClosed {
		if t.pause != nil {
			if err := o.runtime.Commands.ResumeOld(t.pause); err != nil {
				o.mu.Unlock()
				return Result{outcome: NotCommitted, attempt: t.prepared.attempt}, errors.Join(cause, err)
			}
			t.pause = nil
		}
		o.phase, o.pending = ownerOpen, nil
	}
	o.mu.Unlock()
	o.releaseUnwritten(t)
	return Result{outcome: outcome, attempt: t.prepared.attempt, transaction: storage.TransactionNotCommitted}, cause
}

func (o *Owner) releaseUnwritten(t *activationTransfer) {
	declarationErr := o.releaseSourceDeclarations(t)
	if t.operational.projection != nil {
		t.operational.projection.Release()
	}
	if t.registry != nil {
		_ = t.registry.Release()
	}
	if t.pause != nil && declarationErr == nil {
		_ = o.runtime.Commands.ResumeOld(t.pause)
	}
	if t.pilots != nil {
		_ = t.pilots.Release()
	}
	if t.scenes != nil {
		_ = t.scenes.Release()
	}
	if t.templates != nil {
		_ = t.templates.Release()
	}
	if t.runs != nil {
		_ = t.runs.Release()
	}
	if t.policies != nil {
		t.policies.Release()
	}
}

func (o *Owner) BeginClose() <-chan struct{} {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	if o.phase != ownerClosed {
		o.phase = ownerClosed
		o.notifyLocked()
		if o.runtime.Runs != nil {
			o.runtime.Runs.CloseAdmissions()
		}
		o.cancel()
	}
	o.mu.Unlock()
	return o.done
}
func (o *Owner) Done() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.done
}
func (o *Owner) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if absentPort(ctx) {
		return ErrInvalidStartup
	}
	select {
	case <-o.BeginClose():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (o *Owner) disposeClosed(t *activationTransfer) {
	// Terminal shutdown never interprets unresolved SQL as permission to
	// resume A. Retained human reservations are quarantined before release.
	if t.policies != nil {
		t.policies.Quarantine()
		t.policies.Release()
	}
	if t.operational.projection != nil {
		t.operational.projection.Release()
	}
	if t.pilots != nil {
		_ = t.pilots.Release()
	}
	if t.scenes != nil {
		_ = t.scenes.Release()
	}
	if t.templates != nil {
		_ = t.templates.Release()
	}
	if t.runs != nil {
		_ = t.runs.Release()
	}
}

var _ integrations.GenerationAuthority = (*Owner)(nil)
var _ view.Configuration = (*Owner)(nil)
var _ view.ConfigChangeNotifier = (*Owner)(nil)
