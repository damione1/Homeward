package automationruns

import (
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
)

// The observations below contain only whitelisted metadata. In particular
// they cannot carry Trigger.Payload, Environment, template values, Source
// references/inputs, error text, causes, or command arguments.
type HistoryRunRef struct {
	Generation      Generation
	RunID, ParentID RunID
	Provenance      DefinitionProvenance
}

type HistorySource struct {
	Object, File, Path string
	Line, Column       int
}

func sourceForHistory(source ir.Source) HistorySource {
	at := source.At()
	return HistorySource{Object: source.Object(), File: at.File, Path: source.Path(), Line: at.Line, Column: at.Column}
}

type DecisionObservation struct {
	At                             time.Time
	NativeOrdinal                  TraceOrdinal
	Run                            HistoryRunRef
	Decision                       Decision
	Automation, Script, TriggerKey string
}

type NodeObservation struct {
	At                          time.Time
	NativeOrdinal               TraceOrdinal
	Run                         HistoryRunRef
	Visit                       uint64
	Source                      HistorySource
	Action                      ir.ActionKind
	Stage                       NodeStage
	Failure                     ExecutionErrorKind
	Wait                        WaitExit
	Branches, Iterations        [MaxBranchDepth]int
	BranchDepth, IterationDepth int
}

type ScriptLinkObservation struct {
	At            time.Time
	Parent, Child HistoryRunRef
	Script        ir.ScriptID
	Mode          ir.ScriptCallMode
}

// HistoryObservationSink must only perform bounded, nonblocking admission.
// Its implementation has no access to program state or a live Environment.
type HistoryObservationSink interface {
	ObserveDecision(DecisionObservation)
	ObserveNode(NodeObservation)
	ObserveScriptLink(ScriptLinkObservation)
}

func (c *Controller) AttachHistoryObservationSink(sink HistoryObservationSink) error {
	if c == nil || !c.plan.Valid() || nilPort(sink) {
		return ErrInvalidPlan
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.closed || c.sink != nil {
		return ErrInvalidPlan
	}
	c.sink = sink
	return nil
}

func (r Run) historyRef() HistoryRunRef {
	return HistoryRunRef{Generation: r.generation, RunID: r.id, ParentID: r.parent, Provenance: r.provenance}
}

func (t Trace) historyObservation() DecisionObservation {
	script, _ := t.Script.Value()
	return DecisionObservation{At: t.At, NativeOrdinal: t.ordinal,
		Run:      HistoryRunRef{Generation: t.Generation, RunID: t.RunID, ParentID: t.ParentID, Provenance: t.Provenance},
		Decision: t.Decision, Automation: t.Automation, Script: script, TriggerKey: t.TriggerKey}
}

func (t NodeTrace) historyObservation() NodeObservation {
	return NodeObservation{At: t.at, NativeOrdinal: t.ordinal,
		Run:   HistoryRunRef{Generation: t.generation, RunID: t.run, ParentID: t.parent, Provenance: t.provenance},
		Visit: t.visit, Source: sourceForHistory(t.source), Action: t.kind, Stage: t.stage, Failure: t.failure,
		Wait: t.wait.exit, Branches: t.path.branches, Iterations: t.path.iterations,
		BranchDepth: t.path.branchDepth, IterationDepth: t.path.iterationDepth}
}
