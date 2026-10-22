package configactivation

import (
	"context"
	"github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/compat/ha/ir"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/scope"
)

// SourceRuntimeCompiler is immutable trusted runtime composition. Its semantic
// catalogue and trigger environment must come from the existing admitted
// registry/Host composition. It never infers execution support from YAML.
type SourceRuntimeCompiler struct {
	compilationContext configcompilation.Context
	manifest           registry.Manifest
	catalogue          ir.Catalogue
	triggers           automationtriggers.PlanInput
	automations        []ir.Automation
	scripts            []ir.Script
	scenes             scenes.Catalogue
}

func NewSourceRuntimeCompiler(manifest registry.Manifest, catalogue ir.Catalogue, environment automationtriggers.PlanInput) (*SourceRuntimeCompiler, error) {
	if _, valid := manifest.Revision(); !valid || !catalogue.Valid() || absentPort(environment.Resolver) {
		return nil, ErrInvalidStartup
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		return nil, err
	}
	seed, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		return nil, err
	}
	autos, scripts := append([]ir.Automation(nil), environment.Automations...), append([]ir.Script(nil), environment.Scripts...)
	environment.Automations, environment.Scripts = nil, nil
	// Validate the real dormant environment without manufacturing a program.
	if _, err := automationtriggers.NewEmptyActivationPlan(environment.Zone); err != nil {
		return nil, err
	}
	return &SourceRuntimeCompiler{manifest: manifest, catalogue: catalogue, triggers: environment, automations: autos, scripts: scripts, scenes: seed}, nil
}

type SourceRuntimeImage struct {
	compilation     SourceCompilation
	program         *ProgramImage
	sceneCatalogue  *scenes.Catalogue
	dormantRuns     automationruns.Plan
	dormantTriggers automationtriggers.Plan
}

func (i SourceRuntimeImage) Compilation() SourceCompilation { return i.compilation }
func (i SourceRuntimeImage) Program() (ProgramImage, bool) {
	if i.program == nil {
		return ProgramImage{}, false
	}
	return *i.program, true
}
func (i SourceRuntimeImage) Runs() (automationruns.Plan, bool) {
	p, ok := i.Program()
	if !ok && i.sceneCatalogue != nil {
		return i.dormantRuns, i.dormantRuns.Valid()
	}
	return p.runs, ok
}
func (i SourceRuntimeImage) Triggers() (automationtriggers.Plan, bool) {
	p, ok := i.Program()
	if !ok && i.sceneCatalogue != nil {
		return i.dormantTriggers, i.dormantTriggers.Valid()
	}
	return p.triggers, ok
}
func (i SourceRuntimeImage) Scenes() (scenes.Catalogue, bool) {
	if i.sceneCatalogue != nil {
		return *i.sceneCatalogue, true
	}
	p, ok := i.Program()
	if !ok || p.scenes == nil {
		return scenes.Catalogue{}, false
	}
	return *p.scenes, true
}

// Compose always recompiles exact source snapshots. The image is transient;
// only source bytes, descriptors and their hashes enter the existing SQL graph.
func (c *SourceRuntimeCompiler) Compose(ctx context.Context, groups []SourceGroup, templates reactivetemplate.Plan) (SourceRuntimeImage, error) {
	if c == nil || !usableContext(ctx) {
		return SourceRuntimeImage{}, ErrInvalidStartup
	}
	compiled, err := CompileSourceGroups(groups, c.manifest, c.catalogue)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	compiled.compilationContext = c.compilationContext
	result := SourceRuntimeImage{compilation: compiled}
	needsProgram := len(compiled.automations)+len(compiled.scripts)+len(compiled.scenes) != 0
	if !needsProgram {
		return result, nil
	}
	if len(compiled.automations)+len(compiled.scripts)+len(c.automations)+len(c.scripts) != 0 && !templates.Active() {
		return SourceRuntimeImage{}, ErrInvalidStartup
	}
	// Existing immutable baseline seeds remain intact; exact editable overrides
	// replace identities in this one candidate image only.
	auto := append([]ir.Automation(nil), c.automations...)
	for _, replacement := range compiled.automations {
		found := false
		for n, prior := range auto {
			if prior.Source().Object() == replacement.Source().Object() {
				auto[n], found = replacement, true
				break
			}
		}
		if !found {
			auto = append(auto, replacement)
		}
	}
	scripts := append([]ir.Script(nil), c.scripts...)
	for _, replacement := range compiled.scripts {
		found := false
		for n, prior := range scripts {
			if prior.ID() == replacement.ID() {
				scripts[n], found = replacement, true
				break
			}
		}
		if !found {
			scripts = append(scripts, replacement)
		}
	}
	definitions := make([]hascene.Definition, 0, 12)
	replacements := map[domain.HAAlias]hascene.Definition{}
	for _, definition := range compiled.scenes {
		replacements[definition.ID()] = definition
	}
	for _, id := range c.scenes.IDs() {
		definition, present := replacements[id]
		if present {
			delete(replacements, id)
		} else {
			definition, present = c.scenes.Definition(id)
			if !present {
				return SourceRuntimeImage{}, ErrInvalidStartup
			}
		}
		definitions = append(definitions, definition)
	}
	if len(replacements) != 0 {
		return SourceRuntimeImage{}, ErrInvalidStartup
	}
	sceneImage, err := scenes.NewCatalogue(definitions)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	if len(compiled.scenes) != 0 {
		result.sceneCatalogue = &sceneImage
	}
	if len(auto)+len(scripts) == 0 {
		result.dormantRuns = automationruns.NewEmptyActivationPlan()
		result.dormantTriggers, err = automationtriggers.NewEmptyActivationPlan(c.triggers.Zone)
		if err != nil {
			return SourceRuntimeImage{}, err
		}
		return result, ctx.Err()
	}
	runImage, err := automationruns.NewPlanWithScripts(auto, scripts)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	environment := c.triggers
	environment.Automations, environment.Scripts = auto, scripts
	triggerImage, err := automationtriggers.NewPlan(environment)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	image, err := NewProgramImage(runImage, triggerImage, templates, result.sceneCatalogue)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	result.program = &image
	return result, ctx.Err()
}

// SourceOperationalCompiler extends the same fixed operational participant;
// ownership never publishes through an independent writer or active pointer.
type SourceOperationalCompiler interface {
	PrepareOperationalSources(context.Context, config.Config, config.Config, bundle.Candidate, bundle.Candidate, reactivetemplate.Plan, automationtriggers.Plan, []domain.IntegrationInstanceID, *registry.CommittedBindings, SourceCompilation, *registry.ActivationGate) (OperationalImage, error)
}

func (o *Owner) composeSourceProgram(ctx context.Context, t *activationTransfer, templates reactivetemplate.Plan) (SourceRuntimeImage, error) {
	groups := t.prepared.extraGroups
	if t.target.Valid() {
		groups = t.target.extraGroups
	}
	if len(groups) == 0 {
		return SourceRuntimeImage{}, nil
	}
	var input configcompilation.Context
	if t.target.Valid() {
		input = t.target.compilationContext
	} else if t.request.sourceContext != nil {
		input = *t.request.sourceContext
	}
	if !input.Valid() {
		return SourceRuntimeImage{}, ErrProgramCompositionRequired
	}
	operational := t.basis.operational
	if t.target.Valid() {
		operational = t.target.operational
	} else if t.prepared.source.Valid() {
		var err error
		operational, err = t.prepared.source.OperationalConfig()
		if err != nil {
			return SourceRuntimeImage{}, err
		}
	}
	compiler, err := o.sourceCompiler(ctx, input, operational)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	image, err := compiler.Compose(ctx, groups, templates)
	if err != nil {
		return SourceRuntimeImage{}, err
	}
	if _, hasOwnership := image.compilation.Ownership(); hasOwnership {
		if _, present := o.runtime.Operational.(SourceOperationalCompiler); !present {
			return SourceRuntimeImage{}, ErrOperationalComposition
		}
	}
	return image, nil
}

func (o *Owner) confirmEditableSourceComposition(ctx context.Context, request Request, current AdmittedGeneration) error {
	groups, err := replaceSourceGroup(current.extraGroups, request.sourceReplacement)
	if err != nil || len(groups) == 0 {
		return err
	}
	var templates reactivetemplate.Plan
	if o.runtime.Templates != nil {
		templates, err = o.runtime.Templates.PermanentPlan()
		if err != nil {
			return err
		}
	}
	t := activationTransfer{prepared: preparedActivation{extraGroups: groups}, request: request, basis: operationalBasis{operational: current.operational}}
	image, err := o.composeSourceProgram(ctx, &t, templates)
	if err != nil {
		return err
	}
	if _, hasScenes := image.Scenes(); hasScenes && o.runtime.Scenes == nil {
		return ErrProgramCompositionRequired
	}
	if image.program != nil && (o.runtime.Runs == nil || o.runtime.Templates == nil) {
		return ErrProgramCompositionRequired
	}
	if request.sourceCompilation != nil && request.sourceCompilation.hash != image.compilation.hash {
		return ErrInvalidStartup
	}
	return nil
}

func (g AdmittedGeneration) ComposeEditableSources(ctx context.Context, compiler *SourceRuntimeCompiler, templates reactivetemplate.Plan) (SourceRuntimeImage, error) {
	if !g.Valid() {
		return SourceRuntimeImage{}, ErrInvalidStartup
	}
	if len(g.extraGroups) == 0 {
		return SourceRuntimeImage{}, nil
	}
	return compiler.Compose(ctx, g.extraGroups, templates)
}
func (g AdmittedGeneration) HasEditableSources() bool { return g.Valid() && len(g.extraGroups) != 0 }

// Current source consumers read the same published generation. These methods
// expose no second mutable catalogue or publication authority.
func (o *Owner) currentSourceCompilation(ctx context.Context) (SourceCompilation, error) {
	if o == nil || !usableContext(ctx) {
		return SourceCompilation{}, ErrInvalidStartup
	}
	o.mu.RLock()
	current := o.current
	o.mu.RUnlock()
	if !current.Valid() || !current.compilationContext.Valid() {
		return SourceCompilation{}, ErrProgramCompositionRequired
	}
	compiler, err := o.sourceCompiler(ctx, current.compilationContext, current.operational)
	if err != nil {
		return SourceCompilation{}, err
	}
	return CompileSourceGroups(current.extraGroups, compiler.manifest, compiler.catalogue)
}
func (o *Owner) CurrentDashboard(ctx context.Context, id authorization.DashboardID) (dashboard.Definition, bool, error) {
	compiled, err := o.currentSourceCompilation(ctx)
	if err != nil {
		return dashboard.Definition{}, false, err
	}
	definition, present := compiled.Dashboard(id)
	return definition, present, nil
}
func (o *Owner) CurrentOwnership(ctx context.Context) (heatingownership.Decision, bool, error) {
	compiled, err := o.currentSourceCompilation(ctx)
	if err != nil {
		return heatingownership.Decision{}, false, err
	}
	decision, present := compiled.Ownership()
	return decision, present, nil
}
