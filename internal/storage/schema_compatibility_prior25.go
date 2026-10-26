package storage

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/jackc/pgx/v5"
)

// This is a fixed upgrade bridge from the delivered SQL25 release, not another
// runtime profile or an admission of its readers on SQL27. Keep the old release
// outside parseSchemaRelease: an old process must refuse the native27 window.
const prior25SchemaRelease = "homeward-window-25-26-baseline"

type prior25SchemaWindow struct {
	singleton                        bool
	format, schema, baseline         int64
	release, reader, writer, maximum string
	baselinePlan, plan               []byte
}

func (w prior25SchemaWindow) validFor(plan [sha256.Size]byte) bool {
	return plan != ([sha256.Size]byte{}) && w.singleton && w.format == 1 && w.schema == 25 && w.baseline == 25 &&
		w.release == prior25SchemaRelease && w.reader == prior25SchemaRelease &&
		w.writer == prior25SchemaRelease && w.maximum == prior25SchemaRelease &&
		bytes.Equal(w.baselinePlan, plan[:]) && bytes.Equal(w.plan, plan[:])
}

// The caller holds the migration transaction advisory lock. A missing or
// damaged old declaration is never overwritten by a fresh release27 window.
func validatePrior25SchemaWindow(ctx context.Context, tx pgx.Tx, current Version) error {
	if current.number != 25 {
		return nil
	}
	failure := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return &Error{kind: InconsistentSchema}
	}
	build := CurrentSchemaBuild()
	validRelease := build.version.number == 27 && build.release == schemaReleaseBaseline
	if !build.Valid() || !validRelease ||
		build.profile.baseline.number != 27 || build.profile.successor.number != 28 || len(migrations) < 27 {
		return failure()
	}
	// Historical SQL must stay byte-identical to the actual released prefix.
	// Both digests in the prior row must authenticate that same complete prefix.
	plan := schemaPlanSHA256(migrations[:25])
	rows, err := tx.Query(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`)
	if err != nil {
		return failure()
	}
	defer rows.Close()
	if !rows.Next() {
		return failure()
	}
	var window prior25SchemaWindow
	if err := rows.Scan(&window.singleton, &window.format, &window.schema, &window.release, &window.reader, &window.writer, &window.maximum, &window.baseline, &window.baselinePlan, &window.plan); err != nil {
		return failure()
	}
	if rows.Next() || rows.Err() != nil || !window.validFor(plan) {
		return failure()
	}
	return nil
}
