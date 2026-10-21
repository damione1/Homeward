package observability

import (
	"math"
	"sync"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

type counter struct {
	value     uint64
	saturated bool
}

func (c *counter) add(amount uint64) {
	if c.saturated {
		return
	}
	if amount > math.MaxUint64-c.value {
		c.value = math.MaxUint64
		c.saturated = true
		return
	}
	c.value += amount
}
func (c counter) wire() *obs.CounterMeasurement {
	if c.saturated {
		return &obs.CounterMeasurement{Result: &obs.CounterMeasurement_Saturated{Saturated: &obs.SaturatedCounter{Value: math.MaxUint64}}}
	}
	return &obs.CounterMeasurement{Result: &obs.CounterMeasurement_Exact{Exact: c.value}}
}
func unavailableCounter() *obs.CounterMeasurement {
	return &obs.CounterMeasurement{Result: &obs.CounterMeasurement_Unavailable{Unavailable: &obs.ReadUnavailable{Reason: obs.ReadUnavailableReason_READ_UNAVAILABLE_REASON_INTEGRITY}}}
}
func unmeasuredCounter() *obs.CounterMeasurement {
	return &obs.CounterMeasurement{Result: &obs.CounterMeasurement_NotMeasured{NotMeasured: &emptypb.Empty{}}}
}

type queueCounters struct{ rejected, lost, coalesced counter }
type gapCounters struct{ rows, lost, unknown counter }
type runCounters struct{ cancelled, superseded counter }

// One bounded mutex makes each multi-counter transition/snapshot coherent.
// No method takes an owner lock or invokes a callback while holding it.
type runtimeQueueGauge struct {
	active, queued, capacity, inflight, pending uint64
	invalid                                     bool
}

type Cells struct {
	runtimeQueues                                   [10]runtimeQueueGauge
	recorderRetries, recorderLost, recorderExcluded counter
	mu                                              sync.Mutex
	epoch                                           ProcessObservationEpoch
	queues                                          [10]queueCounters
	gaps                                            [4]gapCounters
	runs                                            [3]runCounters
	progress                                        counter
	attempts                                        [3]counter
	recovered                                       counter
	journalEvicted                                  counter
}

func NewCells(epoch ProcessObservationEpoch) (*Cells, error) {
	if !epoch.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return &Cells{epoch: epoch}, nil
}
func (c *Cells) valid() bool { return c != nil && c.epoch.valid() }
func (c *Cells) Epoch() (ProcessObservationEpoch, error) {
	if !c.valid() {
		return ProcessObservationEpoch{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return c.epoch, nil
}
func queueIndex(class obs.QueueClass) (int, error) {
	switch class {
	case obs.QueueClass_QUEUE_CLASS_UI, obs.QueueClass_QUEUE_CLASS_EDGE, obs.QueueClass_QUEUE_CLASS_RECORDER, obs.QueueClass_QUEUE_CLASS_VIEW,
		obs.QueueClass_QUEUE_CLASS_AUTOMATION_PENDING, obs.QueueClass_QUEUE_CLASS_SCRIPT_PENDING, obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND,
		obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS:
		return int(class), nil
	case obs.QueueClass_QUEUE_CLASS_UNSPECIFIED:
		return 0, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	default:
		return 0, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
}
func (c *Cells) AddQueueRejected(class obs.QueueClass, amount uint64) error {
	i, err := queueIndex(class)
	if err != nil || !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queues[i].rejected.add(amount)
	return nil
}
func (c *Cells) AddQueueLost(class obs.QueueClass, amount uint64) error {
	i, err := queueIndex(class)
	if err != nil || !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queues[i].lost.add(amount)
	if class == obs.QueueClass_QUEUE_CLASS_RECORDER {
		// Mailbox evictions are known at the producer, including while the
		// Recorder consumer is blocked in SQL. This is one population of the
		// process-lifetime Recorder total; later loss delivery is not another.
		c.recorderLost.add(amount)
	}
	return nil
}
func (c *Cells) AddQueueCoalesced(class obs.QueueClass, amount uint64) error {
	i, err := queueIndex(class)
	if err != nil || !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queues[i].coalesced.add(amount)
	return nil
}
func (c *Cells) AddActionProgressIndeterminate() error {
	if !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.progress.add(1)
	return nil
}
func (c *Cells) AddFirstAttemptIndeterminate(ledger obs.AttemptLedger) error {
	if !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	switch ledger {
	case obs.AttemptLedger_ATTEMPT_LEDGER_VOLATILE, obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE:
	default:
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[ledger].add(1)
	return nil
}
func (c *Cells) AddDurableRecoveredIndeterminate(rows uint64) error {
	if !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recovered.add(rows)
	return nil
}

type KnownLost struct {
	value        uint64
	known, valid bool
}

func KnownLostLowerBound(value uint64) (KnownLost, error) {
	if value == 0 {
		return KnownLost{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	return KnownLost{value: value, known: true, valid: true}, nil
}
func UnknownLost() KnownLost { return KnownLost{valid: true} }
func (k KnownLost) Wire() (*obs.KnownLost, error) {
	if !k.valid || k.known && k.value == 0 {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	if k.known {
		return &obs.KnownLost{Result: &obs.KnownLost_LowerBound{LowerBound: k.value}}, nil
	}
	return &obs.KnownLost{Result: &obs.KnownLost_NotKnown{NotKnown: &emptypb.Empty{}}}, nil
}
func (c *Cells) AddCommittedGap(reason obs.GapReason, known KnownLost) error {
	if !c.valid() || !known.valid || known.known && known.value == 0 {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_GAP_REASON)
	}
	switch reason {
	case obs.GapReason_GAP_REASON_OVERFLOW, obs.GapReason_GAP_REASON_RECONCILIATION, obs.GapReason_GAP_REASON_ABANDONED:
	default:
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_GAP_REASON)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g := &c.gaps[reason]
	g.rows.add(1)
	if known.known {
		g.lost.add(known.value)
	} else {
		g.unknown.add(1)
	}
	return nil
}
func (c *Cells) addRun(kind obs.RunKind, superseded bool) error {
	if !c.valid() {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	switch kind {
	case obs.RunKind_RUN_KIND_AUTOMATION, obs.RunKind_RUN_KIND_SCRIPT:
	default:
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if superseded {
		c.runs[kind].superseded.add(1)
	} else {
		c.runs[kind].cancelled.add(1)
	}
	return nil
}
func (c *Cells) AddRunCancelled(kind obs.RunKind) error  { return c.addRun(kind, false) }
func (c *Cells) AddRunSuperseded(kind obs.RunKind) error { return c.addRun(kind, true) }

type QueueTotals struct {
	rejected, lost, coalesced counter
	valid                     bool
}

func (c *Cells) QueueCounters(class obs.QueueClass) (QueueTotals, error) {
	i, err := queueIndex(class)
	if err != nil || !c.valid() {
		return QueueTotals{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queues[i]
	return QueueTotals{q.rejected, q.lost, q.coalesced, true}, nil
}
func (q QueueTotals) Rejected() *obs.CounterMeasurement {
	if !q.valid {
		return unavailableCounter()
	}
	return q.rejected.wire()
}
func (q QueueTotals) Lost() *obs.CounterMeasurement {
	if !q.valid {
		return unavailableCounter()
	}
	return q.lost.wire()
}
func (q QueueTotals) Coalesced() *obs.CounterMeasurement {
	if !q.valid {
		return unavailableCounter()
	}
	return q.coalesced.wire()
}
func (c *Cells) ActionCounters() *obs.ActionMetrics {
	if !c.valid() {
		return &obs.ActionMetrics{ProgressIndeterminateTotal: unavailableCounter(), FirstAttempts: []*obs.FirstAttemptMetrics{{Ledger: obs.AttemptLedger_ATTEMPT_LEDGER_VOLATILE, IndeterminateTotal: unavailableCounter()}, {Ledger: obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE, IndeterminateTotal: unavailableCounter()}}, DurableRecoveredIndeterminateTotal: unavailableCounter()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return &obs.ActionMetrics{ProgressIndeterminateTotal: c.progress.wire(), FirstAttempts: []*obs.FirstAttemptMetrics{{Ledger: obs.AttemptLedger_ATTEMPT_LEDGER_VOLATILE, IndeterminateTotal: c.attempts[obs.AttemptLedger_ATTEMPT_LEDGER_VOLATILE].wire()}, {Ledger: obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE, IndeterminateTotal: c.attempts[obs.AttemptLedger_ATTEMPT_LEDGER_DURABLE].wire()}}, DurableRecoveredIndeterminateTotal: c.recovered.wire()}
}
func (c *Cells) GapCounters() ([]*obs.CommittedGapMetrics, error) {
	if !c.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := make([]*obs.CommittedGapMetrics, 0, 3)
	for _, reason := range []obs.GapReason{obs.GapReason_GAP_REASON_OVERFLOW, obs.GapReason_GAP_REASON_RECONCILIATION, obs.GapReason_GAP_REASON_ABANDONED} {
		g := c.gaps[reason]
		rows = append(rows, &obs.CommittedGapMetrics{Reason: reason, RowsTotal: g.rows.wire(), KnownLostLowerBoundTotal: g.lost.wire(), RowsWithoutKnownCountTotal: g.unknown.wire()})
	}
	return rows, nil
}
func (c *Cells) RunCounters(kind obs.RunKind) (*obs.CounterMeasurement, *obs.CounterMeasurement, error) {
	if !c.valid() || kind != obs.RunKind_RUN_KIND_AUTOMATION && kind != obs.RunKind_RUN_KIND_SCRIPT {
		return nil, nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[kind].cancelled.wire(), c.runs[kind].superseded.wire(), nil
}

// These fixed cells aggregate only real MQTT, worker and cloud owners. They
// neither allocate an owner registry nor drive queues or command control flow.
func runtimeQueueClass(class obs.QueueClass) bool {
	switch class {
	case obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS:
		return true
	default:
		return false
	}
}
func observationDelta(value uint64, delta int64) (uint64, bool) {
	if delta < 0 {
		amount := uint64(-(delta + 1)) + 1
		if amount > value {
			return 0, false
		}
		return value - amount, true
	}
	if uint64(delta) > math.MaxUint64-value {
		return 0, false
	}
	return value + uint64(delta), true
}
func (c *Cells) ChangeRuntimeQueue(class obs.QueueClass, active, queued, capacity, inflight, pending int64) error {
	if !c.valid() || !runtimeQueueClass(class) {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.runtimeQueues[class]
	valid := true
	values := []struct {
		target *uint64
		delta  int64
	}{{&state.active, active}, {&state.queued, queued}, {&state.capacity, capacity}, {&state.inflight, inflight}, {&state.pending, pending}}
	for _, value := range values {
		next, ok := observationDelta(*value.target, value.delta)
		valid = valid && ok
		*value.target = next
	}
	if !valid {
		c.runtimeQueues[class].invalid = true
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_METRICS)
	}
	c.runtimeQueues[class] = state
	return nil
}
func (c *Cells) RuntimeQueue(class obs.QueueClass) (*obs.QueueAggregate, error) {
	if !c.valid() || !runtimeQueueClass(class) {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_QUEUE_CLASS)
	}
	c.mu.Lock()
	state, totals := c.runtimeQueues[class], c.queues[class]
	c.mu.Unlock()
	result := &obs.QueueAggregate{QueueClass: class}
	if state.invalid || state.queued > state.capacity || state.inflight > state.pending && class == obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS {
		result.Result = &obs.QueueAggregate_Unavailable{Unavailable: &obs.ReadUnavailable{Reason: obs.ReadUnavailableReason_READ_UNAVAILABLE_REASON_INTEGRITY}}
		return result, nil
	}
	if state.active == 0 {
		result.Result = &obs.QueueAggregate_NotInstalled{NotInstalled: &emptypb.Empty{}}
		return result, nil
	}
	snapshot, err := NewQueueSnapshot(state.active, state.queued, state.capacity, 0, nil, nil)
	if err != nil {
		return nil, err
	}
	wire, err := snapshot.Wire()
	if err != nil {
		return nil, err
	}
	wire.RejectedTotal = totals.rejected.wire()
	if class == obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND {
		wire.LostDeliveriesTotal = totals.lost.wire()
	}
	if class == obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS || class == obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS {
		wire.InFlightEntries = gauge(state.inflight)
	}
	if class == obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS {
		wire.PendingEntries = gauge(state.pending)
	}
	result.Result = &obs.QueueAggregate_Installed{Installed: wire}
	return result, nil
}

func (c *Cells) AddRecorderRetries() {
	if c.valid() {
		c.mu.Lock()
		c.recorderRetries.add(1)
		c.mu.Unlock()
	}
}
func (c *Cells) AddRecorderLost(count uint64) {
	if c.valid() {
		c.mu.Lock()
		c.recorderLost.add(count)
		c.mu.Unlock()
	}
}
func (c *Cells) AddRecorderExcluded(count uint64) {
	if c.valid() {
		c.mu.Lock()
		c.recorderExcluded.add(count)
		c.mu.Unlock()
	}
}
func (c *Cells) RecorderCounters() (*obs.CounterMeasurement, *obs.CounterMeasurement, *obs.CounterMeasurement) {
	if !c.valid() {
		return unavailableCounter(), unavailableCounter(), unavailableCounter()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recorderRetries.wire(), c.recorderLost.wire(), c.recorderExcluded.wire()
}
