package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Version is a schema version read from the database or declared by a migration.
// Its zero value denotes an empty, unmigrated database.
type Version struct{ number uint32 }

func (v Version) Number() uint32 { return v.number }

type Kind uint8

const (
	InvalidSettings Kind = iota + 1
	InvalidContext
	Unavailable
	FutureSchema
	InconsistentSchema
	MigrationFailed
	TransactionFailed
)

// Error deliberately excludes driver and connection details from its diagnostic.
type Error struct {
	kind  Kind
	found Version
	cause error
}

func (e *Error) Kind() Kind            { return e.kind }
func (e *Error) FoundVersion() Version { return e.found }
func (e *Error) Unwrap() error         { return e.cause }
func (e *Error) GoString() string      { return e.Error() }
func (e *Error) Error() string {
	switch e.kind {
	case InvalidSettings:
		return "postgres: invalid explicit connection settings"
	case InvalidContext:
		return "postgres: missing context"
	case Unavailable:
		return "postgres: unavailable"
	case FutureSchema:
		return fmt.Sprintf("postgres: schema version %d is newer than supported version %d", e.found.number, latestVersion.number)
	case InconsistentSchema:
		return "postgres: inconsistent migration history"
	case MigrationFailed:
		return "postgres: migration failed and rolled back"
	case TransactionFailed:
		return "postgres: transaction failed and rolled back"
	default:
		return "postgres: unknown storage failure"
	}
}

type migration struct {
	version Version
	sql     string
}

// The migration list is the only schema authority. Module tables join it in later tickets.
var migrations = []migration{{version: Version{1}, sql: `
CREATE TABLE public.homeward_schema_migrations (
    version integer PRIMARY KEY CHECK (version > 0),
    applied_at timestamptz NOT NULL DEFAULT now()
)`}}

var latestVersion = migrations[len(migrations)-1].version

func CurrentVersion() Version { return latestVersion }

// Migrate serializes concurrent starters and commits DDL and history together.
func (s *Store) Migrate(ctx context.Context) (Version, error) { return s.migrate(ctx, migrations) }

func (s *Store) migrate(ctx context.Context, plan []migration) (Version, error) {
	if ctx == nil {
		return Version{}, &Error{kind: InvalidContext}
	}
	if s == nil || s.pool == nil {
		return Version{}, &Error{kind: InvalidSettings}
	}
	if err := ctx.Err(); err != nil {
		return Version{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return Version{}, ctx.Err()
		}
		return Version{}, &Error{kind: Unavailable}
	}
	defer rollback(tx)
	// Fixed application key, transaction scoped. Lock is released at commit/rollback.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 27)`); err != nil {
		if ctx.Err() != nil {
			return Version{}, ctx.Err()
		}
		return Version{}, &Error{kind: MigrationFailed}
	}
	current, err := versionInTx(ctx, tx)
	if err != nil {
		if ctx.Err() != nil {
			return Version{}, ctx.Err()
		}
		return Version{}, err
	}
	if current.number > latestVersion.number {
		return Version{}, &Error{kind: FutureSchema, found: current}
	}
	for _, m := range plan {
		if m.version.number <= current.number {
			continue
		}
		if m.version.number != current.number+1 {
			return Version{}, &Error{kind: InconsistentSchema}
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			if ctx.Err() != nil {
				return Version{}, ctx.Err()
			}
			return Version{}, &Error{kind: MigrationFailed}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES ($1)`, m.version.number); err != nil {
			if ctx.Err() != nil {
				return Version{}, ctx.Err()
			}
			return Version{}, &Error{kind: MigrationFailed}
		}
		current = m.version
	}
	if err := tx.Commit(ctx); err != nil {
		if ctx.Err() != nil {
			return Version{}, ctx.Err()
		}
		return Version{}, &Error{kind: MigrationFailed}
	}
	return current, nil
}

func versionInTx(ctx context.Context, tx pgx.Tx) (Version, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.homeward_schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return Version{}, &Error{kind: MigrationFailed}
	}
	if !exists {
		return Version{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT version FROM public.homeward_schema_migrations ORDER BY version`)
	if err != nil {
		return Version{}, &Error{kind: InconsistentSchema}
	}
	defer rows.Close()
	var current Version
	for rows.Next() {
		var number int64
		if err := rows.Scan(&number); err != nil || number != int64(current.number)+1 || number > int64(^uint32(0)) {
			return Version{}, &Error{kind: InconsistentSchema}
		}
		current = Version{uint32(number)}
	}
	if rows.Err() != nil {
		return Version{}, &Error{kind: InconsistentSchema}
	}
	return current, nil
}
