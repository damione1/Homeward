package storage

import (
	"bytes"
	"context"
	"github.com/jackc/pgx/v5"
)

// The delivered calendar release used baseline29/successor30. Authenticate
// that exact window solely for migration, never by the runtime release parser.
func validatePrior30SchemaWindow(ctx context.Context, tx pgx.Tx, current Version) error {
	if current.number != 30 {
		return nil
	}
	if schemaAdmissionInTx(ctx, tx, current) == schemaAdmissionCurrent {
		return nil
	}
	var singleton bool
	var format, schema, baseline int64
	var release, reader, writer, maximum string
	var baselinePlan, plan []byte
	rows, err := tx.Query(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`)
	if err != nil {
		return &Error{kind: InconsistentSchema}
	}
	defer rows.Close()
	if !rows.Next() {
		return &Error{kind: InconsistentSchema}
	}
	err = rows.Scan(&singleton, &format, &schema, &release, &reader, &writer, &maximum, &baseline, &baselinePlan, &plan)
	expectedBase, expectedPlan := schemaPlanSHA256(migrations[:29]), schemaPlanSHA256(migrations[:30])
	const old = "homeward-window-29-30-successor"
	if err != nil || rows.Next() || rows.Err() != nil || !singleton || format != 1 || schema != 30 || baseline != 29 || release != old || reader != old || writer != old || maximum != old || !bytes.Equal(baselinePlan, expectedBase[:]) || !bytes.Equal(plan, expectedPlan[:]) {
		return &Error{kind: InconsistentSchema}
	}
	return nil
}
