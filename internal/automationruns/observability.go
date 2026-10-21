package automationruns

import (
	"github.com/damione1/Homeward/compat/ha/ir"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (c *Controller) AttachObservation(cells *observability.Cells) error {
	if c == nil {
		return ErrInvalidPlan
	}
	epoch, err := cells.Epoch()
	if err != nil {
		return ErrInvalidPlan
	}
	handle, err := observability.NewControllerHandle(epoch)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.live != 0 || c.observation != nil {
		return ErrInvalidPlan
	}
	c.observation, c.observationHandle = cells, handle
	return nil
}
func (c *Controller) ObservationHandle() (observability.ControllerHandle, error) {
	if c == nil {
		return observability.ControllerHandle{}, ErrInvalidPlan
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observation == nil {
		return observability.ControllerHandle{}, ErrInvalidPlan
	}
	return c.observationHandle, nil
}
func observedRunKind(kind executionKind) (obs.RunKind, obs.QueueClass, error) {
	switch kind {
	case executionAutomation:
		return obs.RunKind_RUN_KIND_AUTOMATION, obs.QueueClass_QUEUE_CLASS_AUTOMATION_PENDING, nil
	case executionScript:
		return obs.RunKind_RUN_KIND_SCRIPT, obs.QueueClass_QUEUE_CLASS_SCRIPT_PENDING, nil
	default:
		return 0, 0, ErrInvalidPlan
	}
}

// Called exactly where an entry leaves live, under c.mu; this marker is private
// to the entry and survives any number of node traces or repeated close calls.
func (c *Controller) observeTerminalLocked(entry *runEntry, decision Decision) {
	if entry == nil || entry.observationTerminal {
		return
	}
	entry.observationTerminal = true
	kind, _, err := observedRunKind(entry.run.trigger.laneKey().kind)
	if err != nil {
		return
	}
	if decision == DecisionCancelled {
		_ = c.observation.AddRunCancelled(kind)
	}
	if decision == DecisionSuperseded {
		_ = c.observation.AddRunSuperseded(kind)
	}
}
func observationGauge(value uint64) *obs.GaugeMeasurement {
	return &obs.GaugeMeasurement{Result: &obs.GaugeMeasurement_Value{Value: value}}
}
func (c *Controller) ObservationMetrics() ([]*obs.QueueAggregate, *obs.RunMetrics, error) {
	if c == nil {
		return nil, nil, ErrInvalidPlan
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observation == nil || c.live < 0 || c.live > MaxLiveRuns || c.preparing < 0 {
		return nil, nil, ErrInvalidPlan
	}
	var active, pending, capacity, lanes [3]uint64
	for key, l := range c.lanes {
		kind, _, err := observedRunKind(key.kind)
		if err != nil {
			return nil, nil, err
		}
		active[kind] += uint64(len(l.active))
		pending[kind] += uint64(len(l.queue))
		if l.replacement != nil {
			pending[kind]++
		}
		var mode ir.AutomationMode
		var maximum int
		if key.kind == executionAutomation {
			spec := c.plan.items[key.identity]
			mode, maximum = spec.mode, spec.max
		} else {
			id, err := ir.ParseScriptID(key.identity)
			if err != nil {
				return nil, nil, err
			}
			spec := c.plan.scripts[id]
			mode, maximum = spec.mode, spec.max
		}
		switch mode {
		case ir.AutomationQueued:
			if maximum > 1 {
				capacity[kind] += uint64(maximum - 1)
				lanes[kind]++
			}
		case ir.AutomationRestart:
			capacity[kind]++
			lanes[kind]++
		}
	}
	handle, err := c.observationHandle.Wire()
	if err != nil {
		return nil, nil, err
	}
	result := &obs.InstalledRunMetrics{Preparing: observationGauge(uint64(c.preparing)), Controller: handle}
	queues := make([]*obs.QueueAggregate, 0, 2)
	for _, kind := range []obs.RunKind{obs.RunKind_RUN_KIND_AUTOMATION, obs.RunKind_RUN_KIND_SCRIPT} {
		cancelled, superseded, err := c.observation.RunCounters(kind)
		if err != nil {
			return nil, nil, err
		}
		result.Kinds = append(result.Kinds, &obs.RunKindMetrics{Kind: kind, Active: observationGauge(active[kind]), Pending: observationGauge(pending[kind]), CancelledTotal: cancelled, SupersededTotal: superseded})
		class := obs.QueueClass_QUEUE_CLASS_AUTOMATION_PENDING
		if kind == obs.RunKind_RUN_KIND_SCRIPT {
			class = obs.QueueClass_QUEUE_CLASS_SCRIPT_PENDING
		}
		q := &obs.QueueAggregate{QueueClass: class}
		if lanes[kind] == 0 {
			q.Result = &obs.QueueAggregate_NotInstalled{NotInstalled: &emptypb.Empty{}}
		} else {
			cap := capacity[kind]
			if cap > MaxLiveRuns {
				cap = MaxLiveRuns
			}
			snapshot, err := observability.NewQueueSnapshot(lanes[kind], pending[kind], cap, 0, nil, nil)
			if err != nil {
				return nil, nil, err
			}
			value, err := snapshot.Wire()
			if err != nil {
				return nil, nil, err
			}
			counters, err := c.observation.QueueCounters(class)
			if err != nil {
				return nil, nil, err
			}
			value.RejectedTotal = counters.Rejected()
			q.Result = &obs.QueueAggregate_Installed{Installed: value}
		}
		queues = append(queues, q)
	}
	return queues, &obs.RunMetrics{Result: &obs.RunMetrics_Installed{Installed: result}}, nil
}
