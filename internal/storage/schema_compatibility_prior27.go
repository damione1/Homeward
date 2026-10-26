package storage

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/jackc/pgx/v5"
)

// This is the exact delivered SQL27 baseline, outside the current release
// parser. Its reader cannot interpret environment values and must refuse SQL28.
const prior27SchemaRelease = "homeward-window-27-28-baseline"

type prior27SchemaWindow struct {
	singleton                        bool
	format, schema, baseline         int64
	release, reader, writer, maximum string
	baselinePlan, plan               []byte
}

func (w prior27SchemaWindow) validFor(plan [sha256.Size]byte) bool {
	return plan != ([sha256.Size]byte{}) && w.singleton && w.format == 1 && w.schema == 27 && w.baseline == 27 &&
		w.release == prior27SchemaRelease && w.reader == prior27SchemaRelease && w.writer == prior27SchemaRelease && w.maximum == prior27SchemaRelease &&
		bytes.Equal(w.baselinePlan, plan[:]) && bytes.Equal(w.plan, plan[:])
}

// Authenticate the complete released window before applying SQL28, without
// granting the predecessor reader access to new environment values.
func validatePrior27SchemaWindow(ctx context.Context, tx pgx.Tx, current Version) error {
	if current.number != 27 {
		return nil
	}
	failure := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return &Error{kind: InconsistentSchema}
	}
	build := CurrentSchemaBuild()
	if !build.Valid() || build.version.number != schemaWindowBaseline || build.release != schemaReleaseBaseline ||
		build.profile.baseline.number != schemaWindowBaseline || build.profile.successor.number != schemaWindowSuccessor || len(migrations) < int(schemaWindowBaseline) {
		return failure()
	}
	plan := schemaPlanSHA256(migrations[:27])
	rows, err := tx.Query(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`)
	if err != nil {
		return failure()
	}
	defer rows.Close()
	if !rows.Next() {
		return failure()
	}
	var window prior27SchemaWindow
	if err := rows.Scan(&window.singleton, &window.format, &window.schema, &window.release, &window.reader, &window.writer, &window.maximum, &window.baseline, &window.baselinePlan, &window.plan); err != nil {
		return failure()
	}
	if rows.Next() || rows.Err() != nil || !window.validFor(plan) {
		return failure()
	}
	return nil
}
