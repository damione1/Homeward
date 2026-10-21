package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"github.com/damione1/Homeward/contracts"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	"testing"
	"time"
)

func nha105SQLRecord(t *testing.T, entity string, sequence uint64, value *domain.StateValue) RecorderRecord {
	t.Helper()
	id, _ := domain.ParseEntityID(entity)
	instance, _ := domain.ParseIntegrationInstanceID("nha105-sql")
	availability := domain.AvailabilityAvailable
	if value == nil {
		availability = domain.AvailabilityUnavailable
	}
	quality, _ := domain.NewQuality(availability, domain.FreshnessFresh, domain.ProvenanceObserved)
	source, _ := domain.NewSourceStamp("nha105-sql-session", &sequence)
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	state, err := domain.NewObservedStateSnapshot(id, quality, value, domain.StateTimes{ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, source, sequence, nil)
	if err != nil {
		t.Fatal(err)
	}
	cause, _ := domain.NewCausality(nil, nil)
	row, err := NewRecorderRecord(sequence, instance, state, cause, at, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestNHA105RecorderUpgrade16To17(t *testing.T) {
	ctx := context.Background()
	settings := testSettings(t, testCluster(t))
	store, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.migrate(ctx, migrations[:16]); err != nil || version.Number() != 16 {
		t.Fatal("historical schema16 failed")
	}
	epoch := nha030Epoch(t)
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now()); err != nil || outcome != TransactionCommitted {
		t.Fatal(err)
	}
	switchValue := domain.NewSwitchValue(false)
	helper, _ := domain.NewHelperStateValue(domain.NewHelperBoolean(false), 1)
	old := []RecorderRecord{nha105SQLRecord(t, "nha105-old-switch", 1, &switchValue), nha105SQLRecord(t, "nha105-old-helper", 2, &helper), nha105SQLRecord(t, "nha105-old-unavailable", 3, nil)}
	batch, err := NewRecorderBatch(epoch, 0, 3, old, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha105PersistHistoricalBatch(t, store, batch)
	if _, err := store.pool.Exec(ctx, `UPDATE public.state_history SET value_kind=7 WHERE sequence=1`); err == nil {
		t.Fatal("schema16 admitted solar tag")
	} else {
		nha151SQLState(t, err, "23514")
	}
	// Keep this historical upgrade pinned to17 as later migrations are added.
	if version, err := store.migrate(ctx, migrations[:17]); err != nil || version.Number() != 17 {
		t.Fatal("additive schema17 failed")
	}
	for _, row := range old {
		var payload, digest, recordDigest []byte
		if err := store.pool.QueryRow(ctx, `SELECT payload,payload_digest,record_digest FROM public.state_history WHERE epoch=$1 AND sequence=$2`, epoch.data(), row.sequence).Scan(&payload, &digest, &recordDigest); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, row.observed.payload) || !bytes.Equal(digest, row.observed.payloadDigest[:]) || !bytes.Equal(recordDigest, row.digest[:]) {
			t.Fatal("migration changed historical payload/digest")
		}
	}
	absent, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i] = absent
	}
	input.Next[3], _ = domain.SolarOccurrenceAt(time.Unix(100, 0))
	input.Next[2], _ = domain.SolarOccurrenceAt(time.Unix(200, 0))
	solar, _ := domain.NewSolarValue(input)
	solarState, _ := domain.NewSolarStateValue(solar)
	event, _ := domain.NewSolarEventValue(domain.SolarDawn, absent)
	eventState, _ := domain.NewSolarEventStateValue(event)
	rows := []RecorderRecord{nha105SQLRecord(t, "nha105-sun", 4, &solarState), nha105SQLRecord(t, "nha105-event", 5, &eventState)}
	newBatch, err := NewRecorderBatch(epoch, 3, 5, rows, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha105PersistHistoricalBatch(t, store, newBatch)
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE value_kind IN (7,8) AND measurement_value IS NULL AND measurement_unit IS NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatal("solar tags became numeric measurements")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.state_history SET value_kind=9 WHERE sequence=4`); err == nil {
		t.Fatal("schema17 admitted future tag")
	} else {
		nha151SQLState(t, err, "23514")
	}
	if outcome, err := store.CloseRecorderEpoch(ctx, epoch, 5); err != nil || outcome != TransactionCommitted {
		t.Fatal(err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, row := range rows {
		var payload, digest []byte
		if err := reopened.pool.QueryRow(ctx, `SELECT payload,payload_digest FROM public.state_history WHERE epoch=$1 AND sequence=$2`, epoch.data(), row.sequence).Scan(&payload, &digest); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, row.observed.payload) || !bytes.Equal(digest, row.observed.payloadDigest[:]) {
			t.Fatal("reopen changed solar bytes")
		}
		var wire statewire.ObservedState
		if err := proto.Unmarshal(payload, &wire); err != nil {
			t.Fatal(err)
		}
		if row.sequence == 4 {
			if value, err := contracts.AdmitWireSolarValue(wire.GetSolar()); err != nil || value != solar {
				t.Fatal("solar reopen lost typed values")
			}
		} else {
			if value, err := contracts.AdmitWireSolarEventValue(wire.GetSolarEvent()); err != nil || value != event {
				t.Fatal("event reopen lost typed absence")
			}
		}
	}
	checkpoint, err := reopened.RecorderCheckpoint(ctx, epoch)
	if err != nil || !checkpoint.Confirms(newBatch) || !checkpoint.CleanClose {
		t.Fatal("reopen lost checkpoint or clean-close")
	}
}

func TestNHA105DeclarativeHistoryRemainsExact(t *testing.T) {
	store, ctx := nha151Store(t)
	binding := nha151Binding(t, "nha105.configuration", "configuration.yaml")
	v1, _ := nha151Candidate(t, "configuration.yaml", "solar history control")
	_, first := nha151Write(t, store, ctx, v1, binding, ExpectNoConfigActive())
	v2, err := bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := ExpectConfigActive(first)
	_, second := nha151Write(t, store, ctx, v2, binding, expected)
	for _, item := range []struct {
		generation ConfigGenerationRevision
		candidate  bundle.Candidate
	}{{first, v1}, {second, v2}} {
		snapshot, err := store.ReadConfigGeneration(ctx, item.generation)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := snapshot.Restore()
		if err != nil || restored.Schema() != item.candidate.Schema() || restored.Profile() != item.candidate.Profile() || restored.SHA256() != item.candidate.SHA256() {
			t.Fatal("historical declarative authority changed")
		}
		for i, source := range restored.Sources() {
			original := item.candidate.Sources()[i]
			if source.SHA256() != original.SHA256() || sha256.Sum256(source.Bytes()) != source.SHA256() {
				t.Fatal("historical source hash changed")
			}
		}
	}
}

// This test deliberately stays on historical16/17. The current writer requires
// migration21's retention fence; use the unchanged row encoder to construct
// historical bytes, with the same checkpoint/digest in one real transaction.
func nha105PersistHistoricalBatch(t *testing.T, store *Store, batch RecorderBatch) {
	t.Helper()
	if len(batch.gaps) != 0 || batch.image != nil || batch.policyExcluded != 0 {
		t.Fatal("unsupported historical fixture batch")
	}
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		for _, row := range batch.states {
			if err := insertRecorderState(context.Background(), tx, batch.epoch, row); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(context.Background(), `UPDATE public.recorder_epochs SET checkpoint=$3::numeric,last_batch_from=$2::numeric,last_batch_to=$3::numeric,last_batch_digest=$4 WHERE epoch=$1 AND checkpoint=$2::numeric`, batch.epoch.data(), decimalUint(batch.from), decimalUint(batch.through), batch.digest[:])
		if err == nil && tag.RowsAffected() != 1 {
			return ErrRecorderIntegrity
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
