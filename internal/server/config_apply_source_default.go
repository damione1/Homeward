package server

import (
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/scope"
	"time"
)

// Compose the existing real interpreter services while the initial program is
// dormant. Execution remains empty until exact admitted sources are replayed
// or published through the one configuration owner.
func prepareDefaultSourceExecution(plan RuntimePlan, commands *DurableCommandsComposition) (RuntimePlan, error) {
	if plan.AutomationRuns != nil {
		return plan, nil
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		return RuntimePlan{}, err
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		return RuntimePlan{}, err
	}
	triggerPlan, err := automationtriggers.NewEmptyActivationPlan("UTC")
	if err != nil {
		return RuntimePlan{}, err
	}
	plan.AutomationRuns, err = NewScriptRunsComposition(ScriptRunsInput{Plan: automationruns.NewEmptyActivationPlan(), Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC, CommandDeadline: 30 * time.Second, Scenes: &catalogue})
	if err != nil {
		return RuntimePlan{}, err
	}
	plan.AutomationTriggers = &triggerPlan
	return plan, nil
}
