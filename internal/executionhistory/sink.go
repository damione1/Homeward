package executionhistory

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/scriptcommands"
	"github.com/damione1/Homeward/internal/storage"
)

const (
	maxQueueRecords = 1024
	maxQueueBytes   = 4 << 20
	maxBatchRecords = 128
	maxBatchBytes   = 512 << 10
	flushInterval   = 250 * time.Millisecond
)

type Status struct {
	QueueRecords, QueueBytes       int
	Pending                        bool
	CommittedOrdinal               uint64
	Rejected                       [5]uint64
	Degraded, Integrity, Undrained bool
}

type Sink struct {
	mu                                               sync.Mutex
	store                                            *storage.Store
	epoch                                            storage.HistoryEpoch
	opening                                          storage.HistoryEpochOpening
	queue                                            []storage.HistoryRecord
	queueBytes                                       int
	firstQueued                                      time.Time
	pending                                          storage.HistoryBatch
	hasPending                                       bool
	nextOrdinal, nextBatch, committed                uint64
	lastBoundary                                     time.Time
	lossStart                                        time.Time
	started, closing, degraded, integrity, undrained bool
	notify                                           chan struct{}
	done                                             chan struct{}
	cancel                                           context.CancelFunc
	rejected                                         [5]atomic.Uint64
	hasRejected                                      atomic.Bool
	activeLoss                                       [5]atomic.Uint64
}

func New(ctx context.Context, store *storage.Store, epochText string) (*Sink, error) {
	epoch, err := storage.ParseHistoryEpoch(epochText)
	if err != nil || store == nil || !validContext(ctx) {
		return nil, storage.ErrHistoryInvalid
	}
	opening, err := store.OpenExecutionHistoryEpoch(ctx, epoch)
	if errors.Is(err, storage.ErrHistoryCommitUnknown) {
		// The same head-row barrier resolves an ACK lost on epoch opening.
		opening, err = store.OpenExecutionHistoryEpoch(ctx, epoch)
	}
	if err != nil {
		return nil, err
	}
	return &Sink{store: store, epoch: epoch, opening: opening, queue: make([]storage.HistoryRecord, 0, maxQueueRecords),
		lastBoundary: time.Now().UTC(), notify: make(chan struct{}, 1), done: make(chan struct{})}, nil
}

func (s *Sink) EpochOpening() storage.HistoryEpochOpening {
	if s == nil {
		return storage.HistoryEpochOpening{}
	}
	return s.opening
}

func (s *Sink) Start() error {
	if s == nil || s.store == nil || !s.epoch.Valid() || s.notify == nil || s.done == nil {
		return storage.ErrHistoryInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closing {
		return storage.ErrHistoryInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.started = cancel, true
	go s.run(ctx)
	return nil
}

func (s *Sink) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}

func (s *Sink) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	status := Status{QueueRecords: len(s.queue), QueueBytes: s.queueBytes, Pending: s.hasPending,
		CommittedOrdinal: s.committed, Degraded: s.degraded, Integrity: s.integrity, Undrained: s.undrained}
	s.mu.Unlock()
	status.Degraded = status.Degraded || s.hasRejected.Load()
	for i := 1; i < len(status.Rejected); i++ {
		status.Rejected[i] = s.rejected[i].Load()
	}
	return status
}

func (s *Sink) reject(reason storage.HistoryLossReason) {
	if s == nil || reason < 1 || int(reason) >= len(s.rejected) {
		return
	}
	s.hasRejected.Store(true)
	for _, target := range []*atomic.Uint64{&s.rejected[reason], &s.activeLoss[reason]} {
		for {
			old := target.Load()
			if old == math.MaxInt64 || target.CompareAndSwap(old, old+1) {
				break
			}
		}
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// enqueue owns the only transfer lock. Capacity and coarse size admission
// happen before any copy. Failed TryLock and a full queue never wait for SQL.
func (s *Sink) enqueue(estimate int, build func(storage.HistoryTraceID) (storage.HistoryRecordInput, bool)) {
	if s == nil {
		return
	}
	if estimate <= 0 || estimate > 4<<10 {
		s.reject(storage.HistoryLossTooLarge)
		return
	}
	if !s.mu.TryLock() {
		s.reject(storage.HistoryLossContention)
		return
	}
	if s.closing || s.integrity || len(s.queue) == maxQueueRecords || s.queueBytes+estimate*2+512 > maxQueueBytes {
		s.mu.Unlock()
		s.reject(storage.HistoryLossQueueFull)
		return
	}
	if s.nextOrdinal >= math.MaxInt64 {
		s.mu.Unlock()
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	id, _ := storage.NewHistoryTraceID(s.epoch, s.nextOrdinal+1)
	in, ok := build(id)
	if !ok {
		s.mu.Unlock()
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	record, err := storage.NewHistoryRecord(in)
	if err != nil {
		s.mu.Unlock()
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	if s.queueBytes+record.RetainedBytes() > maxQueueBytes {
		s.mu.Unlock()
		s.reject(storage.HistoryLossQueueFull)
		return
	}
	s.nextOrdinal++
	if len(s.queue) == 0 {
		s.firstQueued = time.Now().UTC()
	}
	s.queue = append(s.queue, record)
	s.queueBytes += record.RetainedBytes()
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Sink) ObserveDecision(o automationruns.DecisionObservation) {
	code, ok := decisionCode(o.Decision)
	native, nativeOK := o.NativeOrdinal.Value()
	if !ok || !nativeOK || !validTime(o.At) {
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	estimate := 512 + len(o.Automation) + len(o.Script) + len(o.TriggerKey)
	s.enqueue(estimate, func(id storage.HistoryTraceID) (storage.HistoryRecordInput, bool) {
		in := storage.HistoryRecordInput{ID: id, EmissionTime: o.At, Kind: storage.HistoryDecision, Decision: code,
			NativeOrdinal: native, ObjectID: o.Automation, TriggerKey: o.TriggerKey}
		if o.Script != "" {
			in.ObjectID = o.Script
		}
		valid := runFields(&in, o.Run)
		return in, valid
	})
}

func (s *Sink) ObserveNode(o automationruns.NodeObservation) {
	stage, ok1 := nodeStageCode(o.Stage)
	action, ok2 := actionCode(o.Action)
	failure, ok3 := failureCode(o.Failure)
	wait, ok4 := waitCode(o.Wait)
	native, nativeOK := o.NativeOrdinal.Value()
	if !ok1 || !ok2 || !ok3 || !ok4 || !nativeOK || o.Visit == 0 || !validTime(o.At) ||
		o.BranchDepth < 0 || o.BranchDepth > automationruns.MaxBranchDepth || o.IterationDepth < 0 || o.IterationDepth > automationruns.MaxBranchDepth {
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	for _, part := range o.Branches[:o.BranchDepth] {
		if part < 0 || part > math.MaxInt32 {
			s.reject(storage.HistoryLossInvalidMetadata)
			return
		}
	}
	for _, part := range o.Iterations[:o.IterationDepth] {
		if part < 1 || part > math.MaxInt32 {
			s.reject(storage.HistoryLossInvalidMetadata)
			return
		}
	}
	estimate := 640 + len(o.Source.Object) + len(o.Source.File) + len(o.Source.Path) + 4*(o.BranchDepth+o.IterationDepth)
	s.enqueue(estimate, func(id storage.HistoryTraceID) (storage.HistoryRecordInput, bool) {
		in := storage.HistoryRecordInput{ID: id, EmissionTime: o.At, Kind: storage.HistoryNode, NodeStage: stage,
			NativeOrdinal: native, NodeVisit: o.Visit, ActionKind: action, FailureKind: failure, WaitExit: wait}
		sourceFields(&in, o.Source)
		for _, v := range o.Branches[:o.BranchDepth] {
			in.Branches = append(in.Branches, int32(v))
		}
		for _, v := range o.Iterations[:o.IterationDepth] {
			in.Iterations = append(in.Iterations, int32(v))
		}
		valid := runFields(&in, o.Run)
		return in, valid
	})
}

func (s *Sink) ObserveScriptLink(o automationruns.ScriptLinkObservation) {
	mode, ok := scriptModeCode(o.Mode)
	script, err := o.Script.Value()
	if !ok || err != nil || o.Parent.RunID == 0 || o.Child.ParentID != o.Parent.RunID || !validTime(o.At) {
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	s.enqueue(512+len(script), func(id storage.HistoryTraceID) (storage.HistoryRecordInput, bool) {
		in := storage.HistoryRecordInput{ID: id, EmissionTime: o.At, Kind: storage.HistoryScriptLink, ScriptMode: mode, ObjectID: script}
		valid := runFields(&in, o.Child)
		return in, valid
	})
}

func (s *Sink) ObserveCommand(o scriptcommands.CommandObservation) {
	outcome, ok := commandOutcomeCode(o.Outcome)
	command, err := o.ID.Value()
	if !ok || err != nil || !validTime(o.At) || o.NodeVisit == 0 || o.CommandOrdinal == 0 {
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	estimate := 640 + len(command) + len(o.Source.Object) + len(o.Source.File) + len(o.Source.Path)
	s.enqueue(estimate, func(id storage.HistoryTraceID) (storage.HistoryRecordInput, bool) {
		in := storage.HistoryRecordInput{ID: id, EmissionTime: o.At, Kind: storage.HistoryCommand, CommandOutcome: outcome,
			NodeVisit: o.NodeVisit, CommandOrdinal: o.CommandOrdinal, CommandID: command}
		sourceFields(&in, o.Source)
		valid := runFields(&in, o.Run)
		return in, valid
	})
}

func (s *Sink) ObserveSceneMember(o scenes.SceneMemberObservation) {
	outcome, ok := sceneMemberCode(o.Outcome)
	scene, sceneErr := o.SceneID.Value()
	alias, aliasErr := o.Alias.Value()
	invocation, invocationErr := o.Invocation.Value()
	command, commandErr := o.CommandID.Value()
	if !ok || sceneErr != nil || aliasErr != nil || invocationErr != nil || commandErr != nil ||
		o.Index < 0 || !validTime(o.At) {
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	estimate := 640 + len(scene) + len(alias) + len(invocation) + len(command) + len(o.File)
	s.enqueue(estimate, func(id storage.HistoryTraceID) (storage.HistoryRecordInput, bool) {
		in := storage.HistoryRecordInput{ID: id, EmissionTime: o.At, Kind: storage.HistorySceneMember,
			SceneOutcome: outcome, SceneMember: o.Index, ObjectID: alias, CommandID: command,
			SceneInvocation: invocation, SourceFile: o.File, SourceLine: o.Line, SourceColumn: o.Column,
			NodeVisit: o.Cause.NodeVisit, CommandOrdinal: o.Cause.CommandOrdinal}
		if o.Cause.Source.Object != "" {
			in.SourcePath = o.Cause.Source.Path
		}
		valid := runFields(&in, o.Cause.Run)
		return in, valid
	})
}

func (s *Sink) ObserveSceneInvocation(o scenes.SceneInvocationObservation) {
	outcome, ok := sceneInvocationCode(o.Outcome)
	scene, sceneErr := o.SceneID.Value()
	invocation, invocationErr := o.Invocation.Value()
	if !ok || sceneErr != nil || invocationErr != nil || !validTime(o.At) {
		s.reject(storage.HistoryLossInvalidMetadata)
		return
	}
	s.enqueue(512+len(scene)+len(invocation), func(id storage.HistoryTraceID) (storage.HistoryRecordInput, bool) {
		in := storage.HistoryRecordInput{ID: id, EmissionTime: o.At, Kind: storage.HistorySceneInvocation,
			SceneAggregate: outcome, ObjectID: scene, CommandID: invocation, SceneInvocation: invocation,
			NodeVisit: o.Cause.NodeVisit, CommandOrdinal: o.Cause.CommandOrdinal}
		valid := runFields(&in, o.Cause.Run)
		return in, valid
	})
}

func (s *Sink) closeWithoutWorker() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closing {
		s.closing = true
		s.undrained = s.hasPending || len(s.queue) != 0 || s.hasActiveLoss()
		s.degraded = s.degraded || s.undrained
		close(s.done)
	}
	if s.undrained {
		return storage.ErrHistoryUnavailable
	}
	return nil
}

func (s *Sink) Close(ctx context.Context) error {
	if s == nil || s.done == nil || s.notify == nil || !s.epoch.Valid() || !validContext(ctx) {
		return storage.ErrHistoryInvalid
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return s.closeWithoutWorker()
	}
	s.closing = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	select {
	case <-s.done:
		status := s.Status()
		if status.Integrity {
			return storage.ErrHistoryIntegrity
		}
		if status.Undrained {
			return storage.ErrHistoryUnavailable
		}
		return nil
	case <-ctx.Done():
		s.cancel()
		<-s.done
		return errors.Join(storage.ErrHistoryUnavailable, ctx.Err())
	}
}

func validContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}
