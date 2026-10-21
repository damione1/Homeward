package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/jackc/pgx/v5"
)

type HistoryPageCursor struct {
	epoch                          HistoryEpoch
	since, until, cutoff, lastTime time.Time
	lastOrdinal                    uint64
	commitHigh                     int64
	checkpoint                     uint64
}

type HistoryPageQuery struct {
	Epoch           HistoryEpoch
	Since           time.Time // zero means the current seven-day boundary
	Until           time.Time // optional exclusive emission_time upper bound
	HoldReservation bool      // public reader owns model/wire until Release
	ClipRetention   bool      // explicit public read scope may include an absent prefix
	Cursor          *HistoryPageCursor
	Limit           int
}

type HistoryReadRecord struct {
	Record     HistoryRecord
	InsertedAt time.Time
}

type HistoryReadLoss struct {
	Reason            HistoryLossReason
	KnownCount        uint64
	From, Through     time.Time
	UncertainEnvelope bool
}

// An interrupted epoch has a known successor, but its final uncommitted
// observations are unknown. A visible run without a terminal row was not
// resumed and must not be inferred completed/cancelled from this state.
type HistoryEpochState uint8

const (
	HistoryEpochInvalid HistoryEpochState = iota
	HistoryEpochCurrent
	HistoryEpochInterrupted
)

type HistoryPage struct {
	lease                  *historyReadLease
	Epoch                  HistoryEpoch
	AsOf                   time.Time
	Records                []HistoryReadRecord
	Losses                 []HistoryReadLoss
	Next                   *HistoryPageCursor
	Cutoff                 time.Time
	CommitHigh             int64
	Checkpoint             uint64
	PreviousEpoch          HistoryEpoch
	InterruptedPredecessor bool
	ScopeReduced           bool
	EpochState             HistoryEpochState
}

func (p HistoryPage) Release() { p.lease.close() }

// ReadExecutionHistoryPage reads lines and coverage under one repeatable-read
// snapshot. Its high commit bound is held across keyset pages. A SQL purge
// crossing the cursor's retention boundary invalidates the cursor explicitly.
func (s *Store) ReadExecutionHistoryPage(ctx context.Context, q HistoryPageQuery) (HistoryPage, error) {
	if s == nil || s.pool == nil || s.historyReadSlots == nil || !validHistoryContext(ctx) || (!q.Epoch.Valid() && q.Epoch != (HistoryEpoch{})) || (!q.Until.IsZero() && !q.Since.Before(q.Until)) || q.Limit < 1 || q.Limit > 128 {
		return HistoryPage{}, ErrHistoryInvalid
	}
	if q.Cursor != nil && !q.Epoch.Valid() {
		q.Epoch = q.Cursor.epoch
	}
	if q.Cursor != nil && (q.Cursor.epoch != q.Epoch || !q.Cursor.since.Equal(q.Since) || !q.Cursor.until.Equal(q.Until) || q.Cursor.commitHigh < 0) {
		return HistoryPage{}, ErrHistoryInvalid
	}
	bounded, releaseSlot, err := s.acquireHistoryRead(ctx)
	if err != nil {
		if errors.Is(err, historyquery.ErrInvalid) {
			return HistoryPage{}, historyFailure(ErrHistoryInvalid, err)
		}
		// The shared NHA164 reservation uses its own error family. Preserve the
		// NHA186 storage reader contract and the original deadline/cancel cause.
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, err)
	}
	ctx = bounded
	retained := false
	defer func() {
		if !retained {
			releaseSlot()
		}
	}()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var dbCutoff time.Time
	var high int64
	var currentEpoch *string
	if err := tx.QueryRow(ctx, `SELECT retention_cutoff,commit_sequence,current_epoch FROM public.execution_history_head WHERE singleton=true`).Scan(&dbCutoff, &high, &currentEpoch); err != nil {
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, err)
	}
	if q.Cursor != nil && dbCutoff.After(q.Cursor.cutoff) {
		return HistoryPage{}, ErrHistoryRetentionAdvanced
	}
	if q.Cursor != nil {
		high = q.Cursor.commitHigh
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Microsecond)
	if dbCutoff.After(cutoff) {
		cutoff = dbCutoff
	}
	page := HistoryPage{Cutoff: cutoff, CommitHigh: high, AsOf: time.Now().UTC()}
	if !q.Epoch.Valid() {
		if currentEpoch == nil {
			return HistoryPage{}, ErrHistoryIntegrity
		}
		q.Epoch, err = ParseHistoryEpoch(*currentEpoch)
		if err != nil {
			return HistoryPage{}, ErrHistoryIntegrity
		}
	}
	page.Epoch = q.Epoch
	if q.Cursor != nil && cutoff.After(q.Cursor.cutoff) {
		page.ScopeReduced = true
	}
	if !q.ClipRetention && !q.Since.IsZero() && q.Since.Before(cutoff) {
		return HistoryPage{}, ErrHistoryOutsideRetention
	}
	if q.Since.After(cutoff) {
		cutoff = q.Since.UTC()
	}
	var previous *string
	var checkpoint int64
	if err := tx.QueryRow(ctx, `SELECT previous_epoch,checkpoint,interrupted_predecessor FROM public.execution_history_epochs WHERE epoch=$1`, q.Epoch.String()).Scan(&previous, &checkpoint, &page.InterruptedPredecessor); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HistoryPage{}, ErrHistoryInvalid
		}
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, err)
	}
	page.Checkpoint = uint64(checkpoint)
	if currentEpoch == nil {
		return HistoryPage{}, ErrHistoryIntegrity
	}
	current, err := ParseHistoryEpoch(*currentEpoch)
	if err != nil {
		return HistoryPage{}, ErrHistoryIntegrity
	}
	page.EpochState = HistoryEpochCurrent
	if current != q.Epoch {
		page.EpochState = HistoryEpochInterrupted
	}
	if q.Cursor != nil {
		page.Checkpoint = q.Cursor.checkpoint
	}
	if previous != nil {
		page.PreviousEpoch, err = ParseHistoryEpoch(*previous)
		if err != nil {
			return HistoryPage{}, ErrHistoryIntegrity
		}
	}
	limit := q.Limit
	// At most 31 admitted 8KiB rows leave room for coverage in the
	// 256KiB page and SQL buffers/copies in the two-reader reservation.
	if limit > 31 {
		limit = 31
	}
	var lastTime time.Time
	var lastOrdinal uint64
	if q.Cursor != nil {
		lastTime, lastOrdinal = q.Cursor.lastTime, q.Cursor.lastOrdinal
	}
	rows, err := tx.Query(ctx, `SELECT ordinal,emission_time,inserted_at,kind,decision,node_stage,command_outcome,scene_outcome,scene_aggregate,script_mode,
		native_ordinal,node_visit,command_ordinal,action_kind,failure_kind,wait_exit,
		controller_generation,run_id,parent_run_id,provenance_kind,config_revision,manifest_sha256,
		object_id,source_file,source_path,source_line,source_column,trigger_key,command_id,scene_invocation,scene_member,
		branches,iterations,record_digest
		FROM public.execution_history_records WHERE epoch=$1 AND emission_time >= $2 AND commit_sequence <= $3
		AND ($7::timestamptz IS NULL OR emission_time < $7)
 AND ($4::timestamptz IS NULL OR (emission_time,ordinal) > ($4,$5))
		ORDER BY emission_time,ordinal LIMIT $6`, q.Epoch.String(), historyCeilMicro(cutoff), high, historyNullableTime(lastTime), int64(lastOrdinal), limit+1, historyNullableTime(historyCeilMicro(q.Until)))
	if err != nil {
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, err)
	}
	page.Records = make([]HistoryReadRecord, 0, limit)
	more := false
	for rows.Next() {
		if len(page.Records) == limit {
			more = true
			break
		}
		value, e := scanHistoryRecord(rows, q.Epoch)
		if e != nil {
			rows.Close()
			return HistoryPage{}, e
		}
		page.Records = append(page.Records, value)
	}
	if e := rows.Err(); e != nil {
		rows.Close()
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, e)
	}
	rows.Close()
	if more && len(page.Records) > 0 {
		last := page.Records[len(page.Records)-1].Record.input
		page.Next = &HistoryPageCursor{epoch: q.Epoch, since: q.Since, until: q.Until, cutoff: dbCutoff, lastTime: last.EmissionTime, lastOrdinal: last.ID.Ordinal(), commitHigh: high, checkpoint: page.Checkpoint}
	}
	if e := readHistoryLossUntil(ctx, tx, q.Epoch, cutoff, q.Until, high, &page); e != nil {
		return HistoryPage{}, e
	}
	if e := tx.Commit(ctx); e != nil {
		return HistoryPage{}, historyFailure(ErrHistoryUnavailable, e)
	}
	if q.HoldReservation {
		page.lease = &historyReadLease{release: releaseSlot}
		retained = true
	}
	return page, nil
}

func historyNullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func scanHistoryRecord(rows pgx.Rows, epoch HistoryEpoch) (HistoryReadRecord, error) {
	var ordinal, nativeOrdinal, nodeVisit, commandOrdinal, generation, runID, parentID, revision *int64
	var emission, inserted time.Time
	var kind, provenance int16
	var decision, stage, command, scene, aggregate, mode *int16
	var actionKind, failureKind, waitExit *int16
	var manifest, digest []byte
	var object, file, sourcePath, trigger, commandID, sceneInvocation string
	var line, column, member *int32
	var branches, iterations []int32
	if err := rows.Scan(&ordinal, &emission, &inserted, &kind, &decision, &stage, &command, &scene, &aggregate, &mode,
		&nativeOrdinal, &nodeVisit, &commandOrdinal, &actionKind, &failureKind, &waitExit,
		&generation, &runID, &parentID, &provenance, &revision, &manifest,
		&object, &file, &sourcePath, &line, &column, &trigger, &commandID, &sceneInvocation, &member,
		&branches, &iterations, &digest); err != nil {
		return HistoryReadRecord{}, historyFailure(ErrHistoryIntegrity, err)
	}
	if ordinal == nil || *ordinal <= 0 || len(digest) != 32 || (manifest != nil && len(manifest) != 32) {
		return HistoryReadRecord{}, ErrHistoryIntegrity
	}
	// SQL smallint is wider than the Go enum. Admit the SQL variants before
	// narrowing, so 257/-255 cannot wrap to a valid variant and reuse its digest.
	switch kind {
	case int16(HistoryDecision), int16(HistoryNode), int16(HistoryCommand), int16(HistorySceneMember), int16(HistorySceneInvocation), int16(HistoryScriptLink):
	default:
		return HistoryReadRecord{}, ErrHistoryIntegrity
	}
	switch provenance {
	case int16(HistoryUnboundFixture), int16(HistoryBoundProgram):
	default:
		return HistoryReadRecord{}, ErrHistoryIntegrity
	}
	id, err := NewHistoryTraceID(epoch, uint64(*ordinal))
	if err != nil {
		return HistoryReadRecord{}, ErrHistoryIntegrity
	}
	in := HistoryRecordInput{ID: id, EmissionTime: emission, Kind: HistoryRecordKind(kind), Provenance: HistoryProvenanceKind(provenance),
		ObjectID: object, SourceFile: file, SourcePath: sourcePath, TriggerKey: trigger, CommandID: commandID, SceneInvocation: sceneInvocation,
		Branches: branches, Iterations: iterations}
	for _, value := range []*int16{decision, stage, command, scene, aggregate, mode, actionKind, failureKind, waitExit} {
		if value != nil && (*value <= 0 || *value > 255) {
			return HistoryReadRecord{}, ErrHistoryIntegrity
		}
	}
	// SQL NULL is the only absence for positive optional metadata. Admit
	// presence before copying into Go's zero-as-absence representation.
	for _, value := range []*int64{nativeOrdinal, nodeVisit, commandOrdinal, generation, runID, parentID, revision} {
		if value != nil && *value <= 0 {
			return HistoryReadRecord{}, ErrHistoryIntegrity
		}
	}
	for _, value := range []*int32{line, column} {
		if value != nil && *value <= 0 {
			return HistoryReadRecord{}, ErrHistoryIntegrity
		}
	}
	// Member zero is a real scene member, not an absent value. NULL must
	// remain absent for every other record kind and must not become member0.
	if (in.Kind == HistorySceneMember) != (member != nil) || (member != nil && *member < 0) {
		return HistoryReadRecord{}, ErrHistoryIntegrity
	}
	if decision != nil {
		in.Decision = uint8(*decision)
	}
	if stage != nil {
		in.NodeStage = uint8(*stage)
	}
	if command != nil {
		in.CommandOutcome = uint8(*command)
	}
	if scene != nil {
		in.SceneOutcome = uint8(*scene)
	}
	if aggregate != nil {
		in.SceneAggregate = uint8(*aggregate)
	}
	if mode != nil {
		in.ScriptMode = uint8(*mode)
	}
	if actionKind != nil {
		in.ActionKind = uint8(*actionKind)
	}
	if failureKind != nil {
		in.FailureKind = uint8(*failureKind)
	}
	if waitExit != nil {
		in.WaitExit = uint8(*waitExit)
	}
	if nativeOrdinal != nil {
		in.NativeOrdinal = uint64(*nativeOrdinal)
	}
	if nodeVisit != nil {
		in.NodeVisit = uint64(*nodeVisit)
	}
	if commandOrdinal != nil {
		in.CommandOrdinal = uint64(*commandOrdinal)
	}
	if generation != nil {
		in.ControllerGeneration = uint64(*generation)
	}
	if runID != nil {
		in.RunID = uint64(*runID)
	}
	if parentID != nil {
		in.ParentRunID = uint64(*parentID)
	}
	if revision != nil {
		in.ConfigRevision = *revision
	}
	if manifest != nil {
		copy(in.ManifestSHA256[:], manifest)
	}
	if line != nil {
		in.SourceLine = int(*line)
	}
	if column != nil {
		in.SourceColumn = int(*column)
	}
	if member != nil {
		in.SceneMember = int(*member)
	}
	record, err := NewHistoryRecord(in)
	if err != nil || !bytes.Equal(digest, record.digest[:]) {
		return HistoryReadRecord{}, ErrHistoryIntegrity
	}
	return HistoryReadRecord{Record: record, InsertedAt: inserted.UTC()}, nil
}

func readHistoryLoss(ctx context.Context, tx pgx.Tx, epoch HistoryEpoch, cutoff time.Time, high int64, page *HistoryPage) error {
	return readHistoryLossUntil(ctx, tx, epoch, cutoff, time.Time{}, high, page)
}
func readHistoryLossUntil(ctx context.Context, tx pgx.Tx, epoch HistoryEpoch, cutoff, until time.Time, high int64, page *HistoryPage) error {
	rows, err := tx.Query(ctx, `SELECT reason,SUM(known_count)::text,bool_or(uncertain_envelope OR start_time < $2 OR ($4::timestamptz IS NOT NULL AND emission_time >= $4)),MIN(start_time),MAX(emission_time)
		FROM public.execution_history_loss WHERE epoch=$1 AND emission_time >= $2 AND commit_sequence <= $3 AND ($4::timestamptz IS NULL OR start_time < $4)
		GROUP BY reason ORDER BY reason`, epoch.String(), historyCeilMicro(cutoff), high, historyNullableTime(historyCeilMicro(until)))
	if err != nil {
		return historyFailure(ErrHistoryUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason int16
		var countText string
		var loss HistoryReadLoss
		if err := rows.Scan(&reason, &countText, &loss.UncertainEnvelope, &loss.From, &loss.Through); err != nil {
			return historyFailure(ErrHistoryIntegrity, err)
		}
		count, err := strconv.ParseUint(countText, 10, 64)
		if err != nil || count == 0 || reason < 1 || reason > 4 {
			return ErrHistoryIntegrity
		}
		loss.Reason, loss.KnownCount = HistoryLossReason(reason), count
		page.Losses = append(page.Losses, loss)
	}
	if err := rows.Err(); err != nil {
		return historyFailure(ErrHistoryUnavailable, err)
	}
	return nil
}

// MarshalBinary exports only an admitted position for an encrypted, bound token.
func (c *HistoryPageCursor) MarshalBinary() ([]byte, error) {
	if c == nil || !c.epoch.Valid() || c.commitHigh < 0 || c.lastOrdinal == 0 || c.lastOrdinal > uint64(9223372036854775807) || c.lastTime.IsZero() {
		return nil, ErrHistoryInvalid
	}
	out := make([]byte, 16, 100)
	copy(out, c.epoch.bytes[:])
	for _, at := range []time.Time{c.since, c.until, c.cutoff, c.lastTime} {
		raw, err := at.UTC().MarshalBinary()
		if err != nil || len(raw) != 15 {
			return nil, ErrHistoryInvalid
		}
		out = append(out, raw...)
	}
	var n [8]byte
	for _, v := range []uint64{c.lastOrdinal, uint64(c.commitHigh), c.checkpoint} {
		binary.BigEndian.PutUint64(n[:], v)
		out = append(out, n[:]...)
	}
	return out, nil
}
func ParseHistoryPageCursor(raw []byte) (*HistoryPageCursor, error) {
	if len(raw) != 100 {
		return nil, ErrHistoryInvalid
	}
	c := &HistoryPageCursor{}
	copy(c.epoch.bytes[:], raw[:16])
	for i, at := range []*time.Time{&c.since, &c.until, &c.cutoff, &c.lastTime} {
		if err := at.UnmarshalBinary(raw[16+i*15 : 31+i*15]); err != nil {
			return nil, ErrHistoryInvalid
		}
		*at = at.UTC()
	}
	c.lastOrdinal = binary.BigEndian.Uint64(raw[76:])
	c.commitHigh = int64(binary.BigEndian.Uint64(raw[84:]))
	c.checkpoint = binary.BigEndian.Uint64(raw[92:])
	if _, err := c.MarshalBinary(); err != nil {
		return nil, err
	}
	return c, nil
}

// Trace emission times and loss envelopes are stored on the microsecond
// lattice. Both inclusive lower and exclusive upper comparisons need ceil.
func historyCeilMicro(at time.Time) time.Time {
	if at.IsZero() {
		return at
	}
	floor := at.Truncate(time.Microsecond)
	if at.After(floor) {
		return floor.Add(time.Microsecond)
	}
	return floor
}
