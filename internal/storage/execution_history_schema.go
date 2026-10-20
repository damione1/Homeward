package storage

// Execution history has its own seven-day retention domain. The command ledger,
// registry, audit and recorder tables are intentionally absent from its FKs.
const executionHistorySchemaSQL = `
CREATE TABLE public.execution_history_head (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    current_epoch text,
    commit_sequence bigint NOT NULL DEFAULT 0 CHECK (commit_sequence >= 0),
    retention_cutoff timestamptz NOT NULL DEFAULT '2000-01-01 00:00:00+00'::timestamptz
);
INSERT INTO public.execution_history_head(singleton) VALUES (true);
CREATE TABLE public.execution_history_epochs (
    epoch text PRIMARY KEY CHECK (epoch ~ '^[0-9a-f]{32}$'),
    previous_epoch text,
    previous_checkpoint bigint NOT NULL DEFAULT 0 CHECK (previous_checkpoint >= 0),
    checkpoint bigint NOT NULL DEFAULT 0 CHECK (checkpoint >= 0),
    opened_at timestamptz NOT NULL DEFAULT now(),
    interrupted_predecessor boolean NOT NULL DEFAULT false
);
CREATE TABLE public.execution_history_batches (
    epoch text NOT NULL REFERENCES public.execution_history_epochs(epoch),
    batch_number bigint NOT NULL CHECK (batch_number > 0),
    digest bytea NOT NULL CHECK (octet_length(digest) = 32),
    inserted_at timestamptz NOT NULL DEFAULT now(),
    retention_time timestamptz NOT NULL,
    PRIMARY KEY(epoch, batch_number)
);
CREATE TABLE public.execution_history_records (
    epoch text NOT NULL REFERENCES public.execution_history_epochs(epoch),
    ordinal bigint NOT NULL CHECK (ordinal > 0),
    emission_time timestamptz NOT NULL,
    inserted_at timestamptz NOT NULL DEFAULT now(),
    commit_sequence bigint NOT NULL CHECK (commit_sequence > 0),
    kind smallint NOT NULL CHECK (kind BETWEEN 1 AND 6),
    decision smallint CHECK (decision BETWEEN 1 AND 18),
    node_stage smallint CHECK (node_stage BETWEEN 1 AND 6),
    command_outcome smallint CHECK (command_outcome BETWEEN 1 AND 7),
    scene_outcome smallint CHECK (scene_outcome BETWEEN 1 AND 8),
    scene_aggregate smallint CHECK (scene_aggregate BETWEEN 1 AND 5),
    script_mode smallint CHECK (script_mode BETWEEN 1 AND 2),
    native_ordinal bigint CHECK (native_ordinal > 0),
    node_visit bigint CHECK (node_visit > 0),
    command_ordinal bigint CHECK (command_ordinal > 0),
    action_kind smallint CHECK (action_kind BETWEEN 1 AND 11),
    failure_kind smallint CHECK (failure_kind BETWEEN 1 AND 7),
    wait_exit smallint CHECK (wait_exit BETWEEN 1 AND 5),
    CHECK ((kind = 1 AND decision IS NOT NULL AND node_stage IS NULL AND command_outcome IS NULL AND scene_outcome IS NULL AND scene_aggregate IS NULL AND script_mode IS NULL) OR
           (kind = 2 AND decision IS NULL AND node_stage IS NOT NULL AND command_outcome IS NULL AND scene_outcome IS NULL AND scene_aggregate IS NULL AND script_mode IS NULL) OR
           (kind = 3 AND decision IS NULL AND node_stage IS NULL AND command_outcome IS NOT NULL AND scene_outcome IS NULL AND scene_aggregate IS NULL AND script_mode IS NULL) OR
           (kind = 4 AND decision IS NULL AND node_stage IS NULL AND command_outcome IS NULL AND scene_outcome IS NOT NULL AND scene_aggregate IS NULL AND script_mode IS NULL) OR
           (kind = 5 AND decision IS NULL AND node_stage IS NULL AND command_outcome IS NULL AND scene_outcome IS NULL AND scene_aggregate IS NOT NULL AND script_mode IS NULL) OR
           (kind = 6 AND decision IS NULL AND node_stage IS NULL AND command_outcome IS NULL AND scene_outcome IS NULL AND scene_aggregate IS NULL AND script_mode IS NOT NULL)),
    controller_generation bigint CHECK (controller_generation > 0),
    run_id bigint CHECK (run_id > 0),
    parent_run_id bigint CHECK (parent_run_id > 0),
    provenance_kind smallint NOT NULL CHECK (provenance_kind IN (1,2)),
    config_revision bigint CHECK (config_revision > 0),
    manifest_sha256 bytea CHECK (octet_length(manifest_sha256) = 32),
    CHECK ((provenance_kind = 1 AND config_revision IS NULL AND manifest_sha256 IS NULL) OR
           (provenance_kind = 2 AND config_revision IS NOT NULL AND manifest_sha256 IS NOT NULL)),
    object_id text CHECK (octet_length(object_id) <= 256),
    source_file text CHECK (octet_length(source_file) <= 512),
    source_path text CHECK (octet_length(source_path) <= 1024),
    source_line integer CHECK (source_line > 0),
    source_column integer CHECK (source_column > 0),
    trigger_key text CHECK (octet_length(trigger_key) <= 256),
    command_id text CHECK (octet_length(command_id) <= 256),
    scene_invocation text CHECK (octet_length(scene_invocation) <= 256),
    scene_member integer CHECK (scene_member >= 0),
    branches integer[] CHECK (cardinality(branches) <= 8),
    iterations integer[] CHECK (cardinality(iterations) <= 8),
    record_digest bytea NOT NULL CHECK (octet_length(record_digest) = 32),
    PRIMARY KEY(epoch, ordinal)
);
CREATE INDEX execution_history_records_time ON public.execution_history_records(emission_time, epoch, ordinal);
CREATE INDEX execution_history_records_page ON public.execution_history_records(emission_time, epoch, ordinal, commit_sequence);
CREATE INDEX execution_history_batches_retention ON public.execution_history_batches(retention_time, epoch, batch_number);
CREATE TABLE public.execution_history_loss (
    epoch text NOT NULL REFERENCES public.execution_history_epochs(epoch),
    batch_number bigint NOT NULL,
    reason smallint NOT NULL CHECK (reason BETWEEN 1 AND 4),
    known_count bigint NOT NULL CHECK (known_count > 0),
    uncertain_envelope boolean NOT NULL,
    start_time timestamptz NOT NULL,
    emission_time timestamptz NOT NULL,
    CHECK (emission_time >= start_time),
    commit_sequence bigint NOT NULL CHECK (commit_sequence > 0),
    PRIMARY KEY(epoch, batch_number, reason),
    FOREIGN KEY(epoch, batch_number) REFERENCES public.execution_history_batches(epoch, batch_number)
);
CREATE INDEX execution_history_loss_time ON public.execution_history_loss(emission_time, epoch, batch_number);
`
