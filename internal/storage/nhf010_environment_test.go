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

func TestNHF010RecorderUpgrade27ToCurrentPreservesHistoryAndNonNumericEnvironment(t *testing.T) {
	ctx := context.Background()
	settings := testSettings(t, testCluster(t))
	store, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.migrate(ctx, migrations[:25]); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	for _, migration := range migrations[25:27] {
		if _, err := tx.Exec(ctx, migration.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES ($1)`, migration.version.Number()); err != nil {
			t.Fatal(err)
		}
	}
	baseline := schemaPlanSHA256(migrations[:27])
	const restore = `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan) VALUES(true,1,27,$1,$1,$1,$1,27,$2,$2) ON CONFLICT(singleton) DO UPDATE SET singleton=true,format=1,schema_version=27,release_id=EXCLUDED.release_id,minimum_reader=EXCLUDED.minimum_reader,minimum_writer=EXCLUDED.minimum_writer,maximum_release=EXCLUDED.maximum_release,baseline_schema=27,baseline_plan=EXCLUDED.baseline_plan,migration_plan=EXCLUDED.migration_plan`
	if _, err := tx.Exec(ctx, restore, prior27SchemaRelease, baseline[:]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	epoch := nha030Epoch(t)
	if _, outcome, err := store.OpenRecorderEpoch(ctx, epoch, time.Now()); err != nil || outcome != TransactionCommitted {
		t.Fatal(err)
	}
	oldValue := domain.NewSwitchValue(false)
	old := nha105SQLRecord(t, "nhf010-historical-switch", 1, &oldValue)
	batch, err := NewRecorderBatch(epoch, 0, 1, []RecorderRecord{old}, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha105PersistHistoricalBatch(t, store, batch)
	if _, err := store.pool.Exec(ctx, `UPDATE public.state_history SET value_kind=12 WHERE sequence=1`); err == nil {
		t.Fatal("SQL27 admitted environment kind")
	} else {
		nha151SQLState(t, err, "23514")
	}
	for _, damage := range []string{
		`UPDATE public.homeward_schema_window SET minimum_reader='unknown-release'`,
		`UPDATE public.homeward_schema_window SET migration_plan=sha256('damaged'::bytea)`,
		`DELETE FROM public.homeward_schema_window`,
	} {
		if _, err := store.pool.Exec(ctx, damage); err != nil {
			t.Fatal(err)
		}
		var classified *Error
		if _, err := store.Migrate(ctx); !errors.As(err, &classified) || classified.Kind() != InconsistentSchema {
			t.Fatal("damaged SQL27 window upgraded", err)
		}
		var version int
		if err := store.pool.QueryRow(ctx, `SELECT max(version) FROM public.homeward_schema_migrations`).Scan(&version); err != nil || version != 27 {
			t.Fatal("refused migration changed history", err)
		}
		if _, err := store.pool.Exec(ctx, restore, prior27SchemaRelease, baseline[:]); err != nil {
			t.Fatal(err)
		}
	}
	if version, err := store.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatal("environment migration", err)
	}
	build := CurrentSchemaBuild()
	if build.MinimumReader() != schemaWindowBaselineRelease || build.MinimumWriter() != schemaWindowBaselineRelease || parseSchemaRelease(prior27SchemaRelease) != schemaReleaseInvalid {
		t.Fatal("old reader/writer compatibility incorrectly granted")
	}
	weather, _ := domain.NewWeatherValue(domain.WeatherParts{Temperature: &domain.Temperature{Value: 0, Unit: domain.TemperatureCelsius}})
	w, _ := domain.NewWeatherStateValue(weather)
	person, _ := domain.ParsePersonID("synthetic-person")
	presence, _ := domain.NewPresenceValue(domain.PresenceParts{Person: person, Residence: domain.ResidenceUnknown, Confidence: domain.PresenceConfidenceUnknown})
	p, _ := domain.NewPresenceStateValue(presence)
	details, _ := domain.NewPresenceDetailsValue(domain.PresenceDetailsParts{Person: person, Coordinates: &domain.Coordinates{}})
	d, _ := domain.NewPresenceDetailsStateValue(details)
	rows := []RecorderRecord{nha105SQLRecord(t, "nhf010-weather", 2, &w), nha105SQLRecord(t, "nhf010-presence", 3, &p), nha105SQLRecord(t, "nhf010-private-details", 4, &d)}
	newBatch, err := NewRecorderBatch(epoch, 1, 4, rows, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	nha105PersistHistoricalBatch(t, store, newBatch)
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE value_kind IN(12,13,14) AND measurement_value IS NULL AND measurement_unit IS NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatal("environment became numeric measurements", err)
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
		var kind int16
		if err := reopened.pool.QueryRow(ctx, `SELECT payload,payload_digest,record_digest,value_kind FROM public.state_history WHERE epoch=$1 AND sequence=$2`, epoch.data(), row.sequence).Scan(&payload, &digest, &recordDigest, &kind); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, row.observed.payload) || !bytes.Equal(digest, row.observed.payloadDigest[:]) || !bytes.Equal(recordDigest, row.digest[:]) {
			t.Fatal("history bytes changed across migration/reopen")
		}
		var wire statewire.ObservedState
		if err := proto.Unmarshal(payload, &wire); err != nil {
			t.Fatal(err)
		}
		if rawKind := historicalRawKind(&wire); rawKind == nil || *rawKind != kind {
			t.Fatal("historical typed kind mismatch")
		}
		switch kind {
		case 1:
			if wire.GetSwitchOn() {
				t.Fatal("historical false changed")
			}
		case 12:
			if value, err := contracts.AdmitWireWeatherValue(wire.GetWeather()); err != nil {
				t.Fatal(err)
			} else {
				parts, _ := value.Components()
				if parts.Temperature == nil || parts.Temperature.Value != 0 {
					t.Fatal("weather zero lost")
				}
			}
		case 13:
			if _, err := contracts.AdmitWirePresenceValue(wire.GetPresence()); err != nil {
				t.Fatal(err)
			}
		case 14:
			if value, err := contracts.AdmitWirePresenceDetailsValue(wire.GetPresenceDetails()); err != nil {
				t.Fatal(err)
			} else {
				parts, _ := value.Components()
				if parts.Coordinates == nil || parts.Coordinates.LatitudeDegrees != 0 || parts.Coordinates.LongitudeDegrees != 0 {
					t.Fatal("private zero coordinates lost")
				}
			}
		default:
			t.Fatal("unexpected stored kind")
		}
	}
}
