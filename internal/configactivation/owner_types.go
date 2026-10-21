package configactivation

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
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
)

var ErrActivationBusy = errors.New("config activation: activation already unresolved")
var ErrActivationUnavailable = errors.New("config activation: publication unavailable")
var ErrOperationalComposition = errors.New("config activation: operational replacement has no admitted composition")
var ErrProgramCompositionRequired = errors.New("config activation: current SQL winner requires its trusted IR composition")

type Outcome uint8

const (
	OutcomeInvalid Outcome = iota
	RejectedBeforeCommit
	NotCommitted
	CommitUnknown
	CommittedReconciling
	Published
	RestartRequired
)

// Result separates a durable COMMIT from successful coherent publication.
// Revision identifies the SQL authority being reconciled/published. AttemptOutcome
// independently reports this caller's transaction: a later winner can be
// published even when this caller's attempt definitely did not commit.
type Result struct {
	outcome     Outcome
	revision    storage.ConfigGenerationRevision
	attempt     storage.ActivationAttemptID
	transaction storage.TransactionOutcome
}

func (r Result) Outcome() Outcome                           { return r.outcome }
func (r Result) Revision() storage.ConfigGenerationRevision { return r.revision }
func (r Result) Attempt() storage.ActivationAttemptID       { return r.attempt }
func (r Result) AttemptOutcome() storage.TransactionOutcome { return r.transaction }

// ProgramImage is an immutable IR composition seam for 042/115. It does NOT
// claim that Logic1 sources are in the durable manifest. NHA-115 must bind its
// source admission/manifest/compiler/replay before that claim is possible.
type ProgramImage struct {
	runs      automationruns.Plan
	triggers  automationtriggers.Plan
	templates reactivetemplate.Plan
	scenes    *scenes.Catalogue
	admitted  bool
}

func NewProgramImage(runs automationruns.Plan, triggers automationtriggers.Plan, templates reactivetemplate.Plan, catalogue *scenes.Catalogue) (ProgramImage, error) {
	if !runs.Valid() || !triggers.Valid() || !templates.Active() ||
		!reflect.DeepEqual(runs.AutomationIDs(), triggers.AutomationIDs()) {
		return ProgramImage{}, ErrInvalidStartup
	}
	var owned *scenes.Catalogue
	if catalogue != nil {
		if len(catalogue.IDs()) != 12 {
			return ProgramImage{}, ErrInvalidStartup
		}
		value := *catalogue
		owned = &value
	}
	return ProgramImage{runs: runs, triggers: triggers, templates: templates, scenes: owned, admitted: true}, nil
}

type Request struct {
	definitions           DefinitionChoice
	author                storage.ConfigAuthor
	source                storage.ConfigSource
	policies              []PolicyChange
	program               *ProgramImage
	registry              func(*registry.ConfigWriter) error
	admitted              bool
	workspacePrecondition *WorkspacePrecondition
}

// NewWorkspaceRequest is trusted Go composition, not an HTTP authorization
// bypass. Human configuration routes must admit their existing permissions
// before calling this port. Internal run execution never carries an author.
func NewWorkspaceRequest(definitions DefinitionChoice, author storage.ConfigAuthor, source storage.ConfigSource,
	policies []PolicyChange, program *ProgramImage, mutateRegistry func(*registry.ConfigWriter) error) (Request, error) {
	if definitions.kind != definitionChoiceRetain && definitions.kind != definitionChoiceReplace {
		return Request{}, ErrInvalidStartup
	}
	if _, err := storage.NewConfigVersionMetadata(author, source); err != nil {
		return Request{}, ErrInvalidStartup
	}
	if len(policies) > 16384 {
		return Request{}, ErrInvalidStartup
	}
	var owned *ProgramImage
	if program != nil {
		if !program.admitted {
			return Request{}, ErrInvalidStartup
		}
		value := *program
		owned = &value
	}
	return Request{definitions: definitions, author: author, source: source,
		policies: append([]PolicyChange(nil), policies...), program: owned, registry: mutateRegistry, admitted: true}, nil
}

func (p PolicyChange) Account() contracts.AccountID       { return p.account }
func (p PolicyChange) Source() authorization.PolicySource { return p.source }

// PolicyAdmissions is the existing human account/MFA boundary owned by Run.
// This is the only participant whose identity is human, for explicit edits.
type PolicyAdmissions interface {
	ReserveActivationPolicies(context.Context, []PolicyChange) (PolicyReservation, error)
	ReserveReconciliationPolicies(context.Context, []contracts.AccountID) (PolicyReservation, error)
}
type PolicyReservation interface {
	Publish(context.Context) error
	Quarantine()
	Release()
}

// TemplateForwarding controls the one existing server output pump. It joins
// callbacks already dequeued before publication; it never starts an engine.
type TemplateForwarding interface {
	PauseForActivation(context.Context) error
	ResumeAfterActivation() error
}

// OperationalCompiler receives the exact parsed operational source, rather
// than a mutable IR factory presented as a persisted program. It constructs
// pilot specs only, never connects or starts them during preparation.
type OperationalCompiler interface {
	PrepareOperational(context.Context, config.Config, config.Config, bundle.Candidate, bundle.Candidate, reactivetemplate.Plan, automationtriggers.Plan, []domain.IntegrationInstanceID, *registry.CommittedBindings) (OperationalImage, error)
}

// OperationalProjection is Run's one operational composition, encompassing
// its existing cloud registrations and calculated publisher. It is not a
// participant registry: the two phases are fixed by the activation protocol.
type OperationalProjection interface {
	Quiesce(context.Context) error
	Publish(context.Context) error
	Supersede(context.Context) ([]domain.IntegrationInstanceID, error)
	Release()
}
type OperationalImage struct {
	next       []integrations.PilotSpec
	remove     []domain.IntegrationInstanceID
	registry   func(*registry.ConfigWriter) error
	projection OperationalProjection
	templates  *reactivetemplate.Plan
	triggers   *automationtriggers.Plan
	admitted   bool
}

func NewOperationalImage(next []integrations.PilotSpec, remove []domain.IntegrationInstanceID, bindings *registry.PreparedBindings, projection OperationalProjection, templates *reactivetemplate.Plan, triggers *automationtriggers.Plan) (OperationalImage, error) {
	if len(next)+len(remove) > 1024 {
		return OperationalImage{}, ErrInvalidStartup
	}
	seen := make(map[domain.IntegrationInstanceID]bool, len(next)+len(remove))
	for _, spec := range next {
		id, err := spec.Instance()
		if err != nil || seen[id] {
			return OperationalImage{}, ErrInvalidStartup
		}
		seen[id] = true
	}
	for _, id := range remove {
		if _, err := id.Value(); err != nil || seen[id] {
			return OperationalImage{}, ErrInvalidStartup
		}
		seen[id] = true
	}
	if projection != nil && absentPort(projection) {
		return OperationalImage{}, ErrInvalidStartup
	}
	var owned *reactivetemplate.Plan
	if templates != nil {
		if !templates.Active() {
			return OperationalImage{}, ErrInvalidStartup
		}
		value := *templates
		owned = &value
	}
	var triggerImage *automationtriggers.Plan
	if triggers != nil {
		if !triggers.Valid() {
			return OperationalImage{}, ErrInvalidStartup
		}
		value := *triggers
		triggerImage = &value
	}
	var mutateRegistry func(*registry.ConfigWriter) error
	if bindings != nil {
		mutateRegistry = func(writer *registry.ConfigWriter) error { return writer.ApplyPreparedBindings(bindings) }
	}
	return OperationalImage{next: append([]integrations.PilotSpec(nil), next...), remove: append([]domain.IntegrationInstanceID(nil), remove...), registry: mutateRegistry, projection: projection, templates: owned, triggers: triggerImage, admitted: true}, nil
}

type OwnerInput struct {
	Store      *storage.Store
	Startup    config.StartupInputs
	Current    AdmittedGeneration
	Registry   *registry.Repository
	Projection *sequencer.Sequencer
	Timeout    time.Duration
}
type Runtime struct {
	Host        *integrations.Manager
	Commands    *integrations.DurableCommandService
	Runs        *automationruns.Controller
	Templates   *reactivetemplate.Service
	Forwarding  TemplateForwarding
	Scenes      *scenes.Service
	Supervisor  *integrations.Supervisor
	Policies    PolicyAdmissions
	Operational OperationalCompiler
	Publication ConfigPublicationObserver
}

// ConfigPublicationObserver records an already loaded, coherent generation.
// The composed observer must be bounded and perform no I/O or admission work.
// It cannot decide or retry SQL, release gates, or make readiness true.
type ConfigPublicationObserver interface {
	PublishConfigRevision(storage.ConfigGenerationRevision)
}

func absentPort(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}
