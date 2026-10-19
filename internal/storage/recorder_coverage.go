package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	RecorderCoverageMaxEpochs = 64
	RecorderCoverageMaxGaps   = 512
	RecorderCoverageMaxBytes  = 256 << 10
	RecorderCoverageTimeout   = 2 * time.Second
)

var (
	ErrRecorderCoverageInvalid     = errors.New("recorder coverage: invalid request")
	ErrRecorderCoverageUnavailable = errors.New("recorder coverage: storage unavailable")
	ErrRecorderCoverageIntegrity   = errors.New("recorder coverage: invalid stored metadata")
	ErrRecorderCoverageNotFound    = errors.New("recorder coverage: epoch not found")
)

type RecorderCoverageReadKind uint8

const (
	RecorderCoverageReadUnspecified RecorderCoverageReadKind = iota
	RecorderCoverageReadComplete
	RecorderCoverageReadTruncated
)

type RecorderCoverageTruncation uint8

const (
	RecorderCoverageNoTruncation RecorderCoverageTruncation = iota
	RecorderCoverageEpochLimit
	RecorderCoverageGapLimit
	RecorderCoverageByteLimit
	RecorderCoverageDurationLimit
)

// RecorderCoverageRequest names a logical interval (from, through]. An absent
// through sequence resolves to the end epoch's checkpoint in the same snapshot.
type RecorderCoverageRequest struct {
	fromEpoch, throughEpoch RecorderEpoch
	from, through           uint64
	hasThrough              bool
}

func NewRecorderCoverageRequest(fromEpoch RecorderEpoch, from uint64, throughEpoch RecorderEpoch, through *uint64) (RecorderCoverageRequest, error) {
	if !fromEpoch.valid() || !throughEpoch.valid() {
		return RecorderCoverageRequest{}, ErrRecorderCoverageInvalid
	}
	r := RecorderCoverageRequest{fromEpoch: fromEpoch, from: from, throughEpoch: throughEpoch}
	if through != nil {
		r.through, r.hasThrough = *through, true
		if fromEpoch == throughEpoch && r.through < from {
			return RecorderCoverageRequest{}, ErrRecorderCoverageInvalid
		}
	}
	return r, nil
}

func (r RecorderCoverageRequest) From() (RecorderEpoch, uint64) { return r.fromEpoch, r.from }
func (r RecorderCoverageRequest) End() (RecorderEpoch, uint64, bool) {
	return r.throughEpoch, r.through, r.hasThrough
}

type RecorderCoverageEpoch struct {
	epoch                      RecorderEpoch
	runID                      int64
	started                    time.Time
	closed                     *time.Time
	cleanClose                 bool
	previous                   *RecorderEpoch
	previousCheckpoint         *uint64
	previousCleanClose         *bool
	checkpoint, policyExcluded uint64
	reconciliationComplete     bool
}

func (e RecorderCoverageEpoch) Epoch() RecorderEpoch         { return e.epoch }
func (e RecorderCoverageEpoch) Checkpoint() uint64           { return e.checkpoint }
func (e RecorderCoverageEpoch) PolicyExcluded() uint64       { return e.policyExcluded }
func (e RecorderCoverageEpoch) ReconciliationComplete() bool { return e.reconciliationComplete }
func (e RecorderCoverageEpoch) CleanClose() bool             { return e.cleanClose }
func (e RecorderCoverageEpoch) StartedAt() time.Time         { return e.started }
func (e RecorderCoverageEpoch) ClosedAt() (time.Time, bool) {
	if e.closed == nil {
		return time.Time{}, false
	}
	return *e.closed, true
}
func (e RecorderCoverageEpoch) Previous() (RecorderEpoch, uint64, bool, bool) {
	if e.previous == nil {
		return RecorderEpoch{}, 0, false, false
	}
	return *e.previous, *e.previousCheckpoint, *e.previousCleanClose, true
}

type RecorderCoverageGap struct {
	epoch         RecorderEpoch
	from, through uint64
	reason        RecorderGapReason
	knownLost     *uint64
	uncertain     bool
}

func (g RecorderCoverageGap) Epoch() RecorderEpoch      { return g.epoch }
func (g RecorderCoverageGap) Bounds() (uint64, uint64)  { return g.from, g.through }
func (g RecorderCoverageGap) Reason() RecorderGapReason { return g.reason }
func (g RecorderCoverageGap) KnownLost() (uint64, bool) {
	if g.knownLost == nil {
		return 0, false
	}
	return *g.knownLost, true
}
func (g RecorderCoverageGap) UncertainEnvelope() bool { return g.uncertain }

type RecorderCoverageSnapshot struct {
	kind                    RecorderCoverageReadKind
	truncation              RecorderCoverageTruncation
	fromEpoch, throughEpoch RecorderEpoch
	from, through           uint64
	epochs                  []RecorderCoverageEpoch
	gaps                    []RecorderCoverageGap
}

func (s RecorderCoverageSnapshot) Kind() RecorderCoverageReadKind         { return s.kind }
func (s RecorderCoverageSnapshot) Truncation() RecorderCoverageTruncation { return s.truncation }
func (s RecorderCoverageSnapshot) Bounds() (RecorderEpoch, uint64, RecorderEpoch, uint64) {
	return s.fromEpoch, s.from, s.throughEpoch, s.through
}
func (s RecorderCoverageSnapshot) Epochs() []RecorderCoverageEpoch {
	out := make([]RecorderCoverageEpoch, len(s.epochs))
	for i, e := range s.epochs {
		out[i] = cloneCoverageEpoch(e)
	}
	return out
}
func (s RecorderCoverageSnapshot) Gaps() []RecorderCoverageGap {
	out := make([]RecorderCoverageGap, len(s.gaps))
	for i, g := range s.gaps {
		out[i] = cloneCoverageGap(g)
	}
	return out
}

func cloneCoverageEpoch(e RecorderCoverageEpoch) RecorderCoverageEpoch {
	if e.closed != nil {
		v := *e.closed
		e.closed = &v
	}
	if e.previous != nil {
		v := *e.previous
		e.previous = &v
	}
	if e.previousCheckpoint != nil {
		v := *e.previousCheckpoint
		e.previousCheckpoint = &v
	}
	if e.previousCleanClose != nil {
		v := *e.previousCleanClose
		e.previousCleanClose = &v
	}
	return e
}
func cloneCoverageGap(g RecorderCoverageGap) RecorderCoverageGap {
	if g.knownLost != nil {
		v := *g.knownLost
		g.knownLost = &v
	}
	return g
}

func coverageTruncated(reason RecorderCoverageTruncation) RecorderCoverageSnapshot {
	return RecorderCoverageSnapshot{kind: RecorderCoverageReadTruncated, truncation: reason}
}
func coverageReadError(ctx, bounded context.Context, err error) (RecorderCoverageSnapshot, error) {
	if cause := ctx.Err(); cause != nil {
		return RecorderCoverageSnapshot{}, cause
	}
	if errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return coverageTruncated(RecorderCoverageDurationLimit), nil
	}
	return RecorderCoverageSnapshot{}, fmt.Errorf("%w: %v", ErrRecorderCoverageUnavailable, err)
}
func coverageUint(text string) (uint64, error) {
	v, err := strconv.ParseUint(text, 10, 64)
	if err != nil || strconv.FormatUint(v, 10) != text {
		return 0, ErrRecorderCoverageIntegrity
	}
	return v, nil
}
func coverageEpoch(data []byte) (RecorderEpoch, error) {
	if len(data) != 16 {
		return RecorderEpoch{}, ErrRecorderCoverageIntegrity
	}
	var e RecorderEpoch
	copy(e.bytes[:], data)
	if !e.valid() {
		return RecorderEpoch{}, ErrRecorderCoverageIntegrity
	}
	return e, nil
}

type recorderCoverageLimits struct {
	epochs, gaps, bytes int
	timeout             time.Duration
}

var recorderCoverageProductionLimits = recorderCoverageLimits{
	epochs:  RecorderCoverageMaxEpochs,
	gaps:    RecorderCoverageMaxGaps,
	bytes:   RecorderCoverageMaxBytes,
	timeout: RecorderCoverageTimeout,
}

// ReadRecorderCoverage admits only a complete, bounded PostgreSQL snapshot.
// Truncation has no partial epochs or gaps that a caller could mistake for proof.
func (s *Store) ReadRecorderCoverage(ctx context.Context, request RecorderCoverageRequest) (RecorderCoverageSnapshot, error) {
	return s.readRecorderCoverageWithLimits(ctx, request, recorderCoverageProductionLimits)
}

func (s *Store) readRecorderCoverageWithLimits(ctx context.Context, request RecorderCoverageRequest, limits recorderCoverageLimits) (RecorderCoverageSnapshot, error) {
	if err := accountContextError(ctx); err != nil {
		return RecorderCoverageSnapshot{}, err
	}
	if s == nil || s.pool == nil || !request.fromEpoch.valid() || !request.throughEpoch.valid() ||
		(request.hasThrough && request.fromEpoch == request.throughEpoch && request.through < request.from) ||
		limits.epochs < 1 || limits.epochs > RecorderCoverageMaxEpochs || limits.gaps < 1 || limits.gaps > RecorderCoverageMaxGaps ||
		limits.bytes < 1 || limits.bytes > RecorderCoverageMaxBytes || limits.timeout <= 0 || limits.timeout > RecorderCoverageTimeout {
		return RecorderCoverageSnapshot{}, ErrRecorderCoverageInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, limits.timeout)
	defer cancel()
	tx, err := s.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return coverageReadError(ctx, bounded, err)
	}
	// Rollback shares the reader's deadline; cleanup must not add another 2s
	// after a timed-out coverage request.
	defer func() { _ = tx.Rollback(bounded) }()
	var firstID, lastID int64
	var firstText, lastText string
	if err = tx.QueryRow(bounded, `SELECT run_id,checkpoint::text FROM public.recorder_epochs WHERE epoch=$1`, request.fromEpoch.data()).Scan(&firstID, &firstText); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecorderCoverageSnapshot{}, ErrRecorderCoverageNotFound
		}
		return coverageReadError(ctx, bounded, err)
	}
	if request.fromEpoch == request.throughEpoch {
		lastID, lastText = firstID, firstText
	} else {
		if err = tx.QueryRow(bounded, `SELECT run_id,checkpoint::text FROM public.recorder_epochs WHERE epoch=$1`, request.throughEpoch.data()).Scan(&lastID, &lastText); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return RecorderCoverageSnapshot{}, ErrRecorderCoverageNotFound
			}
			return coverageReadError(ctx, bounded, err)
		}
	}
	firstCheckpoint, err := coverageUint(firstText)
	if err != nil {
		return RecorderCoverageSnapshot{}, err
	}
	lastCheckpoint, err := coverageUint(lastText)
	if err != nil {
		return RecorderCoverageSnapshot{}, err
	}
	if firstID < 1 || lastID < firstID || request.from > firstCheckpoint {
		return RecorderCoverageSnapshot{}, ErrRecorderCoverageInvalid
	}
	through := lastCheckpoint
	if request.hasThrough {
		through = request.through
	}
	if firstID == lastID && through < request.from {
		return RecorderCoverageSnapshot{}, ErrRecorderCoverageInvalid
	}
	result := RecorderCoverageSnapshot{kind: RecorderCoverageReadComplete, fromEpoch: request.fromEpoch, from: request.from, throughEpoch: request.throughEpoch, through: through}
	remaining := limits.bytes
	rows, err := tx.Query(bounded, `SELECT epoch,run_id,started_at,closed_at,clean_close,previous_epoch,previous_checkpoint::text,previous_clean_close,checkpoint::text,policy_excluded::text,reconciliation_complete FROM public.recorder_epochs WHERE run_id BETWEEN $1 AND $2 ORDER BY run_id LIMIT $3`, firstID, lastID, limits.epochs+1)
	if err != nil {
		return coverageReadError(ctx, bounded, err)
	}
	for rows.Next() {
		if len(result.epochs) == limits.epochs {
			rows.Close()
			return coverageTruncated(RecorderCoverageEpochLimit), nil
		}
		var raw, prev []byte
		var e RecorderCoverageEpoch
		var previousText *string
		var checkpointText, excludedText string
		if err = rows.Scan(&raw, &e.runID, &e.started, &e.closed, &e.cleanClose, &prev, &previousText, &e.previousCleanClose, &checkpointText, &excludedText, &e.reconciliationComplete); err != nil {
			rows.Close()
			return coverageReadError(ctx, bounded, err)
		}
		charge := 256 + len(raw) + len(prev) + len(checkpointText) + len(excludedText)
		if previousText != nil {
			charge += len(*previousText)
		}
		if charge > remaining {
			rows.Close()
			return coverageTruncated(RecorderCoverageByteLimit), nil
		}
		remaining -= charge
		e.epoch, err = coverageEpoch(raw)
		if err != nil {
			rows.Close()
			return RecorderCoverageSnapshot{}, err
		}
		e.checkpoint, err = coverageUint(checkpointText)
		if err != nil {
			rows.Close()
			return RecorderCoverageSnapshot{}, err
		}
		e.policyExcluded, err = coverageUint(excludedText)
		if err != nil {
			rows.Close()
			return RecorderCoverageSnapshot{}, err
		}
		if len(prev) != 0 {
			v, parseErr := coverageEpoch(prev)
			if parseErr != nil {
				rows.Close()
				return RecorderCoverageSnapshot{}, parseErr
			}
			e.previous = &v
		}
		if previousText != nil {
			v, parseErr := coverageUint(*previousText)
			if parseErr != nil {
				rows.Close()
				return RecorderCoverageSnapshot{}, parseErr
			}
			e.previousCheckpoint = &v
		}
		if e.runID < firstID || e.runID > lastID || e.started.IsZero() || (e.closed != nil && e.closed.Before(e.started)) ||
			(e.previous == nil) != (e.previousCheckpoint == nil) || (e.previous == nil) != (e.previousCleanClose == nil) ||
			(len(result.epochs) == 0 && e.epoch != request.fromEpoch) {
			rows.Close()
			return RecorderCoverageSnapshot{}, ErrRecorderCoverageIntegrity
		}
		if n := len(result.epochs); n > 0 {
			prior := result.epochs[n-1]
			// PostgreSQL identity values can skip on rolled-back inserts. The
			// durable previous link, not run_id arithmetic, proves adjacency.
			if e.runID <= prior.runID || e.previous == nil || *e.previous != prior.epoch || *e.previousCheckpoint != prior.checkpoint || *e.previousCleanClose != prior.cleanClose {
				rows.Close()
				return RecorderCoverageSnapshot{}, ErrRecorderCoverageIntegrity
			}
		}
		if len(result.epochs) == 0 && e.checkpoint != firstCheckpoint {
			rows.Close()
			return RecorderCoverageSnapshot{}, ErrRecorderCoverageIntegrity
		}
		result.epochs = append(result.epochs, cloneCoverageEpoch(e))
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return coverageReadError(ctx, bounded, err)
	}
	rows.Close()
	if len(result.epochs) == 0 || result.epochs[len(result.epochs)-1].epoch != request.throughEpoch || result.epochs[len(result.epochs)-1].checkpoint != lastCheckpoint {
		return RecorderCoverageSnapshot{}, ErrRecorderCoverageIntegrity
	}
	rows, err = tx.Query(bounded, `SELECT e.run_id,g.epoch,g.from_sequence::text,g.through_sequence::text,g.reason,g.known_lost_count::text,g.uncertain_envelope FROM public.history_gaps AS g JOIN public.recorder_epochs AS e ON e.epoch=g.epoch WHERE e.run_id BETWEEN $1 AND $2 AND (e.run_id<>$1 OR g.through_sequence>$3::numeric) AND (e.run_id<>$2 OR g.from_sequence<=$4::numeric) ORDER BY e.run_id,g.from_sequence,g.through_sequence,g.reason LIMIT $5`, firstID, lastID, decimalUint(request.from), decimalUint(through), limits.gaps+1)
	if err != nil {
		return coverageReadError(ctx, bounded, err)
	}
	index := 0
	var priorRun int64
	var priorThrough uint64
	for rows.Next() {
		if len(result.gaps) == limits.gaps {
			rows.Close()
			return coverageTruncated(RecorderCoverageGapLimit), nil
		}
		var runID int64
		var raw []byte
		var fromText, throughText string
		var knownText *string
		var reason int16
		var g RecorderCoverageGap
		if err = rows.Scan(&runID, &raw, &fromText, &throughText, &reason, &knownText, &g.uncertain); err != nil {
			rows.Close()
			return coverageReadError(ctx, bounded, err)
		}
		charge := 128 + len(raw) + len(fromText) + len(throughText)
		if knownText != nil {
			charge += len(*knownText)
		}
		if charge > remaining {
			rows.Close()
			return coverageTruncated(RecorderCoverageByteLimit), nil
		}
		remaining -= charge
		g.epoch, err = coverageEpoch(raw)
		if err != nil {
			rows.Close()
			return RecorderCoverageSnapshot{}, err
		}
		g.from, err = coverageUint(fromText)
		if err != nil {
			rows.Close()
			return RecorderCoverageSnapshot{}, err
		}
		g.through, err = coverageUint(throughText)
		if err != nil {
			rows.Close()
			return RecorderCoverageSnapshot{}, err
		}
		g.reason = RecorderGapReason(reason)
		if knownText != nil {
			v, parseErr := coverageUint(*knownText)
			if parseErr != nil {
				rows.Close()
				return RecorderCoverageSnapshot{}, parseErr
			}
			g.knownLost = &v
		}
		for index < len(result.epochs) && result.epochs[index].runID < runID {
			index++
		}
		if index == len(result.epochs) || result.epochs[index].runID != runID || result.epochs[index].epoch != g.epoch || g.from == 0 || g.through < g.from || g.through > result.epochs[index].checkpoint || g.reason < RecorderGapOverflow || g.reason > RecorderGapAbandoned ||
			(g.knownLost != nil && (*g.knownLost == 0 || *g.knownLost > g.through-g.from+1)) ||
			(!g.uncertain && (g.knownLost == nil || *g.knownLost != g.through-g.from+1)) ||
			(runID == priorRun && g.from <= priorThrough) {
			rows.Close()
			return RecorderCoverageSnapshot{}, ErrRecorderCoverageIntegrity
		}
		priorRun, priorThrough = runID, g.through
		result.gaps = append(result.gaps, cloneCoverageGap(g))
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return coverageReadError(ctx, bounded, err)
	}
	rows.Close()
	if err = tx.Commit(bounded); err != nil {
		return coverageReadError(ctx, bounded, err)
	}
	return result, nil
}
