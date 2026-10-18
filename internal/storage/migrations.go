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
	ConfigInvalid
	ConfigConflict
	ConfigNotFound
	ConfigIncomplete
	ConfigIntegrity
	ConfigTransactionClosed
	SecretInvalid
	SecretUnavailable
	SecretKeyMismatch
	SecretIntegrity
	SecretNotFound
	SecretCommitUnknown
)

// Error deliberately excludes driver and connection details from its diagnostic.
type Error struct {
	kind  Kind
	found Version
	cause error
}

func (e *Error) Kind() Kind {
	if e == nil {
		return 0
	}
	return e.kind
}
func (e *Error) FoundVersion() Version {
	if e == nil {
		return Version{}
	}
	return e.found
}
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *Error) GoString() string { return e.Error() }
func (e *Error) Error() string {
	if e == nil {
		return "postgres: unknown storage failure"
	}
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
	case ConfigInvalid:
		return "postgres: invalid configuration input"
	case ConfigConflict:
		return "postgres: configuration conflict"
	case ConfigNotFound:
		return "postgres: configuration record not found"
	case ConfigIncomplete:
		return "postgres: historical configuration is incomplete"
	case ConfigIntegrity:
		return "postgres: configuration integrity failure"
	case ConfigTransactionClosed:
		return "postgres: configuration transaction is closed"
	case SecretInvalid:
		return "secrets: invalid input"
	case SecretUnavailable:
		return "secrets: unavailable"
	case SecretKeyMismatch:
		return "secrets: persistent key is missing or does not match the store"
	case SecretIntegrity:
		return "secrets: integrity failure"
	case SecretNotFound:
		return "secrets: handle not found"
	case SecretCommitUnknown:
		return "secrets: commit outcome unknown"
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
`}, {version: Version{4}, sql: `
CREATE TYPE public.config_document_kind AS ENUM ('yaml_source');
CREATE TYPE public.config_schema_version AS ENUM ('ha-declarative-1');
CREATE TYPE public.config_syntax_profile AS ENUM ('ha-2026.10/declarative-1');
CREATE TABLE public.config_documents (
    id text PRIMARY KEY CHECK (id <> '' AND octet_length(id) <= 255),
    logical_path text NOT NULL UNIQUE CHECK (logical_path <> '' AND octet_length(logical_path) <= 4096),
    kind public.config_document_kind NOT NULL,
    last_revision bigint NOT NULL DEFAULT 0 CHECK (last_revision >= 0)
);
CREATE TABLE public.config_versions (
    document_id text NOT NULL REFERENCES public.config_documents(id),
    revision bigint NOT NULL CHECK (revision > 0),
    parent_revision bigint CHECK (parent_revision > 0 AND parent_revision < revision),
    source_hash bytea NOT NULL CHECK (octet_length(source_hash) = 32),
    author text NOT NULL CHECK (author <> '' AND octet_length(author) <= 255),
    source text NOT NULL CHECK (source <> '' AND octet_length(source) <= 255),
    schema_version public.config_schema_version NOT NULL,
    syntax_profile public.config_syntax_profile NOT NULL,
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    yaml_source bytea NOT NULL CHECK (octet_length(yaml_source) <= 1048576),
    CONSTRAINT config_version_source_hash_matches_bytes CHECK (source_hash = sha256(yaml_source)),
    PRIMARY KEY (document_id, revision)
);
CREATE TABLE public.config_generations (
    revision bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY CHECK (revision > 0),
    entry_path text NOT NULL CHECK (entry_path <> ''),
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash) = 32),
    manifest_hash bytea NOT NULL CHECK (octet_length(manifest_hash) = 32),
    entry_count integer NOT NULL CHECK (entry_count > 0 AND entry_count <= 256),
    sealed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE public.config_generation_entries (
    generation_revision bigint NOT NULL REFERENCES public.config_generations(revision),
    document_id text NOT NULL REFERENCES public.config_documents(id),
    version_revision bigint NOT NULL CHECK (version_revision > 0),
    source_hash bytea NOT NULL CHECK (octet_length(source_hash) = 32),
    PRIMARY KEY (generation_revision, document_id)
);
CREATE TABLE public.config_active (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    generation_revision bigint REFERENCES public.config_generations(revision),
    activated_at timestamptz,
    CHECK ((generation_revision IS NULL) = (activated_at IS NULL))
);
INSERT INTO public.config_active(singleton) VALUES (true);
CREATE FUNCTION public.homeward_config_guard_document() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'config document identity and revision are protected' USING ERRCODE = '23514';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id OR
       NEW.logical_path IS DISTINCT FROM OLD.logical_path OR
       NEW.kind IS DISTINCT FROM OLD.kind OR
       NEW.last_revision IS DISTINCT FROM OLD.last_revision + 1 THEN
        RAISE EXCEPTION 'config document identity and revision are protected' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER config_document_identity BEFORE UPDATE OR DELETE ON public.config_documents
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_document();
CREATE FUNCTION public.homeward_config_guard_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'config version is immutable' USING ERRCODE = '23514';
    END IF;
    PERFORM pg_advisory_xact_lock(481316, 151);
    IF current_setting('homeward.config_gc', true) IS DISTINCT FROM 'on' THEN
        RAISE EXCEPTION 'config version deletion requires collection' USING ERRCODE = '23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.config_active a
        JOIN public.config_generation_entries e ON e.generation_revision = a.generation_revision
        WHERE a.singleton AND e.document_id = OLD.document_id AND e.version_revision = OLD.revision
    ) THEN
        RAISE EXCEPTION 'active config version is protected' USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
END
$$;
CREATE TRIGGER config_version_immutable BEFORE UPDATE OR DELETE ON public.config_versions
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_version();
CREATE FUNCTION public.homeward_config_guard_version_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE allocated bigint;
BEGIN
    SELECT last_revision INTO allocated FROM public.config_documents WHERE id = NEW.document_id;
    IF NEW.revision IS DISTINCT FROM allocated OR
       (NEW.revision = 1 AND NEW.parent_revision IS NOT NULL) OR
       (NEW.revision > 1 AND NEW.parent_revision IS DISTINCT FROM NEW.revision - 1) THEN
        RAISE EXCEPTION 'config revision was not allocated' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER config_version_allocated BEFORE INSERT ON public.config_versions
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_version_insert();
CREATE FUNCTION public.homeward_config_guard_manifest() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_count integer;
DECLARE source_bytes bigint;
BEGIN
    IF TG_TABLE_NAME = 'config_generations' THEN
      IF TG_OP = 'UPDATE' THEN
       IF NOT OLD.sealed AND NEW.sealed AND
          NEW.revision IS NOT DISTINCT FROM OLD.revision AND
          NEW.entry_path IS NOT DISTINCT FROM OLD.entry_path AND
          NEW.candidate_hash IS NOT DISTINCT FROM OLD.candidate_hash AND
          NEW.manifest_hash IS NOT DISTINCT FROM OLD.manifest_hash AND
          NEW.entry_count IS NOT DISTINCT FROM OLD.entry_count AND
          NEW.created_at IS NOT DISTINCT FROM OLD.created_at THEN
        PERFORM pg_advisory_xact_lock(481316, 151);
        SELECT count(*), COALESCE(sum(octet_length(v.yaml_source)),0)
          INTO actual_count, source_bytes
          FROM public.config_generation_entries e
          LEFT JOIN public.config_versions v
            ON v.document_id=e.document_id AND v.revision=e.version_revision
          WHERE e.generation_revision=OLD.revision;
        IF actual_count <> OLD.entry_count OR source_bytes > 4194304 OR EXISTS (
            SELECT 1 FROM public.config_generation_entries e
            LEFT JOIN public.config_versions v
              ON v.document_id=e.document_id AND v.revision=e.version_revision
            WHERE e.generation_revision=OLD.revision
              AND (v.revision IS NULL OR v.source_hash <> e.source_hash)
        ) THEN
            RAISE EXCEPTION 'config manifest cannot be sealed' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
       END IF;
      END IF;
    END IF;
    RAISE EXCEPTION 'config manifest is immutable' USING ERRCODE = '23514';
END
$$;
CREATE TRIGGER config_generation_immutable BEFORE UPDATE OR DELETE ON public.config_generations
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_manifest();
CREATE TRIGGER config_entry_immutable BEFORE UPDATE OR DELETE ON public.config_generation_entries
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_manifest();
CREATE FUNCTION public.homeward_config_guard_entry_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE is_sealed boolean;
BEGIN
    PERFORM pg_advisory_xact_lock(481316, 151);
    SELECT sealed INTO is_sealed FROM public.config_generations
      WHERE revision = NEW.generation_revision FOR UPDATE;
    IF is_sealed IS DISTINCT FROM false THEN
        RAISE EXCEPTION 'config manifest is sealed' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER config_entry_sealed BEFORE INSERT ON public.config_generation_entries
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_entry_insert();
CREATE FUNCTION public.homeward_config_guard_active() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_count integer;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'active config pointer cannot be removed' USING ERRCODE = '23514';
    END IF;
    PERFORM pg_advisory_xact_lock(481316, 151);
    IF OLD.generation_revision IS NOT NULL AND
       (NEW.generation_revision IS NULL OR NEW.generation_revision <= OLD.generation_revision) THEN
        RAISE EXCEPTION 'active generation revision must advance' USING ERRCODE = '23514';
    END IF;
    IF NEW.generation_revision IS NOT NULL THEN
        SELECT count(*) INTO actual_count
        FROM public.config_generation_entries
        WHERE generation_revision = NEW.generation_revision;
        IF actual_count <> (SELECT entry_count FROM public.config_generations WHERE revision = NEW.generation_revision)
           OR NOT (SELECT sealed FROM public.config_generations WHERE revision = NEW.generation_revision)
           OR EXISTS (
               SELECT 1 FROM public.config_generation_entries e
               LEFT JOIN public.config_versions v
                 ON v.document_id = e.document_id AND v.revision = e.version_revision
               WHERE e.generation_revision = NEW.generation_revision
                 AND (v.revision IS NULL OR v.source_hash <> e.source_hash)
           ) THEN
            RAISE EXCEPTION 'active generation is incomplete' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER config_active_integrity BEFORE UPDATE OR DELETE ON public.config_active
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_active();
CREATE FUNCTION public.homeward_config_no_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'active config pointer is a singleton' USING ERRCODE = '23514';
END
$$;
CREATE TRIGGER config_active_no_insert BEFORE INSERT ON public.config_active
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_no_insert();
CREATE FUNCTION public.homeward_config_no_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'config history cannot be truncated' USING ERRCODE = '23514';
END
$$;
CREATE TRIGGER config_documents_no_truncate BEFORE TRUNCATE ON public.config_documents
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_no_truncate();
CREATE TRIGGER config_versions_no_truncate BEFORE TRUNCATE ON public.config_versions
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_no_truncate();
CREATE TRIGGER config_generations_no_truncate BEFORE TRUNCATE ON public.config_generations
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_no_truncate();
CREATE TRIGGER config_entries_no_truncate BEFORE TRUNCATE ON public.config_generation_entries
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_no_truncate();
CREATE TRIGGER config_active_no_truncate BEFORE TRUNCATE ON public.config_active
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_no_truncate();
`}, {version: Version{5}, sql: `
CREATE TABLE public.secret_bootstrap (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    key_id bytea NOT NULL UNIQUE CHECK (octet_length(key_id) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION public.homeward_secret_bootstrap_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'secret bootstrap identity is immutable' USING ERRCODE = '23514';
END
$$;
CREATE TRIGGER secret_bootstrap_immutable BEFORE UPDATE OR DELETE ON public.secret_bootstrap
    FOR EACH ROW EXECUTE FUNCTION public.homeward_secret_bootstrap_immutable();
CREATE TRIGGER secret_bootstrap_no_truncate BEFORE TRUNCATE ON public.secret_bootstrap
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_secret_bootstrap_immutable();
CREATE TYPE public.secret_cipher_suite AS ENUM ('aes256gcm_hkdf_sha256_v1');
CREATE TABLE public.secret_salts (
    salt bytea PRIMARY KEY CHECK (octet_length(salt) = 32)
);
CREATE TRIGGER secret_salts_no_delete BEFORE DELETE OR TRUNCATE ON public.secret_salts
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_secret_bootstrap_immutable();
CREATE TABLE public.integration_secrets (
    instance_id text NOT NULL CHECK (instance_id <> ''),
    secret_name text NOT NULL CHECK (secret_name <> '' AND octet_length(secret_name) <= 256),
    revision bigint NOT NULL CHECK (revision > 0),
    key_id bytea NOT NULL REFERENCES public.secret_bootstrap(key_id),
    suite public.secret_cipher_suite NOT NULL,
    salt bytea NOT NULL UNIQUE REFERENCES public.secret_salts(salt),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) >= 28 AND octet_length(ciphertext) <= 1048604),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, secret_name)
);
CREATE TYPE public.secret_audit_reason AS ENUM ('foreign_handle', 'inactive_session');
CREATE TABLE public.secret_access_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    requesting_instance text NOT NULL CHECK (requesting_instance <> ''),
    target_instance text NOT NULL CHECK (target_instance <> ''),
    reason public.secret_audit_reason NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
`}, {version: Version{6}, sql: `
-- A discovery topic is only provenance for an already configured source key.
-- It never creates an identity and remains after retirement so a retained
-- empty payload can be understood after a cold restart.
CREATE TABLE public.registry_mqtt_discovery_topics (
    instance_id text NOT NULL CHECK (instance_id <> ''),
    discovery_topic text NOT NULL CHECK (discovery_topic <> '' AND octet_length(discovery_topic) <= 256),
    source_device text NOT NULL CHECK (source_device <> ''),
    source_entity text NOT NULL CHECK (source_entity <> ''),
    PRIMARY KEY (instance_id, source_device, source_entity),
    FOREIGN KEY (instance_id, source_device, source_entity)
        REFERENCES public.registry_source_bindings(instance_id, source_device, source_entity)
);
CREATE INDEX registry_mqtt_discovery_by_topic
    ON public.registry_mqtt_discovery_topics(instance_id, discovery_topic);
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
