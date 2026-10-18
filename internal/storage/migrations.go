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
	CommandRuntimeBusy
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
	case CommandRuntimeBusy:
		return "postgres: durable command runtime already active"
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
)`}, {version: Version{2}, sql: `
CREATE TABLE public.registry_devices (
    id text PRIMARY KEY CHECK (id <> '')
);
CREATE TABLE public.registry_device_sources (
    instance_id text NOT NULL CHECK (instance_id <> ''),
    source_device text NOT NULL CHECK (source_device <> ''),
    device_id text NOT NULL UNIQUE REFERENCES public.registry_devices(id),
    PRIMARY KEY (instance_id, source_device)
);
CREATE TABLE public.registry_entities (
    id text PRIMARY KEY CHECK (id <> ''),
    device_id text NOT NULL REFERENCES public.registry_devices(id),
    manifest_alias text NOT NULL UNIQUE CHECK (manifest_alias <> ''),
    tombstoned boolean NOT NULL DEFAULT false
);
CREATE TABLE public.registry_source_bindings (
    instance_id text NOT NULL CHECK (instance_id <> ''),
    source_device text NOT NULL CHECK (source_device <> ''),
    source_entity text NOT NULL CHECK (source_entity <> ''),
    entity_id text NOT NULL UNIQUE REFERENCES public.registry_entities(id),
    PRIMARY KEY (instance_id, source_device, source_entity),
    FOREIGN KEY (instance_id, source_device)
        REFERENCES public.registry_device_sources(instance_id, source_device)
);
CREATE TABLE public.registry_aliases (
    alias text PRIMARY KEY CHECK (alias <> ''),
    entity_id text NOT NULL REFERENCES public.registry_entities(id),
    current boolean NOT NULL
);
CREATE UNIQUE INDEX registry_one_current_alias_per_entity
    ON public.registry_aliases(entity_id) WHERE current
`}, {version: Version{3}, sql: `
CREATE TYPE public.command_intent_state AS ENUM ('accepted', 'dispatching', 'result', 'indeterminate');
CREATE TYPE public.command_result_error AS ENUM ('none', 'route_busy', 'route_unavailable', 'canceled', 'deadline', 'unknown', 'indeterminate');
CREATE TABLE public.command_intents (
    command_id text PRIMARY KEY CHECK (command_id <> ''),
    actor_id text NOT NULL CHECK (actor_id <> ''),
    admission_epoch text NOT NULL CHECK (admission_epoch <> ''),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) > 0),
    state public.command_intent_state NOT NULL,
    result_error public.command_result_error NOT NULL DEFAULT 'none',
    response bytea,
    accepted_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state IN ('accepted', 'dispatching') AND response IS NULL AND result_error = 'none') OR
           (state IN ('result', 'indeterminate') AND response IS NOT NULL))
);
CREATE INDEX command_intents_dispatching ON public.command_intents(state)
`}}

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
