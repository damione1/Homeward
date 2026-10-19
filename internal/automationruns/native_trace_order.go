package automationruns

// TraceOrdinal is an admitted, in-memory emission position. Its zero value is
// invalid. Positions are comparable only for observations from the same
// Controller identity held with the snapshots; they are not configuration
// revisions, persistent trace IDs, or causality between independent branches.
// Only the native producers can allocate an ordinal.
type TraceOrdinal struct{ value uint64 }

const exhaustedTraceOrdinal = ^uint64(0)

// Value reports whether this position was admitted. Exhaustion never exposes
// the reserved maximum value or reuses an earlier position.
func (o TraceOrdinal) Value() (uint64, bool) {
	if o.value == 0 || o.value == exhaustedTraceOrdinal {
		return 0, false
	}
	return o.value, true
}

// TraceOrderState qualifies the Controller's ability to order its observations.
// It is independent of both routine results and each ring's eviction count.
type TraceOrderState uint8

const (
	TraceOrderInvalid TraceOrderState = iota
	TraceOrderHealthy
	TraceOrderExhausted
)

func (t Trace) Ordinal() TraceOrdinal     { return t.ordinal }
func (t NodeTrace) Ordinal() TraceOrdinal { return t.ordinal }

// TraceOrderState returns Invalid for nil or unadmitted Controllers. A healthy
// state does not promise complete snapshots: the two rings are copied
// separately, and an emission can be preempted after its atomic operation but
// before its ring write. Consumers proving complete order must join producers,
// check Healthy, and reject evictions in either ring. Positions are never
// renumbered to hide missing observations.
func (c *Controller) TraceOrderState() TraceOrderState {
	if c == nil || c.done == nil {
		return TraceOrderInvalid
	}
	if c.traceOrdinal.Load() == exhaustedTraceOrdinal {
		return TraceOrderExhausted
	}
	return TraceOrderHealthy
}

// done is the immutable constructor admission witness, not a program
// generation: later plan publication may change causal generations while the
// Controller continues to own the same ordering space. Closing done does not
// revoke that space or reset its counter.
//
// nextTraceOrdinal is the native emission linearization point, after producer
// preparation/copies and immediately before the bounded ring write. It adds no
// cross-ring mutex, allocation, callback, or effect on business execution.
// The Controller's lifetime owns this counter; changing its plan never resets it.
func (c *Controller) nextTraceOrdinal() TraceOrdinal {
	if c == nil || c.done == nil {
		return TraceOrdinal{}
	}
	for {
		previous := c.traceOrdinal.Load()
		if previous == exhaustedTraceOrdinal {
			return TraceOrdinal{}
		}
		next := previous + 1
		if c.traceOrdinal.CompareAndSwap(previous, next) {
			if next == exhaustedTraceOrdinal {
				return TraceOrdinal{}
			}
			return TraceOrdinal{value: next}
		}
	}
}
