package storage

// This table declares a reader AND writer window, independently of SQL ordinals.
// Its row is published with the migration history under the migrator's lock.
const schemaCompatibilitySchemaSQL = `
CREATE TABLE public.homeward_schema_window (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    format integer NOT NULL CHECK (format > 0),
    schema_version integer NOT NULL CHECK (schema_version > 0),
    release_id text NOT NULL CHECK (release_id <> ''),
    minimum_reader text NOT NULL CHECK (minimum_reader <> ''),
    minimum_writer text NOT NULL CHECK (minimum_writer <> ''),
    maximum_release text NOT NULL CHECK (maximum_release <> ''),
    baseline_schema integer NOT NULL CHECK (baseline_schema > 0),
    baseline_plan bytea NOT NULL CHECK (octet_length(baseline_plan) = 32),
    migration_plan bytea NOT NULL CHECK (octet_length(migration_plan) = 32)
);
`
