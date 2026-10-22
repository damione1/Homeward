package storage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/jackc/pgx/v5"
)

type schemaRelease uint8

const (
	schemaReleaseInvalid schemaRelease = iota
	schemaReleaseBaseline
	schemaReleaseSuccessor
)

func (r schemaRelease) name() string {
	switch r {
	case schemaReleaseBaseline:
		return schemaWindowBaselineRelease
	case schemaReleaseSuccessor:
		return schemaWindowSuccessorRelease
	default:
		return ""
	}
}
func parseSchemaRelease(raw string) schemaRelease {
	switch raw {
	case schemaWindowBaselineRelease:
		return schemaReleaseBaseline
	case schemaWindowSuccessorRelease:
		return schemaReleaseSuccessor
	default:
		return schemaReleaseInvalid
	}
}

// SchemaProfile exposes only the two identities explicitly compiled into this
// build. Its zero value and unknown release inputs are not admitted.
type SchemaProfile struct {
	baseline, successor               Version
	baselineRelease, successorRelease string
}

func (p SchemaProfile) Baseline() Version        { return p.baseline }
func (p SchemaProfile) Successor() Version       { return p.successor }
func (p SchemaProfile) BaselineRelease() string  { return p.baselineRelease }
func (p SchemaProfile) SuccessorRelease() string { return p.successorRelease }
func schemaReleaseIDValid(raw string) bool {
	if len(raw) == 0 || len(raw) > 128 || raw[0] < 'a' || raw[0] > 'z' {
		return false
	}
	for _, char := range raw {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '.' {
			return false
		}
	}
	return true
}
func (p SchemaProfile) Valid() bool {
	return p == compiledSchemaProfile() && p.baseline.number > 0 && p.successor.number > p.baseline.number && p.successor.number <= (1<<31)-1 &&
		schemaReleaseIDValid(p.baselineRelease) && schemaReleaseIDValid(p.successorRelease) && p.baselineRelease != p.successorRelease
}
func compiledSchemaProfile() SchemaProfile {
	return SchemaProfile{baseline: Version{schemaWindowBaseline}, successor: Version{schemaWindowSuccessor}, baselineRelease: schemaWindowBaselineRelease, successorRelease: schemaWindowSuccessorRelease}
}
func compiledRelease() schemaRelease { return parseSchemaRelease(compiledSchemaReleaseID) }
func compiledReader() schemaRelease  { return parseSchemaRelease(compiledMinimumReaderID) }
func compiledWriter() schemaRelease  { return parseSchemaRelease(compiledMinimumWriterID) }

// SchemaBuild is a read-only identity of this exact compiled migrator.
type SchemaBuild struct {
	release, reader, writer schemaRelease
	version                 Version
	plan                    [sha256.Size]byte
	profile                 SchemaProfile
}

func (b SchemaBuild) Release() string               { return b.release.name() }
func (b SchemaBuild) MinimumReader() string         { return b.reader.name() }
func (b SchemaBuild) MinimumWriter() string         { return b.writer.name() }
func (b SchemaBuild) Version() Version              { return b.version }
func (b SchemaBuild) PlanSHA256() [sha256.Size]byte { return b.plan }
func (b SchemaBuild) Profile() SchemaProfile        { return b.profile }
func (b SchemaBuild) Valid() bool {
	if !b.profile.Valid() || b.release == schemaReleaseInvalid || b.reader == schemaReleaseInvalid || b.writer == schemaReleaseInvalid ||
		b.reader > b.release || b.writer > b.release || b.plan == ([sha256.Size]byte{}) {
		return false
	}
	switch b.release {
	case schemaReleaseBaseline:
		return b.version == b.profile.baseline && b.reader == schemaReleaseBaseline && b.writer == schemaReleaseBaseline
	case schemaReleaseSuccessor:
		return b.version == b.profile.successor
	default:
		return false
	}
}
func CurrentSchemaBuild() SchemaBuild {
	return SchemaBuild{release: compiledRelease(), reader: compiledReader(), writer: compiledWriter(), version: latestVersion, plan: schemaPlanSHA256(migrations), profile: compiledSchemaProfile()}
}

// Length framing prevents ambiguous SQL concatenations. SQL and its ordinal
// come exclusively from the migration authority; the digest is not an SQL
// safety analyzer. Promotion requires the executable qualification as well.
func schemaPlanSHA256(plan []migration) [sha256.Size]byte {
	hash := sha256.New()
	var frame [8]byte
	for _, step := range plan {
		binary.BigEndian.PutUint32(frame[:4], step.version.number)
		binary.BigEndian.PutUint32(frame[4:], uint32(len(step.sql)))
		_, _ = hash.Write(frame[:])
		_, _ = hash.Write([]byte(step.sql))
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

type schemaWindow struct {
	format                           int64
	schema                           int64
	release, reader, writer, maximum schemaRelease
	baseline                         int64
	baselinePlan, plan               [sha256.Size]byte
}

func readSchemaWindow(ctx context.Context, tx pgx.Tx) (schemaWindow, error) {
	var window schemaWindow
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.homeward_schema_window') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return window, errors.New("schema window absent")
	}
	var release, reader, writer, maximum string
	var baselinePlan, plan []byte
	var singleton bool
	rows, err := tx.Query(ctx, `SELECT singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan FROM public.homeward_schema_window`)
	if err != nil {
		return window, err
	}
	defer rows.Close()
	if !rows.Next() {
		return window, errors.New("schema window absent")
	}
	if err := rows.Scan(&singleton, &window.format, &window.schema, &release, &reader, &writer, &maximum, &window.baseline, &baselinePlan, &plan); err != nil {
		return window, err
	}
	if !singleton || rows.Next() || rows.Err() != nil || len(baselinePlan) != sha256.Size || len(plan) != sha256.Size {
		return window, errors.New("schema window malformed")
	}
	window.release, window.reader, window.writer, window.maximum = parseSchemaRelease(release), parseSchemaRelease(reader), parseSchemaRelease(writer), parseSchemaRelease(maximum)
	copy(window.baselinePlan[:], baselinePlan)
	copy(window.plan[:], plan)
	return window, nil
}

func (w schemaWindow) validFor(current Version) bool {
	if !compiledSchemaProfile().Valid() || w.format != 1 || w.schema != int64(current.number) || w.baseline != int64(schemaWindowBaseline) ||
		w.release == schemaReleaseInvalid || w.reader == schemaReleaseInvalid || w.writer == schemaReleaseInvalid || w.maximum == schemaReleaseInvalid ||
		w.reader > w.release || w.writer > w.release || w.maximum != w.release ||
		w.baselinePlan == ([sha256.Size]byte{}) || w.plan == ([sha256.Size]byte{}) {
		return false
	}
	switch w.release {
	case schemaReleaseBaseline:
		return current.number == schemaWindowBaseline && w.reader == schemaReleaseBaseline && w.writer == schemaReleaseBaseline && w.plan == w.baselinePlan
	case schemaReleaseSuccessor:
		return current.number == schemaWindowSuccessor && w.plan != w.baselinePlan
	default:
		return false
	}
}

type schemaAdmission uint8

const (
	schemaAdmissionRefused schemaAdmission = iota
	schemaAdmissionCurrent
	schemaAdmissionSuccessor
)

func schemaAdmissionInTx(ctx context.Context, tx pgx.Tx, current Version) schemaAdmission {
	if !CurrentSchemaBuild().Valid() {
		return schemaAdmissionRefused
	}
	window, err := readSchemaWindow(ctx, tx)
	if err != nil || !window.validFor(current) || len(migrations) < int(schemaWindowBaseline) {
		return schemaAdmissionRefused
	}
	if window.baselinePlan != schemaPlanSHA256(migrations[:schemaWindowBaseline]) {
		return schemaAdmissionRefused
	}
	if current.number == schemaWindowBaseline && compiledRelease() == schemaReleaseSuccessor && latestVersion.number == schemaWindowSuccessor {
		if window.release == schemaReleaseBaseline && window.plan == schemaPlanSHA256(migrations[:schemaWindowBaseline]) {
			return schemaAdmissionCurrent
		}
		return schemaAdmissionRefused
	}
	if current == latestVersion {
		if window.release == compiledRelease() && window.reader == compiledReader() && window.writer == compiledWriter() && window.plan == schemaPlanSHA256(migrations) {
			return schemaAdmissionCurrent
		}
		return schemaAdmissionRefused
	}
	// An explicit pair, not latest+1 tolerance: this baseline admits precisely
	// its declared successor, provided BOTH old reading and writing are kept.
	if compiledRelease() == schemaReleaseBaseline && latestVersion.number == schemaWindowBaseline &&
		window.release == schemaReleaseSuccessor && window.reader == schemaReleaseBaseline && window.writer == schemaReleaseBaseline && window.maximum == schemaReleaseSuccessor {
		return schemaAdmissionSuccessor
	}
	return schemaAdmissionRefused
}

func publishSchemaWindow(ctx context.Context, tx pgx.Tx, current Version) error {
	if current.number < schemaWindowBaseline {
		return nil
	}
	build := CurrentSchemaBuild()
	if current != build.version || len(migrations) < int(schemaWindowBaseline) || !build.Valid() {
		return &Error{kind: InconsistentSchema}
	}
	baseline := schemaPlanSHA256(migrations[:schemaWindowBaseline])
	window := schemaWindow{format: 1, schema: int64(current.number), release: compiledRelease(), reader: compiledReader(), writer: compiledWriter(), maximum: compiledRelease(), baseline: int64(schemaWindowBaseline), baselinePlan: baseline, plan: build.plan}
	if !window.validFor(current) {
		return &Error{kind: InconsistentSchema}
	}
	_, err := tx.Exec(ctx, `INSERT INTO public.homeward_schema_window(singleton,format,schema_version,release_id,minimum_reader,minimum_writer,maximum_release,baseline_schema,baseline_plan,migration_plan)
        VALUES (true,1,$1,$2,$3,$4,$2,$5,$6,$7)
        ON CONFLICT (singleton) DO UPDATE SET format=EXCLUDED.format,schema_version=EXCLUDED.schema_version,release_id=EXCLUDED.release_id,minimum_reader=EXCLUDED.minimum_reader,minimum_writer=EXCLUDED.minimum_writer,maximum_release=EXCLUDED.maximum_release,baseline_schema=EXCLUDED.baseline_schema,baseline_plan=EXCLUDED.baseline_plan,migration_plan=EXCLUDED.migration_plan`, current.number, build.Release(), build.MinimumReader(), build.MinimumWriter(), schemaWindowBaseline, baseline[:], build.plan[:])
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{kind: MigrationFailed}
	}
	return nil
}
