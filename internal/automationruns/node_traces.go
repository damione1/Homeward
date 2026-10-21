package automationruns

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
)

type NodeStage uint8

const (
	NodeStageInvalid NodeStage = iota
	NodeEntered
	NodeCompleted
	NodeStopped
	NodeCancelled
	NodeFailed
	NodeWaiting
)

type executionPath struct {
	branches, iterations        [MaxBranchDepth]int
	branchDepth, iterationDepth int
}

func (p *executionPath) pushBranch(index int) bool {
	if p.branchDepth >= len(p.branches) || index < 0 {
		return false
	}
	p.branches[p.branchDepth] = index
	p.branchDepth++
	return true
}
func (p *executionPath) pushIteration(index int) bool {
	if p.iterationDepth >= len(p.iterations) || index < 1 {
		return false
	}
	p.iterations[p.iterationDepth] = index
	p.iterationDepth++
	return true
}

// Trace values retain bounded metadata, not command payloads or environments.
// Sources/trigger IDs reference already owned immutable admitted program data.
// The ring's actual struct size is included in its one-MiB reservation.
type NodeTrace struct {
	ordinal    TraceOrdinal
	run        RunID
	visit      uint64
	parent     RunID
	generation Generation
	provenance DefinitionProvenance
	source     ir.Source
	kind       ir.ActionKind
	stage      NodeStage
	at         time.Time
	failure    ExecutionErrorKind
	path       executionPath
	wait       waitObservation
	hasWait    bool
}

const nodeTraceCapacity = min(MaxNodeTraces, nodeTraceCharge/(int(unsafe.Sizeof(NodeTrace{}))+512))

func (t NodeTrace) RunID() RunID                     { return t.run }
func (t NodeTrace) Visit() uint64                    { return t.visit }
func (t NodeTrace) ParentID() RunID                  { return t.parent }
func (t NodeTrace) Provenance() DefinitionProvenance { return t.provenance }
func (t NodeTrace) Generation() Generation           { return t.generation }
func (t NodeTrace) Source() ir.Source                { return t.source }
func (t NodeTrace) Kind() ir.ActionKind              { return t.kind }
func (t NodeTrace) Stage() NodeStage                 { return t.stage }
func (t NodeTrace) At() time.Time                    { return t.at }
func (t NodeTrace) Failure() ExecutionErrorKind      { return t.failure }
func (t NodeTrace) Branches() []int {
	return append([]int(nil), t.path.branches[:t.path.branchDepth]...)
}
func (t NodeTrace) Iterations() []int {
	return append([]int(nil), t.path.iterations[:t.path.iterationDepth]...)
}
func (t NodeTrace) WaitExit() WaitExit { return t.wait.exit }
func (t NodeTrace) WaitValue() (template.Value, bool, error) {
	if !t.hasWait {
		return template.Value{}, false, nil
	}
	value, err := t.wait.value()
	return value, true, err
}
func (i *Interpreter) nodeTrace(run Run, node executableNode, stage NodeStage, err error) NodeTrace {
	trace := NodeTrace{run: run.id, visit: run.visit, parent: run.parent, generation: run.generation, provenance: run.provenance, source: node.source,
		kind: node.kind, stage: stage, at: i.input.Clock.Now().UTC(), path: run.path}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		trace.stage = NodeCancelled
	}
	var failure *ExecutionError
	if errors.As(err, &failure) {
		trace.failure = failure.Kind()
	}
	return trace
}
func (i *Interpreter) appendNodeTrace(trace NodeTrace) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.controller == nil || i.image == nil || cap(i.traces) != nodeTraceCapacity {
		return
	}
	// The existing bind supplies the single Controller-owned ordering space.
	// No Controller mutex is acquired while holding the interpreter mutex.
	trace.ordinal = i.controller.nextTraceOrdinal()
	if len(i.traces) == nodeTraceCapacity {
		i.traces[i.traceNext] = trace
		i.traceNext = (i.traceNext + 1) % nodeTraceCapacity
		i.evicted++
	} else {
		i.traces = append(i.traces, trace)
	}
	if i.controller.sink != nil {
		i.controller.sink.ObserveNode(trace.historyObservation())
	}
}
func (i *Interpreter) traceNode(run Run, node executableNode, stage NodeStage, err error) {
	i.appendNodeTrace(i.nodeTrace(run, node, stage, err))
}
func (i *Interpreter) traceWait(run Run, node executableNode, wait waitObservation, err error) {
	stage := NodeCompleted
	if wait.exit == WaitStoppedByTimeout {
		stage = NodeStopped
	}
	if err != nil {
		stage = NodeFailed
	}
	trace := i.nodeTrace(run, node, stage, err)
	trace.wait, trace.hasWait = wait, true
	i.appendNodeTrace(trace)
}
func (i *Interpreter) NodeTraces() ([]NodeTrace, uint64) {
	if i == nil {
		return nil, 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.traces) == 0 {
		return nil, i.evicted
	}
	result := make([]NodeTrace, 0, len(i.traces))
	result = append(result, i.traces[i.traceNext:]...)
	result = append(result, i.traces[:i.traceNext]...)
	return result, i.evicted
}
