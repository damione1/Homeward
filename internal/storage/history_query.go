package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/damione1/Homeward/contracts"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// RecorderQueryCursor is a bounded opaque storage position. It is not an
// authorization credential; the server encrypts it with query/policy/catalogue
// bindings. No transaction or page cache survives a request.
type RecorderQueryCursor struct {
	kind                         historyquery.Kind
	highEpoch                    RecorderEpoch
	highRun                      int64
	highSequence                 uint64
	afterRun                     int64
	afterSequence                uint64
	rawCutoff, asOf, afterBucket time.Time
	revision                     int64
	group                        [32]byte
	liveSequence                 uint64
	highPolicyExcluded           uint64
}

func (c *RecorderQueryCursor) MarshalBinary() ([]byte, error) {
	if c == nil || !c.highEpoch.valid() || c.highRun < 1 || c.revision < 0 || c.afterRun < 0 || c.afterRun > c.highRun || c.asOf.IsZero() || c.liveSequence < c.highSequence || c.highPolicyExcluded > c.highSequence || (c.kind != historyquery.Observations && c.kind != historyquery.Aggregates) {
		return nil, historyquery.ErrStale
	}
	out := make([]byte, 1+16+9*8+32+8)
	out[0] = byte(c.kind)
	copy(out[1:17], c.highEpoch.data())
	nums := []uint64{uint64(c.highRun), c.highSequence, uint64(c.afterRun), c.afterSequence, uint64(c.rawCutoff.UnixMicro()), uint64(c.asOf.UnixMicro()), uint64(c.afterBucket.UnixMicro()), uint64(c.revision), c.liveSequence}
	for i, n := range nums {
		binary.BigEndian.PutUint64(out[17+i*8:], n)
	}
	copy(out[89:121], c.group[:])
	binary.BigEndian.PutUint64(out[121:], c.highPolicyExcluded)
	return out, nil
}
func ParseRecorderQueryCursor(raw []byte) (*RecorderQueryCursor, error) {
	if len(raw) != 129 {
		return nil, historyquery.ErrStale
	}
	c := &RecorderQueryCursor{kind: historyquery.Kind(raw[0])}
	copy(c.highEpoch.bytes[:], raw[1:17])
	n := func(i int) uint64 { return binary.BigEndian.Uint64(raw[17+i*8:]) }
	c.highRun = int64(n(0))
	c.highSequence = n(1)
	c.afterRun = int64(n(2))
	c.afterSequence = n(3)
	c.rawCutoff = time.UnixMicro(int64(n(4))).UTC()
	c.asOf = time.UnixMicro(int64(n(5))).UTC()
	c.afterBucket = time.UnixMicro(int64(n(6))).UTC()
	c.revision = int64(n(7))
	copy(c.group[:], raw[89:121])
	c.highPolicyExcluded = binary.BigEndian.Uint64(raw[121:])
	c.liveSequence = n(8)
	if c.liveSequence < c.highSequence {
		return nil, historyquery.ErrStale
	}
	if _, err := c.MarshalBinary(); err != nil {
		return nil, err
	}
	return c, nil
}

type HistoricalObservation struct {
	Epoch      RecorderEpoch
	Sequence   uint64
	State      *state.ObservedState
	Descriptor *recordermaintenance.NumericInput
}
type historyReadLease struct {
	once    sync.Once
	release func()
}

func (l *historyReadLease) close() {
	if l != nil {
		l.once.Do(l.release)
	}
}

type RecorderQueryPage struct {
	lease                            *historyReadLease
	Observations                     []HistoricalObservation
	Aggregates                       []RecorderHourlyAggregate
	Coverage                         RecorderCoverageSnapshot
	Next                             *RecorderQueryCursor
	RawCutoff, AggregateCutoff, AsOf time.Time
	AnalyticRun                      int64
	AnalyticSequence                 uint64
	NotRetained                      bool
	AnalyticalTailUnproven           bool
}

func (p RecorderQueryPage) Release() { p.lease.close() }

type historyReservationKey struct{}
type historyReservation struct {
	store    *Store
	released atomic.Bool
}

// WithHistoryReadReservation owns one request through Connect decoding,
// catalogue capture, SQL, conversion and encoding. Readers borrowing this
// private Store-bound context must not acquire a second slot.
func (s *Store) WithHistoryReadReservation(ctx context.Context, read func(context.Context) error) error {
	if read == nil {
		return historyquery.ErrInvalid
	}
	bounded, release, err := s.acquireHistoryRead(ctx)
	if err != nil {
		return err
	}
	owner := &historyReservation{store: s}
	defer func() { owner.released.Store(true); release() }()
	return read(context.WithValue(bounded, historyReservationKey{}, owner))
}

// acquireHistoryRead is shared by raw, aggregate and persisted trace reads.
// The reservation is acquired once, before SQL, and includes queueing in 2s.
func (s *Store) acquireHistoryRead(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.pool == nil || s.historyReadSlots == nil || !validHistoryContext(ctx) {
		return nil, nil, historyquery.ErrUnavailable
	}
	if owner, ok := ctx.Value(historyReservationKey{}).(*historyReservation); ok {
		if owner == nil || owner.store != s || owner.released.Load() {
			return nil, nil, historyquery.ErrInvalid
		}
		if cause := ctx.Err(); cause != nil {
			return nil, nil, historyFailure(historyquery.ErrUnavailable, cause)
		}
		return ctx, func() {}, nil
	}
	bounded, cancel := context.WithTimeout(ctx, historyquery.QueryTimeout)
	select {
	case s.historyReadSlots <- struct{}{}:
		return bounded, func() { <-s.historyReadSlots; cancel() }, nil
	case <-bounded.Done():
		err := bounded.Err()
		cancel()
		return nil, nil, historyFailure(historyquery.ErrUnavailable, err)
	}
}

// RecorderLiveHigh comes from the trusted sequencer capture, never wire input.
type RecorderLiveHigh struct {
	epoch    RecorderEpoch
	sequence uint64
}

func NewRecorderLiveHigh(epoch string, sequence uint64) (RecorderLiveHigh, error) {
	admitted, err := ParseRecorderEpoch(epoch)
	if err != nil {
		return RecorderLiveHigh{}, historyquery.ErrInvalid
	}
	return RecorderLiveHigh{epoch: admitted, sequence: sequence}, nil
}
func (s *Store) ReadHistoricalRecorderPage(ctx context.Context, q historyquery.Query, cursor *RecorderQueryCursor) (RecorderQueryPage, error) {
	return s.ReadHistoricalRecorderPageAt(ctx, q, cursor, nil)
}
func (s *Store) ReadHistoricalRecorderPageAt(ctx context.Context, q historyquery.Query, cursor *RecorderQueryCursor, live *RecorderLiveHigh) (RecorderQueryPage, error) {
	if !q.Valid() || (q.Kind() != historyquery.Observations && q.Kind() != historyquery.Aggregates) || (cursor != nil && cursor.kind != q.Kind()) {
		return RecorderQueryPage{}, historyquery.ErrInvalid
	}
	bounded, release, err := s.acquireHistoryRead(ctx)
	if err != nil {
		return RecorderQueryPage{}, err
	}
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	tx, err := s.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return RecorderQueryPage{}, historyFailure(historyquery.ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(bounded) }()
	page, next, err := readHistoricalHead(bounded, tx, q, cursor, live)
	if err != nil {
		return RecorderQueryPage{}, err
	}
	if !page.NotRetained {
		if q.Kind() == historyquery.Observations {
			err = readHistoricalObservations(bounded, tx, q, next, &page)
		} else {
			err = readHistoricalAggregates(bounded, tx, q, next, &page)
		}
		if err == nil {
			page.Coverage, err = readHistoricalCoverage(bounded, tx, next)
		}
	}
	if err != nil {
		return RecorderQueryPage{}, err
	}
	if err = tx.Commit(bounded); err != nil {
		return RecorderQueryPage{}, historyFailure(historyquery.ErrUnavailable, err)
	}
	page.lease = &historyReadLease{release: release}
	retained = true
	return page, nil
}
func readHistoricalHead(ctx context.Context, tx pgx.Tx, q historyquery.Query, cursor *RecorderQueryCursor, live *RecorderLiveHigh) (RecorderQueryPage, *RecorderQueryCursor, error) {
	var page RecorderQueryPage
	var rawCutoff time.Time
	var revision, analyticRun int64
	var analyticText string
	if err := tx.QueryRow(ctx, `SELECT raw_cutoff,revision,analytic_run,analytic_sequence::text,transaction_timestamp() FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&rawCutoff, &revision, &analyticRun, &analyticText, &page.AsOf); err != nil {
		return page, nil, historyFailure(historyquery.ErrUnavailable, err)
	}
	var err error
	page.AnalyticSequence, err = coverageUint(analyticText)
	if err != nil || revision < 0 || analyticRun < 0 {
		return page, nil, historyquery.ErrIntegrity
	}
	page.AnalyticRun = analyticRun
	page.AsOf = page.AsOf.UTC()
	page.RawCutoff = rawCutoff.UTC()
	policyCutoff := page.AsOf.Add(-7 * 24 * time.Hour)
	if policyCutoff.After(page.RawCutoff) {
		page.RawCutoff = policyCutoff
	}
	page.AggregateCutoff, err = recordermaintenance.CivilCutoff(page.AsOf)
	if err != nil {
		return page, nil, historyquery.ErrIntegrity
	}
	next := &RecorderQueryCursor{kind: q.Kind(), rawCutoff: rawCutoff.UTC(), asOf: page.AsOf, revision: revision}
	var epoch []byte
	var sequence, policyExcluded string
	if err = tx.QueryRow(ctx, `SELECT epoch,run_id,checkpoint::text,policy_excluded::text FROM public.recorder_epochs ORDER BY run_id DESC LIMIT 1`).Scan(&epoch, &next.highRun, &sequence, &policyExcluded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return page, nil, historyquery.ErrUnavailable
		}
		return page, nil, historyFailure(historyquery.ErrUnavailable, err)
	}
	next.highEpoch, err = coverageEpoch(epoch)
	if err != nil {
		return page, nil, historyquery.ErrIntegrity
	}
	next.highSequence, err = coverageUint(sequence)
	if err != nil {
		return page, nil, historyquery.ErrIntegrity
	}
	next.highPolicyExcluded, err = coverageUint(policyExcluded)
	if err != nil || next.highPolicyExcluded > next.highSequence {
		return page, nil, historyquery.ErrIntegrity
	}
	next.liveSequence = next.highSequence
	if live != nil {
		if !live.epoch.valid() {
			return page, nil, historyquery.ErrInvalid
		}
		if live.epoch != next.highEpoch {
			if cursor != nil {
				return page, nil, historyquery.ErrStale
			}
			return page, nil, historyquery.ErrUnavailable
		}
		if live.sequence > next.liveSequence {
			next.liveSequence = live.sequence
		}
	}
	if cursor != nil {
		if live != nil && cursor.highEpoch != live.epoch {
			return page, nil, historyquery.ErrStale
		}
		if _, err = cursor.MarshalBinary(); err != nil || rawCutoff.After(cursor.rawCutoff) || (q.Kind() == historyquery.Aggregates && revision != cursor.revision) || cursor.highRun > next.highRun {
			return page, nil, historyquery.ErrStale
		}
		*next = *cursor
		page.AsOf = cursor.asOf
		// Freeze the policy window across continuation; actual purge still invalidates.
		page.RawCutoff = maxHistoryTime(cursor.rawCutoff, cursor.asOf.Add(-7*24*time.Hour))
		page.AggregateCutoff, _ = recordermaintenance.CivilCutoff(cursor.asOf)
	}
	_, end := q.Bounds()
	cutoff := page.RawCutoff
	if q.Kind() == historyquery.Aggregates {
		cutoff = page.AggregateCutoff
	}
	page.NotRetained = !end.After(cutoff)
	page.AnalyticalTailUnproven = analyticRun < next.highRun || (analyticRun == next.highRun && page.AnalyticSequence < next.highSequence)
	return page, next, nil
}
func maxHistoryTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func readHistoricalObservations(ctx context.Context, tx pgx.Tx, q historyquery.Query, next *RecorderQueryCursor, page *RecorderQueryPage) error {
	entity, _ := q.Entity().Value()
	start, end := q.Bounds()
	start = maxHistoryTime(start, page.RawCutoff)
	// The first query reads only bounded keys and lengths, never the payload.
	// Exact identity AND digest scope the SQL, before any protobuf reaches Go.
	rows, err := tx.Query(ctx, `SELECT h.epoch,e.run_id,h.sequence::text,octet_length(h.payload),octet_length(h.entity_id),COALESCE(octet_length(h.measurement_unit),0),COALESCE(octet_length(h.measurement_canonical_unit),0),(octet_length(h.source_session)::bigint+octet_length(h.instance_id)+COALESCE(octet_length(h.causation_id),0)+COALESCE(octet_length(h.correlation_id),0))
 FROM public.state_history h JOIN public.recorder_epochs e ON e.epoch=h.epoch
 WHERE h.entity_digest=$1 AND h.entity_id=$2 AND h.received_at >= $3 AND h.received_at < $4
 AND (e.run_id,h.sequence)<=($5,$6::numeric) AND (e.run_id,h.sequence)>($7,$8::numeric)
 ORDER BY e.run_id,h.sequence LIMIT $9`, hashEntity(entity), entity, start.Truncate(time.Microsecond), historyCeilMicro(end), next.highRun, decimalUint(next.highSequence), next.afterRun, decimalUint(next.afterSequence), historyquery.MaxPoints+1)
	if err != nil {
		return historyFailure(historyquery.ErrUnavailable, err)
	}
	type key struct {
		epoch    RecorderEpoch
		run      int64
		sequence uint64
		size     int
	}
	keys := make([]key, 0, historyquery.MaxPoints)
	bytesLeft := historyquery.MaxPointBytes
	more := false
	inputLeft := 4 << 20
	for rows.Next() {
		var raw []byte
		var seq string
		var run, payload, identity, unit, canonical, metadata int64
		if err = rows.Scan(&raw, &run, &seq, &payload, &identity, &unit, &canonical, &metadata); err != nil {
			rows.Close()
			return historyFailure(historyquery.ErrIntegrity, err)
		}
		if len(keys) == historyquery.MaxPoints {
			more = true
			break
		}
		// Fail before materializing any oversized point. Do not skip or advance it.
		charge := payload + identity + unit + canonical + 512
		if payload < 1 || identity < 1 || unit < 0 || canonical < 0 || run < 1 {
			rows.Close()
			return historyquery.ErrIntegrity
		}
		if metadata < 0 {
			rows.Close()
			return historyquery.ErrIntegrity
		}
		if charge > historyquery.MaxPointBytes || metadata+payload > 4<<20 {
			if len(keys) > 0 {
				more = true
				break
			}
			rows.Close()
			return historyquery.ErrPointTooLarge
		}
		if charge > int64(bytesLeft) || metadata+payload > int64(inputLeft) {
			more = true
			break
		}
		inputLeft -= int(metadata + payload)
		k := key{run: run, size: int(payload)}
		k.epoch, err = coverageEpoch(raw)
		if err != nil {
			rows.Close()
			return historyquery.ErrIntegrity
		}
		k.sequence, err = coverageUint(seq)
		if err != nil || k.sequence == 0 {
			rows.Close()
			return historyquery.ErrIntegrity
		}
		keys = append(keys, k)
		bytesLeft -= int(charge)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return historyFailure(historyquery.ErrUnavailable, err)
	}
	page.Observations = make([]HistoricalObservation, 0, len(keys))
	var examined *key
	for index := range keys {
		k := keys[index]
		if len(page.Observations) == q.Limit() {
			more = true
			break
		}
		var payload, digest []byte
		if err = tx.QueryRow(ctx, `SELECT payload,payload_digest FROM public.state_history WHERE epoch=$1 AND sequence=$2::numeric AND entity_digest=$3 AND entity_id=$4 AND octet_length(payload)=$5`, k.epoch.data(), decimalUint(k.sequence), hashEntity(entity), entity, k.size).Scan(&payload, &digest); err != nil {
			return historyFailure(historyquery.ErrUnavailable, err)
		}
		actual := sha256.Sum256(payload)
		if len(payload) != k.size || !bytes.Equal(digest, actual[:]) {
			return historyquery.ErrIntegrity
		}
		wire := &state.ObservedState{}
		if err = (proto.UnmarshalOptions{RecursionLimit: 16}).Unmarshal(payload, wire); err != nil {
			return historyquery.ErrIntegrity
		}
		if contracts.AdmitHistoricalState(wire) != nil || wire.Entity.GetEntityId() != entity {
			return historyquery.ErrIntegrity
		}
		descriptor, err := verifyHistoricalRecord(ctx, tx, k.epoch, k.sequence, wire, payload)
		if err != nil {
			return err
		}
		examined = &keys[index]
		// SQL only preselects conservatively on its microsecond lattice. The
		// admitted payload retains the exact nanoseconds and decides membership.
		received := wire.ReceivedAt.AsTime()
		if received.Before(start) || !received.Before(end) {
			continue
		}
		point := HistoricalObservation{Epoch: k.epoch, Sequence: k.sequence, State: wire, Descriptor: descriptor}
		page.Observations = append(page.Observations, point)
	}
	if more && examined != nil {
		last := *examined
		next.afterRun = last.run
		next.afterSequence = last.sequence
		copy := *next
		page.Next = &copy
	}
	return nil
}

func readHistoricalAggregates(ctx context.Context, tx pgx.Tx, q historyquery.Query, next *RecorderQueryCursor, page *RecorderQueryPage) error {
	entity, _ := q.Entity().Value()
	start, end := q.Bounds()
	rows, err := tx.Query(ctx, `SELECT a.entity_id,a.bucket_start,a.bucket_end,a.group_digest,a.descriptor,a.availability,a.freshness,a.provenance,a.duration_ns,a.weighted,a.minimum,a.maximum,a.contribution,a.reset_kind,
 (a.reasons | COALESCE((SELECT bit_or(b.reasons) FROM public.recorder_analytical_breaks b WHERE b.entity_digest=a.entity_digest AND b.start_at<a.bucket_end AND (b.end_at>a.bucket_start OR (b.start_at=b.end_at AND b.start_at>=a.bucket_start))),0))
 FROM public.recorder_hourly_aggregates a WHERE a.entity_digest=$1 AND a.entity_id=$2 AND a.bucket_start >= $3 AND a.bucket_start < $4 AND a.bucket_end>$5
 AND ($6::timestamptz IS NULL OR (a.bucket_start,a.group_digest)>($6,$7)) ORDER BY a.bucket_start,a.group_digest LIMIT $8`, hashEntity(entity), entity, start, end, page.AggregateCutoff, historyNullableTime(next.afterBucket), next.group[:], q.Limit()+1)
	if err != nil {
		return historyFailure(historyquery.ErrUnavailable, err)
	}
	defer rows.Close()
	page.Aggregates = make([]RecorderHourlyAggregate, 0, q.Limit())
	more := false
	for rows.Next() {
		if len(page.Aggregates) == q.Limit() {
			more = true
			break
		}
		var a RecorderHourlyAggregate
		var descriptor, group []byte
		var av, fr, pr, reset int16
		var duration int64
		var reasons int32
		var minimum, maximum, delta *float64
		var weighted float64
		if err = rows.Scan(&a.Entity, &a.Start, &a.End, &group, &descriptor, &av, &fr, &pr, &duration, &weighted, &minimum, &maximum, &delta, &reset, &reasons); err != nil {
			return historyFailure(historyquery.ErrIntegrity, err)
		}
		if a.Entity != entity || len(group) != 32 || av < 1 || av > 3 || fr < 1 || fr > 2 || pr < 1 || pr > 3 {
			return historyquery.ErrIntegrity
		}
		a.Start = a.Start.UTC()
		a.End = a.End.UTC()
		if !a.End.Equal(a.Start.Add(time.Hour)) || !a.Start.Equal(a.Start.Truncate(time.Hour)) {
			return historyquery.ErrIntegrity
		}
		a.Descriptor, err = decodeAggregateDescriptor(descriptor)
		if err != nil {
			return historyquery.ErrIntegrity
		}
		a.Availability = domain.Availability(av)
		a.Freshness = domain.Freshness(fr)
		a.Provenance = domain.Provenance(pr)
		if _, err = recordermaintenance.NewPoint(recordermaintenance.PointInput{Epoch: "00000000000000000000000000000001", Sequence: 1, At: a.Start, Availability: a.Availability, Freshness: a.Freshness, Provenance: a.Provenance, Kind: domain.ValueMeasurement, Numeric: &a.Descriptor}); err != nil {
			return historyquery.ErrIntegrity
		}
		a.Values, err = aggregateValues(duration, weighted, minimum, maximum, delta, reset, reasons)
		if err != nil {
			return historyquery.ErrIntegrity
		}
		page.Aggregates = append(page.Aggregates, a)
		next.afterBucket = a.Start
		copy(next.group[:], group)
	}
	if err = rows.Err(); err != nil {
		return historyFailure(historyquery.ErrUnavailable, err)
	}
	if more {
		copy := *next
		page.Next = &copy
	}
	return nil
}
