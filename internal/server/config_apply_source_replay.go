package server

import (
	"context"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

// prepareAdmittedEditableSources runs before listeners, pilots or readiness.
// Factories must supply the real existing execution composition. Source bytes
// alone cannot create an executor, command authority or capability catalogue.
func prepareAdmittedEditableSources(ctx context.Context, admitted configactivation.AdmittedGeneration, plan RuntimePlan) (RuntimePlan, error) {
	if !admitted.HasEditableSources() {
		return plan, nil
	}
	if plan.SourceFactory == nil {
		return RuntimePlan{}, configactivation.ErrProgramCompositionRequired
	}
	input, present := admitted.CompilationContext()
	if !present {
		return RuntimePlan{}, configactivation.ErrStartupIntegrity
	}
	operational, err := admitted.OperationalConfig()
	if err != nil {
		return RuntimePlan{}, err
	}
	plan.SourceCompiler, err = plan.SourceFactory(ctx, input, operational)
	if err != nil || plan.SourceCompiler == nil {
		return RuntimePlan{}, configactivation.ErrProgramCompositionRequired
	}
	var templates reactivetemplate.Plan
	if plan.ReactiveTemplates != nil {
		templates = plan.ReactiveTemplates.plan
	}
	image, err := admitted.ComposeEditableSources(ctx, plan.SourceCompiler, templates)
	if err != nil {
		return RuntimePlan{}, err
	}
	if decision, hasOwnership := image.Compilation().Ownership(); hasOwnership {
		// Initial cloud factories already consumed this exact SQL-bound decision.
		// A custom assembly without that typed composition remains closed.
		if plan.sourceOwnership == nil || !sameSourceOwnership(*plan.sourceOwnership, decision) {
			return RuntimePlan{}, configactivation.ErrOperationalComposition
		}
	}
	_, hasProgram := image.Program()
	if !hasProgram {
		if sceneImage, hasScenes := image.Scenes(); hasScenes {
			if plan.AutomationRuns == nil || plan.AutomationRuns.scripts == nil {
				return RuntimePlan{}, configactivation.ErrProgramCompositionRequired
			}
			input := plan.AutomationRuns.scripts.input
			input.Scenes = &sceneImage
			plan.AutomationRuns, err = NewScriptRunsComposition(input)
			if err != nil {
				return RuntimePlan{}, err
			}
		}
		return plan, nil
	}
	runs, _ := image.Runs()
	if plan.AutomationRuns == nil {
		return RuntimePlan{}, configactivation.ErrProgramCompositionRequired
	}
	triggers, present := image.Triggers()
	if !present {
		return RuntimePlan{}, configactivation.ErrProgramCompositionRequired
	}
	if scripts := plan.AutomationRuns.scripts; scripts != nil {
		input := scripts.input
		input.Plan = runs
		if scenes, present := image.Scenes(); present {
			input.Scenes = &scenes
		}
		plan.AutomationRuns, err = NewScriptRunsComposition(input)
	} else {
		plan.AutomationRuns, err = NewAutomationRunsComposition(AutomationRunsInput{Plan: runs, Preparer: plan.AutomationRuns.preparer, Executor: plan.AutomationRuns.executor})
	}
	if err != nil {
		return RuntimePlan{}, err
	}
	plan.AutomationTriggers = &triggers
	return plan, nil
}
