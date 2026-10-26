package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func TestNHF009RecorderUpgrade26ToCurrentPreservesHistoryAndControlValues(t *testing.T) {
	ctx := context.Background()
	settings := testSettings(t, testCluster(t))
	store, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Fixture built from the frozen historical SQL, not an old executable.
	// The release qualification separately exercises the actual baseline binary.
	if version, err := store.migrate(ctx, migrations[:25]); err != nil || version.Number() != 25 {
		t.Fatalf("historical prefix: %v", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, migrations[25].sql); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES (26)`); err != nil {
		t.Fatal(err)
	}
	baseline := schemaPlanSHA256(migrations[:26])
	if _, err := tx.Exec(ctx, `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan) VALUES (true,1,26,$1,$1,$1,$1,26,$2,$2)`, prior26SchemaRelease, baseline[:]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	epoch := nha030Epoch(t)
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now()); err != nil || outcome != TransactionCommitted {
		t.Fatal(err)
	}
	switchValue := domain.NewSwitchValue(false)
	old := nha105SQLRecord(t, "nhf009-old-switch", 1, &switchValue)
	batch, err := NewRecorderBatch(epoch, 0, 1, []RecorderRecord{old}, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha105PersistHistoricalBatch(t, store, batch)
	if _, err := store.pool.Exec(ctx, `UPDATE public.state_history SET value_kind=9 WHERE sequence=1`); err == nil {
		t.Fatal("schema26 admitted cover kind")
	} else {
		nha151SQLState(t, err, "23514")
	}
	for _, damage := range []string{
		`UPDATE public.homeward_schema_window SET migration_plan=sha256('damaged'::bytea)`,
		`UPDATE public.homeward_schema_window SET minimum_reader='unknown-release'`,
		`DELETE FROM public.homeward_schema_window`,
	} {
		if _, err := store.pool.Exec(ctx, damage); err != nil {
			t.Fatal(err)
		}
		var classified *Error
		if _, err := store.Migrate(ctx); !errors.As(err, &classified) || classified.Kind() != InconsistentSchema {
			t.Fatalf("damaged SQL26 window upgraded: %v", err)
		}
		var version int
		if err := store.pool.QueryRow(ctx, `SELECT max(version) FROM public.homeward_schema_migrations`).Scan(&version); err != nil || version != 26 {
			t.Fatal("refused upgrade changed migration history", err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan) VALUES (true,1,26,$1,$1,$1,$1,26,$2,$2) ON CONFLICT (singleton) DO UPDATE SET minimum_reader=EXCLUDED.minimum_reader,migration_plan=EXCLUDED.migration_plan`, prior26SchemaRelease, baseline[:]); err != nil {
			t.Fatal(err)
		}
	}
	if version, err := store.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatalf("current schema: %v", err)
	}
	build := CurrentSchemaBuild()
	if build.Release() != schemaWindowBaselineRelease || build.reader != schemaReleaseBaseline || build.writer != schemaReleaseBaseline {
		t.Fatal("current schema falsely promises old reader/writer compatibility")
	}
	position, obstructed := 0.0, false
	cover, _ := domain.NewCoverValue(nil, &position, &obstructed)
	coverState, _ := domain.NewCoverStateValue(cover)
	lockState, _ := domain.NewLockStateValue(domain.LockStateJammed)
	alarmState, _ := domain.NewAlarmStateValue(domain.AlarmStateTriggered)
	values := []domain.StateValue{coverState, lockState, alarmState}
	rows := make([]RecorderRecord, len(values))
	for index, value := range values {
		rows[index] = nha105SQLRecord(t, "nhf009-control", uint64(index+2), &value)
	}
	newBatch, err := NewRecorderBatch(epoch, 1, 4, rows, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha105PersistHistoricalBatch(t, store, newBatch)
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE value_kind IN (9,10,11) AND measurement_value IS NULL AND measurement_unit IS NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatal("control observations became numeric measurements", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.state_history SET value_kind=32767 WHERE sequence=2`); err == nil {
		t.Fatal("unknown future kind admitted")
	} else {
		nha151SQLState(t, err, "23514")
	}
	if outcome, err := store.CloseRecorderEpoch(ctx, epoch, 4); err != nil || outcome != TransactionCommitted {
		t.Fatal(err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, row := range append([]RecorderRecord{old}, rows...) {
		var payload, digest, recordDigest []byte
		if err := reopened.pool.QueryRow(ctx, `SELECT payload,payload_digest,record_digest FROM public.state_history WHERE epoch=$1 AND sequence=$2`, epoch.data(), row.sequence).Scan(&payload, &digest, &recordDigest); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, row.observed.payload) || !bytes.Equal(digest, row.observed.payloadDigest[:]) || !bytes.Equal(recordDigest, row.digest[:]) {
			t.Fatal("upgrade/reopen changed historical bytes")
		}
		var wire statewire.ObservedState
		if err := proto.Unmarshal(payload, &wire); err != nil {
			t.Fatal(err)
		}
		switch row.sequence {
		case 1:
			if wire.GetSwitchOn() {
				t.Fatal("historical false changed")
			}
		case 2:
			decoded, err := contracts.AdmitWireCoverValue(wire.GetCover())
			if err != nil || decoded != cover {
				t.Fatal("cover presence lost", err)
			}
		case 3:
			decoded, err := contracts.AdmitWireLockValue(wire.GetLock())
			if err != nil || decoded != domain.LockStateJammed {
				t.Fatal("jammed lock changed", err)
			}
		case 4:
			decoded, err := contracts.AdmitWireAlarmValue(wire.GetAlarm())
			if err != nil || decoded != domain.AlarmStateTriggered {
				t.Fatal("triggered alarm changed", err)
			}
		}
	}
	checkpoint, err := reopened.RecorderCheckpoint(ctx, epoch)
	if err != nil || !checkpoint.Confirms(newBatch) || !checkpoint.CleanClose {
		t.Fatal("control checkpoint lost", err)
	}
}
