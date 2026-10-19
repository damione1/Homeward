// Package historycoverage admits logical recorder coverage for historical and
// statistical readers. UTC presentation is never evidence of gap placement.
package historycoverage

import (
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

var ErrInvalidCoverage = errors.New("history coverage: invalid projection")

type Verdict uint8

const (
	VerdictUnspecified Verdict = iota
	CompleteUnderPolicy
	Incomplete
	Truncated
)

type IssueKind uint8

const (
	IssueUnspecified IssueKind = iota
	StoredGap
	EpochBridge
	UncommittedTail
	InitialSnapshotPending
)

type TemporalPlacement uint8

const (
	TemporalUnspecified TemporalPlacement = iota
	TemporalNotRequested
	TemporalUnknown
)

type Position struct {
	epoch    storage.RecorderEpoch
	sequence uint64
}

func NewPosition(epoch storage.RecorderEpoch, sequence uint64) (Position, error) {
	if _, err := epoch.Hex(); err != nil {
		return Position{}, ErrInvalidCoverage
	}
	return Position{epoch: epoch, sequence: sequence}, nil
}
func (p Position) Epoch() storage.RecorderEpoch { return p.epoch }
func (p Position) Sequence() uint64             { return p.sequence }

type UTCWindow struct{ start, end time.Time }

func NewUTCWindow(start, end time.Time) (UTCWindow, error) {
	if start.IsZero() || !start.Before(end) {
		return UTCWindow{}, ErrInvalidCoverage
	}
	return UTCWindow{start: start.UTC(), end: end.UTC()}, nil
}
func (w UTCWindow) Bounds() (time.Time, time.Time) { return w.start, w.end }

type EpochScope struct {
	epoch                                     storage.RecorderEpoch
	from, through, checkpoint, policyExcluded uint64
}

func (s EpochScope) Epoch() storage.RecorderEpoch { return s.epoch }
func (s EpochScope) Bounds() (uint64, uint64)     { return s.from, s.through }
func (s EpochScope) Checkpoint() uint64           { return s.checkpoint }

// PolicyExcluded is the whole epoch's count, not a count for Bounds.
func (s EpochScope) PolicyExcluded() uint64 { return s.policyExcluded }

type Issue struct {
	kind          IssueKind
	from, through Position
	reason        storage.RecorderGapReason
	knownLost     uint64
	hasKnownLost  bool
	uncertain     bool
	placement     TemporalPlacement
}

func (i Issue) Kind() IssueKind                      { return i.kind }
func (i Issue) Bounds() (Position, Position)         { return i.from, i.through }
func (i Issue) Reason() storage.RecorderGapReason    { return i.reason }
func (i Issue) KnownLost() (uint64, bool)            { return i.knownLost, i.hasKnownLost }
func (i Issue) UncertainEnvelope() bool              { return i.uncertain }
func (i Issue) TemporalPlacement() TemporalPlacement { return i.placement }

type Coverage struct {
	verdict       Verdict
	truncation    storage.RecorderCoverageTruncation
	from, through Position
	scopes        []EpochScope
	issues        []Issue
	window        *UTCWindow
}

func (c Coverage) Verdict() Verdict                               { return c.verdict }
func (c Coverage) Truncation() storage.RecorderCoverageTruncation { return c.truncation }
func (c Coverage) Bounds() (Position, Position)                   { return c.from, c.through }
func (c Coverage) Scopes() []EpochScope                           { return append([]EpochScope(nil), c.scopes...) }
func (c Coverage) Issues() []Issue                                { return append([]Issue(nil), c.issues...) }
func (c Coverage) UTCWindow() (UTCWindow, bool) {
	if c.window == nil {
		return UTCWindow{}, false
	}
	return *c.window, true
}

// FromRecorderSnapshot projects only a fully admitted SQL snapshot. A clipped
// gap retains its position bounds, but its whole-gap known count is withheld
// because it cannot be apportioned to the requested subrange.
func FromRecorderSnapshot(snapshot storage.RecorderCoverageSnapshot, window *UTCWindow) (Coverage, error) {
	if window != nil && (window.start.IsZero() || !window.start.Before(window.end)) {
		return Coverage{}, ErrInvalidCoverage
	}
	if snapshot.Kind() == storage.RecorderCoverageReadTruncated {
		if snapshot.Truncation() == storage.RecorderCoverageNoTruncation {
			return Coverage{}, ErrInvalidCoverage
		}
		return Coverage{verdict: Truncated, truncation: snapshot.Truncation()}, nil
	}
	if snapshot.Kind() != storage.RecorderCoverageReadComplete || snapshot.Truncation() != storage.RecorderCoverageNoTruncation {
		return Coverage{}, ErrInvalidCoverage
	}
	fromEpoch, fromSequence, throughEpoch, throughSequence := snapshot.Bounds()
	from, err := NewPosition(fromEpoch, fromSequence)
	if err != nil {
		return Coverage{}, err
	}
	through, err := NewPosition(throughEpoch, throughSequence)
	if err != nil {
		return Coverage{}, err
	}
	runs := snapshot.Epochs()
	gaps := snapshot.Gaps()
	if len(runs) == 0 || runs[0].Epoch() != fromEpoch || runs[len(runs)-1].Epoch() != throughEpoch {
		return Coverage{}, ErrInvalidCoverage
	}
	c := Coverage{verdict: CompleteUnderPolicy, from: from, through: through, scopes: make([]EpochScope, 0, len(runs)), issues: make([]Issue, 0, len(gaps)+len(runs))}
	if window != nil {
		w := *window
		c.window = &w
	}
	placement := TemporalNotRequested
	if window != nil {
		placement = TemporalUnknown
	}
	for n, run := range runs {
		start := uint64(0)
		end := run.Checkpoint()
		if n == 0 {
			start = fromSequence
		}
		if n == len(runs)-1 {
			end = throughSequence
		}
		if n == 0 && start > run.Checkpoint() || (n == len(runs)-1 && n == 0 && end < start) {
			return Coverage{}, ErrInvalidCoverage
		}
		c.scopes = append(c.scopes, EpochScope{epoch: run.Epoch(), from: start, through: end, checkpoint: run.Checkpoint(), policyExcluded: run.PolicyExcluded()})
		if run.Checkpoint() == 0 && !run.ReconciliationComplete() {
			// OpenRecorderEpoch commits before its initial image. A read in
			// that narrow window cannot claim even an empty stable baseline.
			at := Position{epoch: run.Epoch(), sequence: 0}
			c.issues = append(c.issues, Issue{kind: InitialSnapshotPending, from: at, through: at, uncertain: true, placement: placement})
			c.verdict = Incomplete
		}
		if n > 0 {
			prior := runs[n-1]
			previousEpoch, previousCheckpoint, _, ok := run.Previous()
			if !ok || previousEpoch != prior.Epoch() || previousCheckpoint != prior.Checkpoint() {
				return Coverage{}, ErrInvalidCoverage
			}
			c.issues = append(c.issues, Issue{kind: EpochBridge, from: Position{epoch: prior.Epoch(), sequence: prior.Checkpoint()}, through: Position{epoch: run.Epoch(), sequence: 0}, uncertain: true, placement: placement})
			c.verdict = Incomplete
		}
		if end > run.Checkpoint() {
			c.issues = append(c.issues, Issue{kind: UncommittedTail, from: Position{epoch: run.Epoch(), sequence: run.Checkpoint()}, through: Position{epoch: run.Epoch(), sequence: end}, uncertain: true, placement: placement})
			c.verdict = Incomplete
		}
	}
	for _, gap := range gaps {
		index := -1
		for n := range c.scopes {
			if c.scopes[n].epoch == gap.Epoch() {
				index = n
				break
			}
		}
		if index < 0 {
			return Coverage{}, ErrInvalidCoverage
		}
		scope := c.scopes[index]
		gapFrom, gapThrough := gap.Bounds()
		if gapFrom == 0 || gapThrough < gapFrom || gapThrough > scope.checkpoint {
			return Coverage{}, ErrInvalidCoverage
		}
		clipFrom := gapFrom
		if clipFrom <= scope.from {
			if scope.from == ^uint64(0) {
				continue
			}
			clipFrom = scope.from + 1
		}
		clipThrough := gapThrough
		if clipThrough > scope.through {
			clipThrough = scope.through
		}
		if clipFrom > clipThrough {
			continue
		}
		issue := Issue{kind: StoredGap, from: Position{epoch: gap.Epoch(), sequence: clipFrom}, through: Position{epoch: gap.Epoch(), sequence: clipThrough}, reason: gap.Reason(), uncertain: gap.UncertainEnvelope(), placement: placement}
		if count, ok := gap.KnownLost(); ok && clipFrom == gapFrom && clipThrough == gapThrough {
			issue.knownLost, issue.hasKnownLost = count, true
		}
		c.issues = append(c.issues, issue)
		c.verdict = Incomplete
	}
	return c, nil
}

// Both consumer models carry the same immutable coverage value. Point and
// aggregate payloads belong to NHA-164 and NHA-032 respectively.
type HistoryReadModel struct{ coverage Coverage }
type StatisticsReadModel struct{ coverage Coverage }

func NewHistoryReadModel(c Coverage) (HistoryReadModel, error) {
	if c.verdict < CompleteUnderPolicy || c.verdict > Truncated {
		return HistoryReadModel{}, ErrInvalidCoverage
	}
	return HistoryReadModel{coverage: cloneCoverage(c)}, nil
}
func NewStatisticsReadModel(c Coverage) (StatisticsReadModel, error) {
	if c.verdict < CompleteUnderPolicy || c.verdict > Truncated {
		return StatisticsReadModel{}, ErrInvalidCoverage
	}
	return StatisticsReadModel{coverage: cloneCoverage(c)}, nil
}
func (m HistoryReadModel) Coverage() Coverage    { return cloneCoverage(m.coverage) }
func (m StatisticsReadModel) Coverage() Coverage { return cloneCoverage(m.coverage) }
func cloneCoverage(c Coverage) Coverage {
	c.scopes = append([]EpochScope(nil), c.scopes...)
	c.issues = append([]Issue(nil), c.issues...)
	if c.window != nil {
		v := *c.window
		c.window = &v
	}
	return c
}
