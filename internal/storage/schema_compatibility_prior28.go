package storage

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/jackc/pgx/v5"
)

// This is the exact delivered SQL28 baseline, outside the current release
// parser. Its reader cannot interpret environment values and must refuse SQL29.
const prior28SchemaRelease = "homeward-window-28-29-baseline"

type prior28SchemaWindow struct {
	singleton                        bool
	format, schema, baseline         int64
	release, reader, writer, maximum string
	baselinePlan, plan               []byte
}

func (w prior28SchemaWindow) validFor(plan [sha256.Size]byte) bool {
	return plan != ([sha256.Size]byte{}) && w.singleton && w.format == 1 && w.schema == 28 && w.baseline == 28 &&
		w.release == prior28SchemaRelease && w.reader == prior28SchemaRelease && w.writer == prior28SchemaRelease && w.maximum == prior28SchemaRelease &&
		bytes.Equal(w.baselinePlan, plan[:]) && bytes.Equal(w.plan, plan[:])
}

// Authenticate the complete released window before applying SQL29, without
// granting the predecessor reader access to new environment values.
func validatePrior28SchemaWindow(ctx context.Context, tx pgx.Tx, current Version) error {
	if current.number != 28 {
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
	plan := schemaPlanSHA256(migrations[:28])
	rows, err := tx.Query(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`)
	if err != nil {
		return failure()
	}
	defer rows.Close()
	if !rows.Next() {
		return failure()
	}
	var window prior28SchemaWindow
	if err := rows.Scan(&window.singleton, &window.format, &window.schema, &window.release, &window.reader, &window.writer, &window.maximum, &window.baseline, &window.baselinePlan, &window.plan); err != nil {
		return failure()
	}
	if rows.Next() || rows.Err() != nil || !window.validFor(plan) {
		return failure()
	}
	return nil
}
