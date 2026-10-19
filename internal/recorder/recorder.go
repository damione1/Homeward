// Package recorder persists accepted states without joining Host admission or
// durable command dispatch. It consumes the sequencer's bounded subscription.
package recorder

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrInvalidRecorder = errors.New("recorder: invalid runtime")
var ErrDrainIncomplete = errors.New("recorder: final history drain incomplete")

const flushInterval = 250 * time.Millisecond
const attemptTimeout = 2 * time.Second
const maxRecordPayloadBytes = 4 << 20

// Writer is the narrow SQL seam. Production supplies the existing Store.
type Writer interface {
	OpenRecorderEpoch(context.Context, storage.RecorderEpoch, time.Time) (storage.RecorderPrevious, storage.TransactionOutcome, error)
	PersistRecorderBatch(context.Context, storage.RecorderBatch) (storage.TransactionOutcome, error)
	RecorderCheckpoint(context.Context, storage.RecorderEpoch) (storage.RecorderCheckpoint, error)
	CloseRecorderEpoch(context.Context, storage.RecorderEpoch, uint64) (storage.TransactionOutcome, error)
}

type Phase uint8

const (
	PhaseUnspecified Phase = iota
	PhaseRunning
	PhaseWriting
	PhaseBackoff
	PhaseStopping
	PhaseFailed
	PhaseStopped
)

type Status struct {
	Phase                  Phase
	HistoryDegraded        bool
	ReconciliationTooLarge bool
	ReconciliationComplete bool
	TotalCapacityBytes     int
	QueueCapacityBytes     int
	QueuedBytes            int
	QueuedEntries          int
	PendingBytes           int
	LostEntries            uint64
	PolicyExcluded         uint64
	Retries                uint64
	Checkpoint             uint64
	Epoch                  string
	LastError              error
	ActiveCoverage         LiveCoverage
}

// LiveCoverage is a volatile, bounded at-risk interval. Its known count is a
// lower bound from actual evictions, not the width of the interval. It is
// separate from the durable SQL coverage and never claims a committed gap.
type LiveCoverage struct {
	epoch         storage.RecorderEpoch
	from, through uint64
	knownLost     uint64
	hasKnownLost  bool
	present       bool
}

func (c LiveCoverage) Bounds() (storage.RecorderEpoch, uint64, uint64, bool) {
	return c.epoch, c.from, c.through, c.present
}
func (c LiveCoverage) KnownLost() (uint64, bool) { return c.knownLost, c.hasKnownLost }

type Recorder struct {
	sequence                                           *sequencer.Sequencer
	writer                                             Writer
	limits                                             Limits
	epoch                                              storage.RecorderEpoch
	sub                                                *sequencer.Subscription
	mu                                                 sync.Mutex
	status                                             Status
	stopCtx                                            context.Context
	stopBoundary                                       uint64
	readCtx                                            context.Context
	readCancel                                         context.CancelFunc
	done                                               chan struct{}
	doneErr                                            error
	activeLossFrom, activeLossThrough, activeKnownLost uint64
	activeLossPresent, activeKnownValid                bool
}

type pending struct {
	from, through uint64
	rows          []storage.RecorderRecord
	gaps          []storage.RecorderGap
	excluded      uint64
	abandoned     uint64
	needsImage    bool
	bytes         int
	firstEntryAt  time.Time
}

// Start prepares the initial subscription and epoch before any producer is
// admitted. A failed SQL start closes the mailbox and returns no Recorder.
func Start(ctx context.Context, sequence *sequencer.Sequencer, writer Writer, limits Limits) (*Recorder, error) {
	if nilInterface(ctx) || sequence == nil || !limits.Valid() || nilInterface(writer) {
		return nil, ErrInvalidRecorder
	}
	capture, err := sequence.CaptureRecorder(nil, sequencer.Cursor{}, limits.ImageOrBatchBytes(), limits.QueueBytes())
	if err != nil {
		return nil, err
	}
	epochText, boundary, err := capture.Cursor.Components()
	if err != nil {
		_ = capture.Subscription.Close()
		return nil, err
	}
	epoch, err := storage.ParseRecorderEpoch(epochText)
	if err != nil {
		_ = capture.Subscription.Close()
		return nil, err
	}
	initial := make([]storage.RecorderSnapshotRecord, 0, len(capture.States))
	for _, state := range capture.States {
		row, e := storage.NewRecorderSnapshotRecord(state.State, maxRecordPayloadBytes)
		if e != nil {
			_ = capture.Subscription.Close()
			return nil, e
		}
		initial = append(initial, row)
	}
	image := &storage.RecorderImage{Boundary: boundary, States: initial}
	var startupGaps []storage.RecorderGap
	if boundary > 0 {
		startupGaps = []storage.RecorderGap{{From: 1, Through: boundary, Reason: storage.RecorderGapReconciliation, UncertainEnvelope: true}}
	}
	batch, err := storage.NewRecorderBatch(epoch, 0, boundary, nil, startupGaps, 0, image)
	if err != nil {
		_ = capture.Subscription.Close()
		return nil, err
	}
	// The batch owns deep copies. Drop construction buffers before pgx stages
	// the initial transaction; the live mailbox remains the only queue owner.
	capture.States = nil
	initial = nil
	image = nil
	openCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	previous, outcome, err := writer.OpenRecorderEpoch(openCtx, epoch, time.Now().UTC())
	cancel()
	if outcome != storage.TransactionCommitted || err != nil {
		_ = capture.Subscription.Close()
		return nil, errors.Join(ErrInvalidRecorder, err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	outcome, err = writer.PersistRecorderBatch(writeCtx, batch)
	cancel()
	if outcome == storage.TransactionIndeterminate {
		verifyCtx, verifyCancel := context.WithTimeout(ctx, attemptTimeout)
		checkpoint, readErr := writer.RecorderCheckpoint(verifyCtx, epoch)
		verifyCancel()
		if readErr == nil && checkpoint.Confirms(batch) {
			outcome, err = storage.TransactionCommitted, nil
		}
	}
	if outcome != storage.TransactionCommitted || err != nil {
		_ = capture.Subscription.Close()
		return nil, errors.Join(ErrInvalidRecorder, err)
	}
	readCtx, readCancel := context.WithCancel(context.Background())
	r := &Recorder{sequence: sequence, writer: writer, limits: limits, epoch: epoch, sub: capture.Subscription, readCtx: readCtx, readCancel: readCancel, done: make(chan struct{})}
	r.status = Status{Phase: PhaseRunning, HistoryDegraded: previous.Present || boundary > 0, ReconciliationComplete: true,
		TotalCapacityBytes: limits.TotalBytes(), QueueCapacityBytes: limits.QueueBytes(), Checkpoint: boundary, Epoch: epochText}
	go r.run(boundary)
	return r, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (r *Recorder) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}

func (r *Recorder) Status() (Status, error) {
	if r == nil || r.done == nil {
		return Status{}, ErrInvalidRecorder
	}
	r.mu.Lock()
	status := r.status
	sub := r.sub
	activeFrom, activeThrough, activeCount := r.activeLossFrom, r.activeLossThrough, r.activeKnownLost
	activePresent, activeValid := r.activeLossPresent, r.activeKnownValid
	r.mu.Unlock()
	cursor, cursorErr := r.sequence.RecorderCursor()
	if cursorErr == nil {
		epochText, through, err := cursor.Components()
		if err == nil && epochText == status.Epoch && through > status.Checkpoint {
			status.ActiveCoverage = LiveCoverage{epoch: r.epoch, from: status.Checkpoint + 1, through: through, present: true}
			if activePresent && activeValid && activeFrom >= status.ActiveCoverage.from && activeThrough <= through {
				status.ActiveCoverage.knownLost = activeCount
				status.ActiveCoverage.hasKnownLost = activeCount > 0
			}
		}
	}
	if sub != nil {
		queue, err := sub.Status()
		if err == nil {
			status.QueuedBytes, status.QueuedEntries = queue.QueuedBytes, queue.Queued
			if queue.Lost > status.LostEntries {
				status.LostEntries = queue.Lost
			}
			if queue.Lost > 0 || queue.PendingLoss != nil || !queue.ReplayValid {
				status.HistoryDegraded = true
				status.ReconciliationComplete = false
			}
			if queue.PendingLoss != nil && status.ActiveCoverage.present {
				fromEpoch, from, fromErr := queue.PendingLoss.From.Components()
				toEpoch, through, toErr := queue.PendingLoss.To.Components()
				if fromErr == nil && toErr == nil && fromEpoch == status.Epoch && toEpoch == status.Epoch &&
					from >= status.ActiveCoverage.from && through <= status.ActiveCoverage.through &&
					queue.PendingLoss.Count > 0 &&
					queue.PendingLoss.Count <= ^uint64(0)-status.ActiveCoverage.knownLost {
					status.ActiveCoverage.knownLost += queue.PendingLoss.Count
					status.ActiveCoverage.hasKnownLost = true
				}
			}
		}
	}
	return status, nil
}

// Stop is called after admissions and producers drain. Its context bounds the
// final SQL work; Done closes only after the worker leaves every SQL call.
func (r *Recorder) Stop(ctx context.Context) error {
	if r == nil || r.done == nil || nilInterface(ctx) {
		return ErrInvalidRecorder
	}
	boundary, err := r.sequence.RecorderCursor()
	if err != nil {
		return err
	}
	_, through, err := boundary.Components()
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.stopCtx == nil {
		r.stopCtx, r.stopBoundary = ctx, through
		r.status.Phase = PhaseStopping
		r.readCancel()
	}
	r.mu.Unlock()
	select {
	case <-r.done:
		r.mu.Lock()
		result := r.doneErr
		r.mu.Unlock()
		return result
	case <-ctx.Done():
		return errors.Join(ErrDrainIncomplete, ctx.Err())
	}
}

func (r *Recorder) stopping() (context.Context, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopCtx, r.stopBoundary, r.stopCtx != nil
}

func (r *Recorder) phase(phase Phase, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Phase = phase
	if err != nil {
		r.status.LastError = err
		r.status.HistoryDegraded = true
	}
}

func (r *Recorder) run(checkpoint uint64) {
	var result error
	defer func() {
		r.readCancel()
		_ = r.sub.Close()
		r.mu.Lock()
		r.doneErr = result
		if result != nil {
			r.status.Phase = PhaseFailed
			r.status.LastError = result
			r.status.HistoryDegraded = true
		} else {
			r.status.Phase = PhaseStopped
		}
		r.mu.Unlock()
		close(r.done)
	}()
	p := pending{from: checkpoint, through: checkpoint}
	needsImage := false
	nextImageAttempt := time.Time{}
	imageBackoff := time.Second
	for {
		if stopCtx, boundary, stopping := r.stopping(); stopping {
			result = r.drain(stopCtx, boundary, &p, &checkpoint)
			return
		}
		// An uninterrupted stream of policy-excluded edges must not postpone
		// its SQL checkpoint by repeatedly satisfying Next before its timeout.
		if p.ready() && !p.firstEntryAt.IsZero() && !time.Now().Before(p.firstEntryAt.Add(flushInterval)) {
			if err := r.flush(&p, &checkpoint); err != nil {
				result = err
				return
			}
			continue
		}
		if needsImage && !nextImageAttempt.IsZero() && !time.Now().Before(nextImageAttempt) {
			if p.ready() && p.through > p.from {
				if err := r.flush(&p, &checkpoint); err != nil {
					result = err
					return
				}
			}
			ok, err := r.reconcile(&p, &checkpoint, 0)
			if err != nil {
				result = err
				return
			}
			if ok {
				needsImage, nextImageAttempt, imageBackoff = false, time.Time{}, time.Second
			} else {
				nextImageAttempt = time.Now().Add(imageBackoff)
				imageBackoff = growBackoff(imageBackoff)
			}
			continue
		}
		wait := flushInterval
		if !p.firstEntryAt.IsZero() {
			wait = time.Until(p.firstEntryAt.Add(flushInterval))
			if wait < 0 {
				wait = 0
			}
			if wait == 0 && !p.ready() {
				wait = flushInterval
			}
		}
		if needsImage && !nextImageAttempt.IsZero() {
			until := time.Until(nextImageAttempt)
			if until < wait {
				wait = until
			}
		}
		if wait < 0 {
			wait = 0
		}
		nextCtx, cancel := context.WithTimeout(r.readCtx, wait)
		delivery, err := r.sub.Next(nextCtx)
		cancel()
		if errors.Is(err, context.Canceled) {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			if p.ready() && p.through > p.from {
				if err := r.flush(&p, &checkpoint); err != nil {
					result = err
					return
				}
			}
			continue
		}
		if errors.Is(err, sequencer.ErrRecorderGap) {
			loss, ok := delivery.Loss()
			if !ok {
				result = ErrInvalidRecorder
				return
			}
			if err := r.addLoss(loss); err != nil {
				result = err
				return
			}
			if p.ready() && p.through > p.from {
				if err := r.flush(&p, &checkpoint); err != nil {
					result = err
					return
				}
			}
			before := checkpoint
			captured, e := r.reconcile(&p, &checkpoint, loss.Count)
			if e != nil {
				result = e
				return
			}
			if !captured {
				if checkpoint == before {
					if e := p.addLoss(loss, r.limits, r.status.Epoch); e != nil {
						result = e
						return
					}
				}
				needsImage = true
				nextImageAttempt = time.Now().Add(imageBackoff)
				imageBackoff = growBackoff(imageBackoff)
			}
			continue
		}
		if err != nil {
			result = err
			return
		}
		entry, ok := delivery.Entry()
		if !ok {
			result = ErrInvalidRecorder
			return
		}
		if err := r.accept(&p, entry); err != nil {
			result = err
			return
		}
		if p.bytes >= r.limits.ImageOrBatchBytes() || len(p.rows) >= FlushStates {
			if !p.ready() {
				p.collapseToGap()
				r.markCoverageIncomplete()
			}
		}
		if p.needsImage {
			needsImage = true
			nextImageAttempt = time.Now()
		}
		if p.ready() && (len(p.rows) >= FlushStates || p.encodedBytes() >= FlushEncodedBytes || p.bytes >= r.limits.ImageOrBatchBytes()) {
			if err := r.flush(&p, &checkpoint); err != nil {
				result = err
				return
			}
		}
	}
}

func growBackoff(current time.Duration) time.Duration {
	if current >= 30*time.Second {
		return 30 * time.Second
	}
	if current >= 16*time.Second {
		return 30 * time.Second
	}
	return current * 2
}

func (r *Recorder) addLoss(loss sequencer.Loss) error {
	fromEpoch, from, err := loss.From.Components()
	if err != nil {
		return ErrInvalidRecorder
	}
	toEpoch, through, err := loss.To.Components()
	if err != nil || fromEpoch != r.status.Epoch || toEpoch != r.status.Epoch || from == 0 || through < from || loss.Count == 0 || loss.Count > through-from+1 {
		return ErrInvalidRecorder
	}
	r.recordLoss(from, through, loss.Count)
	return nil
}

func (r *Recorder) recordLoss(from, through, count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.HistoryDegraded = true
	r.status.ReconciliationComplete = false
	if count > ^uint64(0)-r.status.LostEntries {
		r.status.LostEntries = ^uint64(0)
	} else {
		r.status.LostEntries += count
	}
	if !r.activeLossPresent {
		r.activeLossFrom, r.activeLossThrough, r.activeKnownLost = from, through, count
		r.activeLossPresent, r.activeKnownValid = true, true
	} else {
		if from < r.activeLossFrom {
			r.activeLossFrom = from
		}
		if through > r.activeLossThrough {
			r.activeLossThrough = through
		}
		if count > ^uint64(0)-r.activeKnownLost {
			r.activeKnownValid = false
		} else {
			r.activeKnownLost += count
		}
	}
}

func (r *Recorder) trimActiveLoss(checkpoint uint64) {
	if !r.activeLossPresent {
		return
	}
	if r.activeLossThrough <= checkpoint {
		r.activeLossFrom, r.activeLossThrough, r.activeKnownLost = 0, 0, 0
		r.activeLossPresent, r.activeKnownValid = false, false
	} else if r.activeLossFrom <= checkpoint {
		r.activeLossFrom = checkpoint + 1
		r.activeKnownLost, r.activeKnownValid = 0, false
	}
}

func (r *Recorder) markCoverageIncomplete() {
	r.mu.Lock()
	r.status.HistoryDegraded = true
	r.status.ReconciliationComplete = false
	r.mu.Unlock()
}

func (r *Recorder) accept(p *pending, entry sequencer.Entry) error {
	epochText, sequence, err := entry.Cursor().Components()
	if err != nil || epochText != r.status.Epoch {
		return ErrInvalidRecorder
	}
	if sequence <= p.from {
		r.mu.Lock()
		degraded := r.status.ReconciliationTooLarge
		r.mu.Unlock()
		if degraded {
			return nil
		} // already covered by an explicit uncertain envelope
		return ErrInvalidRecorder
	}
	if sequence > p.through {
		p.through = sequence
	}
	if p.firstEntryAt.IsZero() {
		p.firstEntryAt = time.Now()
	}
	if len(p.gaps) == 1 && p.gaps[0].UncertainEnvelope && p.gaps[0].From == p.from+1 {
		p.gaps[0].Through = p.through
	}
	switch entry.Kind() {
	case sequencer.KindState:
		_, state, _, ok := entry.State()
		instance, _, causality, hasOrigin := entry.Origin()
		if !ok || !hasOrigin {
			return ErrInvalidRecorder
		}
		row, err := storage.NewRecorderRecord(sequence, instance, state, causality, entry.ReceivedAt(), maxRecordPayloadBytes)
		if errors.Is(err, storage.ErrRecorderTooLarge) {
			p.needsImage = true
			count := uint64(1)
			p.gaps = append(p.gaps, storage.RecorderGap{From: sequence, Through: sequence, KnownLost: &count, Reason: storage.RecorderGapAbandoned})
			if len(p.gaps) > 32 {
				p.collapseToGap()
			}
			r.recordLoss(sequence, sequence, 1)
			return nil
		}
		if err != nil {
			return err
		}
		charge := row.RetainedBytes()
		if charge > r.limits.ImageOrBatchBytes()-p.bytes {
			p.needsImage = true
			// A valid state can exceed the batch slice. Preserve control and
			// declare the historical loss instead of retaining it indefinitely.
			count := uint64(1)
			p.gaps = append(p.gaps, storage.RecorderGap{From: sequence, Through: sequence, KnownLost: &count, Reason: storage.RecorderGapAbandoned})
			if len(p.gaps) > 32 {
				p.collapseToGap()
			}
			r.recordLoss(sequence, sequence, 1)
			return nil
		}
		p.rows = append(p.rows, row)
		p.bytes += charge
	case sequencer.KindEvent, sequencer.KindHealth, sequencer.KindRegistry, sequencer.KindCapabilities, sequencer.KindNativeMQTTEdge:
		if p.excluded == ^uint64(0) {
			return ErrInvalidRecorder
		}
		p.excluded++
		r.mu.Lock()
		if r.status.PolicyExcluded < ^uint64(0) {
			r.status.PolicyExcluded++
		}
		r.mu.Unlock()
	case sequencer.KindUnspecified:
		return ErrInvalidRecorder
	default:
		return ErrInvalidRecorder
	}
	r.mu.Lock()
	r.status.PendingBytes = p.bytes
	r.mu.Unlock()
	return nil
}

func (p *pending) encodedBytes() int {
	total := 0
	for _, row := range p.rows {
		total += row.EncodedBytes()
	}
	return total
}

func (p *pending) ready() bool {
	if p.through <= p.from {
		return false
	}
	if len(p.gaps) == 1 && p.gaps[0].UncertainEnvelope && p.gaps[0].From == p.from+1 && p.gaps[0].Through == p.through {
		return true
	}
	covered := uint64(len(p.rows)) + p.excluded
	for _, gap := range p.gaps {
		if gap.KnownLost == nil {
			return false
		}
		if *gap.KnownLost > ^uint64(0)-covered {
			return false
		}
		covered += *gap.KnownLost
	}
	return covered == p.through-p.from
}

func (p *pending) addLoss(loss sequencer.Loss, limits Limits, expectedEpoch string) error {
	fromEpoch, from, err := loss.From.Components()
	if err != nil {
		return err
	}
	throughEpoch, through, err := loss.To.Components()
	if err != nil || fromEpoch != expectedEpoch || throughEpoch != expectedEpoch || from <= p.from || through < from || loss.Count == 0 || loss.Count > through-from+1 {
		return ErrInvalidRecorder
	}
	if through > p.through {
		p.through = through
	}
	count := loss.Count
	for _, previous := range p.gaps {
		if previous.KnownLost != nil {
			if *previous.KnownLost > ^uint64(0)-count {
				return ErrInvalidRecorder
			}
			count += *previous.KnownLost
		}
	}
	if count > p.through-p.from {
		return ErrInvalidRecorder
	}
	// The mailbox's Count remains a known lower bound. The full interval is
	// uncertain because queued entries can lie between evicted positions.
	p.gaps = []storage.RecorderGap{{From: p.from + 1, Through: p.through, KnownLost: &count, Reason: storage.RecorderGapOverflow, UncertainEnvelope: true}}
	if p.bytes > limits.ImageOrBatchBytes() {
		p.collapseToGap()
	}
	return nil
}

func (p *pending) collapseToGap() {
	if p.through <= p.from {
		return
	}
	p.needsImage = true
	// Deliberately abandon the whole accepted interval. Retained state rows
	// now discarded and prior mailbox losses have known counts; the interval's
	// width is never called a state count.
	var known *uint64
	dropped := uint64(len(p.rows))
	if dropped > ^uint64(0)-p.abandoned {
		p.abandoned = ^uint64(0)
	} else {
		p.abandoned += dropped
	}
	count := dropped
	for _, gap := range p.gaps {
		if gap.KnownLost != nil {
			if *gap.KnownLost > ^uint64(0)-count {
				count = ^uint64(0)
			} else {
				count += *gap.KnownLost
			}
		}
	}
	if count > 0 {
		known = &count
	}
	p.rows = nil
	p.gaps = []storage.RecorderGap{{From: p.from + 1, Through: p.through, KnownLost: known, Reason: storage.RecorderGapAbandoned, UncertainEnvelope: true}}
	p.bytes = 0
	p.firstEntryAt = time.Time{}
}

func (r *Recorder) flush(p *pending, checkpoint *uint64) error {
	if p.through == p.from {
		return nil
	}
	if !p.ready() {
		return ErrInvalidRecorder
	}
	batch, err := storage.NewRecorderBatch(r.epoch, p.from, p.through, p.rows, p.gaps, p.excluded, nil)
	if errors.Is(err, storage.ErrRecorderTooLarge) {
		p.collapseToGap()
		batch, err = storage.NewRecorderBatch(r.epoch, p.from, p.through, p.rows, p.gaps, p.excluded, nil)
		r.markCoverageIncomplete()
	}
	if err != nil {
		return err
	}
	if batch.HasGaps() {
		r.markCoverageIncomplete()
	}
	retained := p.bytes
	abandoned := p.abandoned
	*p = pending{from: batch.Through(), through: batch.Through()}
	r.mu.Lock()
	r.status.PendingBytes = retained
	r.mu.Unlock()
	if err := r.write(batch); err != nil {
		return err
	}
	*checkpoint = batch.Through()
	r.mu.Lock()
	r.status.Checkpoint = *checkpoint
	r.trimActiveLoss(*checkpoint)
	r.status.PendingBytes = 0
	if abandoned > ^uint64(0)-r.status.LostEntries {
		r.status.LostEntries = ^uint64(0)
	} else {
		r.status.LostEntries += abandoned
	}
	r.mu.Unlock()
	return nil
}

func (r *Recorder) reconcile(p *pending, checkpoint *uint64, knownLoss uint64) (bool, error) {
	// A pending interval whose coverage is already uncertain cannot be written
	// as a normal batch. Release its encoded rows before admitting a new image,
	// while retaining their known dropped count in the gap envelope.
	if !p.ready() && p.bytes > 0 {
		p.collapseToGap()
		r.markCoverageIncomplete()
	}
	if uint64(len(p.rows)) > ^uint64(0)-knownLoss {
		return false, ErrInvalidRecorder
	}
	knownLoss += uint64(len(p.rows))
	for _, gap := range p.gaps {
		if gap.KnownLost != nil {
			if *gap.KnownLost > ^uint64(0)-knownLoss {
				return false, ErrInvalidRecorder
			}
			knownLoss += *gap.KnownLost
		}
	}
	after, err := sequencer.ParseCursor(r.status.Epoch, *checkpoint)
	if err != nil {
		return false, err
	}
	capture, err := r.sequence.CaptureRecorder(r.sub, after, r.limits.ImageOrBatchBytes(), r.limits.QueueBytes())
	if errors.Is(err, sequencer.ErrRecorderSnapshotTooLarge) {
		r.mu.Lock()
		r.status.ReconciliationTooLarge = true
		r.status.HistoryDegraded = true
		r.status.ReconciliationComplete = false
		r.mu.Unlock()
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	r.sub = capture.Subscription
	r.mu.Unlock()
	if capture.Abandoned != nil && capture.Abandoned.PendingLoss != nil {
		count := capture.Abandoned.PendingLoss.Count
		if count > ^uint64(0)-knownLoss {
			return false, ErrInvalidRecorder
		}
		knownLoss += count
	}
	_, through, err := capture.Cursor.Components()
	if err != nil {
		return false, err
	}
	rows := make([]storage.RecorderSnapshotRecord, 0, len(capture.States))
	imageTooLarge := false
	for _, state := range capture.States {
		row, e := storage.NewRecorderSnapshotRecord(state.State, maxRecordPayloadBytes)
		if errors.Is(e, storage.ErrRecorderTooLarge) {
			imageTooLarge = true
			rows = nil
			break
		}
		if e != nil {
			return false, e
		}
		rows = append(rows, row)
	}
	var gaps []storage.RecorderGap
	if through > *checkpoint {
		var count *uint64
		if knownLoss > 0 {
			copied := knownLoss
			count = &copied
		}
		gaps = []storage.RecorderGap{{From: *checkpoint + 1, Through: through, KnownLost: count, Reason: storage.RecorderGapReconciliation, UncertainEnvelope: true}}
	}
	abandoned := p.abandoned
	if uint64(len(p.rows)) > ^uint64(0)-abandoned {
		abandoned = ^uint64(0)
	} else {
		abandoned += uint64(len(p.rows))
	}
	var image *storage.RecorderImage
	if !imageTooLarge {
		image = &storage.RecorderImage{Boundary: through, States: rows}
	}
	batch, err := storage.NewRecorderBatch(r.epoch, *checkpoint, through, nil, gaps, p.excluded, image)
	if errors.Is(err, storage.ErrRecorderTooLarge) {
		imageTooLarge = true
		image = nil
		if through > *checkpoint {
			batch, err = storage.NewRecorderBatch(r.epoch, *checkpoint, through, nil, gaps, p.excluded, nil)
		}
	}
	if imageTooLarge && through == *checkpoint {
		r.mu.Lock()
		r.status.ReconciliationTooLarge = true
		r.status.HistoryDegraded = true
		r.status.ReconciliationComplete = false
		r.mu.Unlock()
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The frozen batch owns its payloads. Do not retain the source projection
	// and encoded rows across the SQL write and its driver staging allocation.
	capture.States = nil
	rows = nil
	image = nil
	*p = pending{from: through, through: through}
	r.mu.Lock()
	r.status.PendingBytes = capture.EstimatedBytes
	r.mu.Unlock()
	if err := r.write(batch); err != nil {
		return false, err
	}
	*checkpoint = through
	r.mu.Lock()
	r.status.Checkpoint = through
	r.trimActiveLoss(through)
	if abandoned > ^uint64(0)-r.status.LostEntries {
		r.status.LostEntries = ^uint64(0)
	} else {
		r.status.LostEntries += abandoned
	}
	r.status.ReconciliationTooLarge = imageTooLarge
	r.status.ReconciliationComplete = !imageTooLarge
	r.status.PendingBytes = 0
	if len(gaps) > 0 {
		r.status.HistoryDegraded = true
	}
	r.mu.Unlock()
	return !imageTooLarge, nil
}

func (r *Recorder) write(batch storage.RecorderBatch) error {
	backoff := time.Second
	indeterminate := false
	for {
		base := context.Background()
		if stopCtx, _, stopping := r.stopping(); stopping {
			base = stopCtx
		}
		var err error
		if indeterminate {
			verifyCtx, verifyCancel := context.WithTimeout(base, attemptTimeout)
			checkpoint, checkErr := r.writer.RecorderCheckpoint(verifyCtx, r.epoch)
			verifyCancel()
			if checkErr == nil {
				if checkpoint.Confirms(batch) {
					r.phase(PhaseRunning, nil)
					return nil
				}
				if checkpoint.Sequence != batch.From() {
					return storage.ErrRecorderIntegrity
				}
				if batch.From() == batch.Through() && checkpoint.HasLastBatch && checkpoint.LastFrom == batch.From() && checkpoint.LastThrough == batch.Through() {
					return storage.ErrRecorderIntegrity
				}
				indeterminate = false
			} else {
				err = checkErr
			}
		}
		if !indeterminate {
			r.phase(PhaseWriting, nil)
			ctx, cancel := context.WithTimeout(base, attemptTimeout)
			outcome, writeErr := r.writer.PersistRecorderBatch(ctx, batch)
			cancel()
			err = writeErr
			if outcome == storage.TransactionCommitted && err == nil {
				r.phase(PhaseRunning, nil)
				return nil
			}
			if outcome == storage.TransactionIndeterminate {
				indeterminate = true
				continue
			}
		}
		if errors.Is(err, storage.ErrRecorderIntegrity) || errors.Is(err, storage.ErrRecorderInvalid) {
			return err
		}
		r.mu.Lock()
		r.status.Retries++
		r.status.HistoryDegraded = true
		r.status.LastError = err
		r.mu.Unlock()
		if _, _, stopping := r.stopping(); stopping {
			return errors.Join(ErrDrainIncomplete, err)
		}
		r.phase(PhaseBackoff, err)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-r.readCtx.Done():
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		backoff = growBackoff(backoff)
	}
}

func (r *Recorder) drain(ctx context.Context, boundary uint64, p *pending, checkpoint *uint64) error {
	r.phase(PhaseStopping, nil)
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrDrainIncomplete, err)
		}
		status, err := r.sub.Status()
		if err != nil {
			return err
		}
		if status.Queued == 0 && status.PendingLoss == nil {
			break
		}
		delivery, nextErr := r.sub.Next(ctx)
		if errors.Is(nextErr, sequencer.ErrRecorderGap) {
			loss, ok := delivery.Loss()
			if !ok {
				return ErrInvalidRecorder
			}
			if err := r.addLoss(loss); err != nil {
				return err
			}
			if err := p.addLoss(loss, r.limits, r.status.Epoch); err != nil {
				return err
			}
			continue
		}
		if nextErr != nil {
			return nextErr
		}
		entry, ok := delivery.Entry()
		if !ok {
			return ErrInvalidRecorder
		}
		if err := r.accept(p, entry); err != nil {
			return err
		}
		if p.bytes >= r.limits.ImageOrBatchBytes() || len(p.rows) >= FlushStates {
			if !p.ready() {
				p.collapseToGap()
				r.markCoverageIncomplete()
			}
		}
		if p.ready() && (len(p.rows) >= FlushStates || p.encodedBytes() >= FlushEncodedBytes || p.bytes >= r.limits.ImageOrBatchBytes()) {
			if err := r.flush(p, checkpoint); err != nil {
				return err
			}
		}
	}
	if p.through < boundary {
		p.through = boundary
	}
	if !p.ready() && p.through > p.from {
		p.collapseToGap()
		r.markCoverageIncomplete()
	}
	if p.through > p.from {
		if err := r.flush(p, checkpoint); err != nil {
			return err
		}
	}
	if *checkpoint != boundary {
		return ErrDrainIncomplete
	}
	closeCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	outcome, err := r.writer.CloseRecorderEpoch(closeCtx, r.epoch, *checkpoint)
	cancel()
	if outcome == storage.TransactionCommitted && err == nil {
		return nil
	}
	if outcome == storage.TransactionIndeterminate {
		verifyCtx, verifyCancel := context.WithTimeout(ctx, attemptTimeout)
		checkpointState, readErr := r.writer.RecorderCheckpoint(verifyCtx, r.epoch)
		verifyCancel()
		if readErr == nil && checkpointState.CleanClose && checkpointState.Sequence == *checkpoint {
			return nil
		}
	}
	return errors.Join(ErrDrainIncomplete, err)
}
