package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
)

func nha030Epoch(t *testing.T) RecorderEpoch {
	t.Helper()
	epoch, err := ParseRecorderEpoch("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

// The production migration installs the Recorder schema in disposable PG.
func nha030RecorderSQLFixture(t *testing.T) *Store {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := ParseSettings(ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestNHA030RecorderSQLBatchIdempotenceAndUint64(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	epoch := nha030Epoch(t)
	previous, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now())
	if outcome != TransactionCommitted || err != nil || previous.Present {
		t.Fatalf("open epoch: %+v %v %v", previous, outcome, err)
	}
	const high = uint64(1<<63) + 9
	row := nha030Record(high, strings.Repeat("long-open-entity-id-", 600))
	row.admittedAt = time.Date(2026, 10, 18, 12, 0, 0, 123456789, time.UTC)
	row.digest = digestRecorderRecord(row)
	batch, err := NewRecorderBatch(epoch, 0, high, []RecorderRecord{row}, []RecorderGap{{From: 1, Through: high, Reason: RecorderGapReconciliation, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		outcome, err := store.PersistRecorderBatch(ctx, batch)
		if outcome != TransactionCommitted || err != nil {
			t.Fatalf("persist/replay %d: %v %v", i, outcome, err)
		}
	}
	checkpoint, err := store.RecorderCheckpoint(ctx, epoch)
	if err != nil || !checkpoint.Confirms(batch) {
		t.Fatalf("uint64 checkpoint or digest changed: %+v %v", checkpoint, err)
	}
	var storedEntity, storedSequence string
	var seconds int64
	var nanos int
	if err := store.pool.QueryRow(ctx, `SELECT entity_id,sequence::text,admitted_unix_seconds,admitted_nanos FROM public.state_history WHERE epoch=$1`, epoch.data()).Scan(&storedEntity, &storedSequence, &seconds, &nanos); err != nil {
		t.Fatal(err)
	}
	if storedEntity != row.observed.entity || storedSequence != decimalUint(high) || seconds != row.admittedAt.Unix() || nanos != row.admittedAt.Nanosecond() {
		t.Fatalf("SQL precision or long ID changed: entity=%d sequence=%s seconds=%d nanos=%d", len(storedEntity), storedSequence, seconds, nanos)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE epoch=$1`, epoch.data()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotence inserted %d rows: %v", count, err)
	}
	changed := row
	changed.observed = cloneRecorderObserved(row.observed)
	changed.observed.payload = []byte("changed")
	changed.observed.payloadDigest = sha256.Sum256(changed.observed.payload)
	changed.digest = digestRecorderRecord(changed)
	conflict, err := NewRecorderBatch(epoch, 0, high, []RecorderRecord{changed}, []RecorderGap{{From: 1, Through: high, Reason: RecorderGapReconciliation, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PersistRecorderBatch(ctx, conflict); !errors.Is(err, ErrRecorderIntegrity) {
		t.Fatalf("different replay was accepted: %v", err)
	}
	nextEpoch, err := ParseRecorderEpoch("fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	previous, outcome, err = store.OpenRecorderEpoch(ctx, nextEpoch, time.Now())
	if outcome != TransactionCommitted || err != nil || !previous.Present || previous.CleanClose || previous.Checkpoint != high {
		t.Fatalf("unclean previous epoch was not explicit: %+v %v %v", previous, outcome, err)
	}
}

func TestNHA030ZeroWidthImageRejectsDifferentReplay(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	epoch := nha030Epoch(t)
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now()); outcome != TransactionCommitted || err != nil {
		t.Fatalf("open: %v %v", outcome, err)
	}
	first, err := NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(ctx, first); outcome != TransactionCommitted || err != nil {
		t.Fatalf("first image: %v %v", outcome, err)
	}
	row := nha030Record(1, "existing-state")
	snapshot := RecorderSnapshotRecord{observed: cloneRecorderObserved(row.observed)}
	snapshot.digest = digestRecorderSnapshot(snapshot)
	changed, err := NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &RecorderImage{Boundary: 0, States: []RecorderSnapshotRecord{snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PersistRecorderBatch(ctx, changed); !errors.Is(err, ErrRecorderIntegrity) {
		t.Fatalf("different image at same zero-width checkpoint accepted: %v", err)
	}
	checkpoint, err := store.RecorderCheckpoint(ctx, epoch)
	if err != nil || !checkpoint.Confirms(first) {
		t.Fatalf("original image fingerprint changed: %+v %v", checkpoint, err)
	}
}

func TestNHA030CheckpointReconciliationTracksLatestGapAndImage(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	epoch := nha030Epoch(t)
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now()); outcome != TransactionCommitted || err != nil {
		t.Fatalf("open: %v %v", outcome, err)
	}
	image, err := NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &RecorderImage{Boundary: 0})
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	gap, err := NewRecorderBatch(epoch, 0, 1, nil, []RecorderGap{{From: 1, Through: 1, KnownLost: &one, Reason: RecorderGapOverflow, UncertainEnvelope: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	regular, err := NewRecorderBatch(epoch, 1, 2, []RecorderRecord{nha030Record(2, "state-two")}, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewRecorderBatch(epoch, 2, 2, nil, nil, 0, &RecorderImage{Boundary: 2})
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewRecorderBatch(epoch, 2, 3, []RecorderRecord{nha030Record(3, "state-three")}, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		batch    RecorderBatch
		complete bool
	}{
		{image, true},
		{gap, false},
		{regular, false},
		{recovery, true},
		{next, true},
	} {
		if outcome, err := store.PersistRecorderBatch(ctx, step.batch); outcome != TransactionCommitted || err != nil {
			t.Fatalf("persist through %d: %v %v", step.batch.Through(), outcome, err)
		}
		checkpoint, err := store.RecorderCheckpoint(ctx, epoch)
		if err != nil || !checkpoint.Confirms(step.batch) || checkpoint.ReconciliationComplete != step.complete {
			t.Fatalf("incorrect reconciliation after through %d: %+v %v", step.batch.Through(), checkpoint, err)
		}
	}
}

func TestNHA030RecorderSQLStateProvenance(t *testing.T) {
	store := nha030RecorderSQLFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	epoch := nha030Epoch(t)
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now()); outcome != TransactionCommitted || err != nil {
		t.Fatalf("open: %v %v", outcome, err)
	}
	entity, err := domain.ParseEntityID("state-with-optional-values")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("fake-pilot")
	if err != nil {
		t.Fatal(err)
	}
	noSequence, err := domain.NewSourceStamp("session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	zeroSequence, err := domain.NewSourceStamp("session-1", &zero)
	if err != nil {
		t.Fatal(err)
	}
	available, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceRestored)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceEstimated)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := domain.NewQuality(domain.AvailabilityUnknown, domain.FreshnessStale, domain.ProvenanceRestored)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 18, 12, 0, 0, 123456789, time.UTC)
	falseValue := domain.NewSwitchValue(false)
	first, err := domain.NewObservedStateSnapshot(entity, available, &falseValue, domain.StateTimes{ReceivedAt: base, LastChanged: base, LastUpdated: base, LastReported: base}, noSequence, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog := domain.NewUnitCatalog()
	if err := catalog.Register("custom-affine", domain.DimensionTemperature, 2.5, 17); err != nil {
		t.Fatal(err)
	}
	affine, err := catalog.Parse("custom-affine")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(0, affine, 2, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	zeroValue, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	observed := base.Add(123 * time.Nanosecond)
	secondTime := base.Add(time.Second + 321*time.Nanosecond)
	second, err := domain.NewObservedStateSnapshot(entity, restored, &zeroValue, domain.StateTimes{ObservedAt: &observed, ReceivedAt: secondTime, LastChanged: secondTime, LastUpdated: secondTime, LastReported: secondTime}, zeroSequence, 2, &first)
	if err != nil {
		t.Fatal(err)
	}
	thirdTime := secondTime.Add(time.Second)
	third, err := domain.NewObservedStateSnapshot(entity, unavailable, nil, domain.StateTimes{ReceivedAt: thirdTime, LastChanged: thirdTime, LastUpdated: thirdTime, LastReported: thirdTime}, noSequence, 3, &second)
	if err != nil {
		t.Fatal(err)
	}
	if _, current := third.CurrentValue(); current {
		t.Fatal("unavailable state unexpectedly has a current value")
	}
	if _, display := third.DisplayValue(); !display {
		t.Fatal("fixture lost last-known display value")
	}
	kilowatts, err := catalog.Parse("kW")
	if err != nil {
		t.Fatal(err)
	}
	watts, err := catalog.Parse("W")
	if err != nil {
		t.Fatal(err)
	}
	whole, err := domain.NewMeasurement(1, kilowatts, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	coarse, err := whole.Convert(watts)
	if err != nil {
		t.Fatal(err)
	}
	coarseValue, err := domain.NewMeasurementValue(coarse)
	if err != nil {
		t.Fatal(err)
	}
	fourthTime := thirdTime.Add(time.Second)
	fourth, err := domain.NewObservedStateSnapshot(entity, available, &coarseValue, domain.StateTimes{ReceivedAt: fourthTime, LastChanged: fourthTime, LastUpdated: fourthTime, LastReported: fourthTime}, noSequence, 4, &third)
	if err != nil {
		t.Fatal(err)
	}
	fifthTime := fourthTime.Add(time.Second)
	fifth, err := domain.NewObservedStateSnapshot(entity, unknown, nil, domain.StateTimes{ReceivedAt: fifthTime, LastChanged: fifthTime, LastUpdated: fifthTime, LastReported: fifthTime}, noSequence, 5, &fourth)
	if err != nil {
		t.Fatal(err)
	}
	states := []domain.ObservedState{first, second, third, fourth, fifth}
	rows := make([]RecorderRecord, 0, len(states))
	for i, state := range states {
		row, err := NewRecorderRecord(uint64(i+1), instance, state, domain.Causality{}, base.Add(time.Duration(i+1)*time.Second+987*time.Nanosecond), 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	batch, err := NewRecorderBatch(epoch, 0, 5, rows, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(ctx, batch); outcome != TransactionCommitted || err != nil {
		t.Fatalf("persist: %v %v", outcome, err)
	}
	var observedAbsent, sourceAbsent bool
	var firstPayload []byte
	if err := store.pool.QueryRow(ctx, `SELECT observed_at IS NULL,source_sequence IS NULL,payload FROM public.state_history WHERE epoch=$1 AND sequence=1`, epoch.data()).Scan(&observedAbsent, &sourceAbsent, &firstPayload); err != nil {
		t.Fatal(err)
	}
	var firstWire statewire.ObservedState
	if err := proto.Unmarshal(firstPayload, &firstWire); err != nil {
		t.Fatal(err)
	}
	if !observedAbsent || !sourceAbsent || firstWire.GetObservedAt() != nil || firstWire.GetSwitchOn() || firstWire.SourceSeq != nil {
		t.Fatal("false, absent observation or absent source sequence conflated")
	}
	var secondSource, value, scale, offset string
	var secondPayload []byte
	if err := store.pool.QueryRow(ctx, `SELECT source_sequence::text,measurement_value::text,measurement_scale::text,measurement_offset::text,payload FROM public.state_history WHERE epoch=$1 AND sequence=2`, epoch.data()).Scan(&secondSource, &value, &scale, &offset, &secondPayload); err != nil {
		t.Fatal(err)
	}
	var secondWire statewire.ObservedState
	if err := proto.Unmarshal(secondPayload, &secondWire); err != nil {
		t.Fatal(err)
	}
	if secondSource != "0" || value != "0" || scale != "2.5" || offset != "17" || !secondWire.GetObservedAt().AsTime().Equal(observed) || !secondWire.GetReceivedAt().AsTime().Equal(secondTime) {
		t.Fatalf("zero, affine coefficients or observed nanos changed: %q %q %q %q", secondSource, value, scale, offset)
	}
	var noCurrent, noMeasurement bool
	var thirdPayload []byte
	if err := store.pool.QueryRow(ctx, `SELECT value_kind IS NULL,measurement_value IS NULL,payload FROM public.state_history WHERE epoch=$1 AND sequence=3`, epoch.data()).Scan(&noCurrent, &noMeasurement, &thirdPayload); err != nil {
		t.Fatal(err)
	}
	var thirdWire statewire.ObservedState
	if err := proto.Unmarshal(thirdPayload, &thirdWire); err != nil {
		t.Fatal(err)
	}
	if !noCurrent || !noMeasurement || thirdWire.GetMeasurement() != nil {
		t.Fatal("last-known value was promoted into unavailable current state")
	}
	var negativePrecision string
	if err := store.pool.QueryRow(ctx, `SELECT measurement_precision::text FROM public.state_history WHERE epoch=$1 AND sequence=4`, epoch.data()).Scan(&negativePrecision); err != nil {
		t.Fatal(err)
	}
	if negativePrecision != "-3" {
		t.Fatalf("converted negative precision was lost: %s", negativePrecision)
	}
	for sequence, expected := range map[int][3]int16{
		2: {int16(domain.AvailabilityAvailable), int16(domain.FreshnessStale), int16(domain.ProvenanceRestored)},
		3: {int16(domain.AvailabilityUnavailable), int16(domain.FreshnessStale), int16(domain.ProvenanceEstimated)},
		5: {int16(domain.AvailabilityUnknown), int16(domain.FreshnessStale), int16(domain.ProvenanceRestored)},
	} {
		var availability, freshness, provenance int16
		if err := store.pool.QueryRow(ctx, `SELECT availability,freshness,provenance FROM public.state_history WHERE epoch=$1 AND sequence=$2::numeric`, epoch.data(), sequence).Scan(&availability, &freshness, &provenance); err != nil {
			t.Fatal(err)
		}
		if [3]int16{availability, freshness, provenance} != expected {
			t.Fatalf("quality altered for sequence %d: %d/%d/%d", sequence, availability, freshness, provenance)
		}
	}
}

func TestNHA030DistinctObservationReceiptAndAdmissionClocks(t *testing.T) {
	observed := time.Date(2026, 10, 18, 12, 0, 0, 123456789, time.UTC)
	received := observed.Add(time.Second + 864197532*time.Nanosecond)
	admitted := received.Add(2*time.Second + 124232323*time.Nanosecond)
	entity, err := domain.ParseEntityID("open-entity-id")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("fake-pilot")
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceRestored)
	if err != nil {
		t.Fatal(err)
	}
	sourceZero := uint64(0)
	source, err := domain.NewSourceStamp("fake-session", &sourceZero)
	if err != nil {
		t.Fatal(err)
	}
	value := domain.NewSwitchValue(false)
	times := domain.StateTimes{ObservedAt: &observed, ReceivedAt: received, LastChanged: received, LastUpdated: received, LastReported: received}
	snapshot, err := domain.NewObservedStateSnapshot(entity, quality, &value, times, source, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := NewRecorderRecord(1, instance, snapshot, domain.Causality{}, admitted, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	var wire statewire.ObservedState
	if err := proto.Unmarshal(row.observed.payload, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.GetObservedAt().AsTime().Equal(observed) || !wire.GetReceivedAt().AsTime().Equal(received) || !row.admittedAt.Equal(admitted) || row.admittedAt.Nanosecond() == 0 {
		t.Fatalf("observation, receipt or admission precision changed: %+v %+v %+v", wire.GetObservedAt(), wire.GetReceivedAt(), row.admittedAt)
	}
	if row.observed.sourceSequence == nil || *row.observed.sourceSequence != 0 || !row.observed.receivedAt.Equal(received) || row.observed.valueKind == nil {
		t.Fatal("present source zero or typed state metadata was erased")
	}
	if row.admittedAt.Unix() == received.Unix() && row.admittedAt.Nanosecond() == received.Nanosecond() {
		t.Fatal("admission clock collapsed into observation receipt")
	}
}

func nha030Record(sequence uint64, entity string) RecorderRecord {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	o := recorderObserved{entity: entity, entityDigest: sha256.Sum256([]byte(entity)), sourceSession: "test-session", revision: 1,
		receivedAt: now, lastChanged: now, lastUpdated: now, lastReported: now,
		availability: 1, freshness: 1, provenance: 1, payload: []byte("state"), payloadDigest: sha256.Sum256([]byte("state"))}
	r := RecorderRecord{sequence: sequence, instance: "fake-pilot", admittedAt: now, observed: o}
	r.digest = digestRecorderRecord(r)
	return r
}

func TestNHA030BatchRejectsFalseCoverageAndCopiesInputs(t *testing.T) {
	epoch := nha030Epoch(t)
	row := nha030Record(1, "entity-with-an-open-id")
	one := uint64(1)
	gap := RecorderGap{From: 1, Through: 1, KnownLost: &one, Reason: RecorderGapOverflow}
	if _, err := NewRecorderBatch(epoch, 0, 2, []RecorderRecord{row}, []RecorderGap{gap}, 0, nil); err == nil {
		t.Fatal("overlapping exact state and gap advanced a false checkpoint")
	}
	if _, err := NewRecorderBatch(epoch, 0, 2, nil, []RecorderGap{gap, gap}, 0, nil); err == nil {
		t.Fatal("duplicate exact gaps advanced a false checkpoint")
	}
	uncertain := RecorderGap{From: 1, Through: 2, KnownLost: &one, Reason: RecorderGapOverflow, UncertainEnvelope: true}
	batch, err := NewRecorderBatch(epoch, 0, 2, []RecorderRecord{row}, []RecorderGap{uncertain}, 0, nil)
	if err != nil {
		t.Fatalf("explicit uncertain envelope rejected: %v", err)
	}
	if _, err := NewRecorderBatch(epoch, 0, 1, []RecorderRecord{row}, []RecorderGap{{From: 1, Through: 1, KnownLost: &one, Reason: RecorderGapOverflow, UncertainEnvelope: true}}, 0, nil); err == nil {
		t.Fatal("uncertain known loss exceeded the positions left after retained rows")
	}
	fingerprint := batch.Digest()
	row.observed.payload[0] = 'X'
	one = 2
	if batch.Digest() != fingerprint || batch.states[0].observed.payload[0] != 's' || *batch.gaps[0].KnownLost != 1 {
		t.Fatal("batch retained mutable caller input")
	}
	if _, err := NewRecorderBatch(epoch, 0, 2, nil, []RecorderGap{{From: 1, Through: 2, Reason: RecorderGapReconciliation, UncertainEnvelope: true}}, 0, nil); err != nil {
		t.Fatalf("oversize-image fallback could not persist uncertain coverage: %v", err)
	}
}
