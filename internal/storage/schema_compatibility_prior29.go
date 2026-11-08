package storage

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/jackc/pgx/v5"
)

// This is the exact delivered SQL29 baseline, outside the current release
// parser. Its reader cannot interpret environment values and must refuse SQL30.
const prior29SchemaRelease = "homeward-window-29-30-baseline"

type prior29SchemaWindow struct {
	singleton                        bool
	format, schema, baseline         int64
	release, reader, writer, maximum string
	baselinePlan, plan               []byte
}

func (w prior29SchemaWindow) validFor(plan [sha256.Size]byte) bool {
	return plan != ([sha256.Size]byte{}) && w.singleton && w.format == 1 && w.schema == 29 && w.baseline == 29 &&
		w.release == prior29SchemaRelease && w.reader == prior29SchemaRelease && w.writer == prior29SchemaRelease && w.maximum == prior29SchemaRelease &&
		bytes.Equal(w.baselinePlan, plan[:]) && bytes.Equal(w.plan, plan[:])
}

// Authenticate the complete released window before applying SQL30, without
// granting the predecessor reader access to new environment values.
func validatePrior29SchemaWindow(ctx context.Context, tx pgx.Tx, current Version) error {
	if current.number != 29 {
		return nil
	}
	failure := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return &Error{kind: InconsistentSchema}
	}
	build := CurrentSchemaBuild()
	if !build.Valid() || build.version != latestVersion || build.release != compiledRelease() ||
		build.profile.baseline.number != schemaWindowBaseline || build.profile.successor.number != schemaWindowSuccessor || len(migrations) < int(schemaWindowBaseline) {
		return failure()
	}
	plan := schemaPlanSHA256(migrations[:29])
	rows, err := tx.Query(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`)
	if err != nil {
		return failure()
	}
	defer rows.Close()
	if !rows.Next() {
		return failure()
	}
	var window prior29SchemaWindow
	if err := rows.Scan(&window.singleton, &window.format, &window.schema, &window.release, &window.reader, &window.writer, &window.maximum, &window.baseline, &window.baselinePlan, &window.plan); err != nil {
		return failure()
	}
	if rows.Next() || rows.Err() != nil || !window.validFor(plan) {
		return failure()
	}
	return nil
}
