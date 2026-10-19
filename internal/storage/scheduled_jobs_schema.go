package storage

// scheduledJobsSchema is registered once in the append-only migration plan.
// Registration/version is serialized by the campaign coordinator. Numeric
// state/work/late values are converted explicitly by internal/jobs; its schema
// conformance tests pin their accepted ranges and reject unknown values.
const scheduledJobsSchema = `
CREATE TABLE public.scheduled_job_definitions (
    job_id text NOT NULL CHECK (octet_length(job_id) BETWEEN 1 AND 1024),
    version bigint NOT NULL CHECK (version > 0),
    deadline_utc timestamptz NOT NULL,
    timezone text NOT NULL CHECK (octet_length(timezone) BETWEEN 1 AND 128),
    expires_at_utc timestamptz NOT NULL CHECK (expires_at_utc > deadline_utc),
    late_policy smallint NOT NULL CHECK (late_policy IN (1, 2)),
    tolerance_us bigint NOT NULL,
    work_kind smallint NOT NULL CHECK (work_kind IN (1, 2)),
    owner_id text,
    session_id text,
    command_template bytea,
    backup_key text,
    PRIMARY KEY (job_id, version),
    CHECK ((late_policy=1 AND tolerance_us BETWEEN 1 AND 2592000000000) OR
           (late_policy=2 AND tolerance_us=0)),
    CHECK ((work_kind=1 AND owner_id IS NOT NULL AND octet_length(owner_id) BETWEEN 1 AND 1024 AND
            session_id IS NOT NULL AND octet_length(session_id) BETWEEN 1 AND 1024 AND
            command_template IS NOT NULL AND octet_length(command_template) BETWEEN 1 AND 65536 AND backup_key IS NULL) OR
           (work_kind=2 AND owner_id IS NULL AND session_id IS NULL AND command_template IS NULL AND
            backup_key IS NOT NULL AND octet_length(backup_key) BETWEEN 1 AND 1024))
);
CREATE TABLE public.scheduled_jobs (
    occurrence_id text PRIMARY KEY CHECK (octet_length(occurrence_id) BETWEEN 1 AND 1024),
    job_id text NOT NULL,
    definition_version bigint NOT NULL,
    idempotency_key text NOT NULL UNIQUE CHECK (octet_length(idempotency_key) BETWEEN 1 AND 1024),
    command_id text UNIQUE,
    state smallint NOT NULL DEFAULT 1 CHECK (state BETWEEN 1 AND 8),
    claim_token bigint NOT NULL DEFAULT 0 CHECK (claim_token >= 0),
    lease_until timestamptz,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (job_id, definition_version) REFERENCES public.scheduled_job_definitions(job_id, version),
    UNIQUE (job_id, definition_version),
    CHECK ((state IN (2, 3) AND claim_token > 0 AND lease_until IS NOT NULL) OR
           (state NOT IN (2, 3) AND lease_until IS NULL))
);
CREATE INDEX scheduled_jobs_pending ON public.scheduled_jobs(job_id, definition_version, occurrence_id) WHERE state IN (1, 2);
CREATE INDEX scheduled_jobs_uncertain ON public.scheduled_jobs(lease_until, occurrence_id) WHERE state=3;
CREATE INDEX scheduled_job_deadlines ON public.scheduled_job_definitions(deadline_utc, job_id, version);
CREATE FUNCTION public.homeward_job_definition_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'durable job definition is immutable' USING ERRCODE = '23514';
END
$$;
CREATE TRIGGER scheduled_job_definition_immutable BEFORE UPDATE ON public.scheduled_job_definitions
    FOR EACH ROW EXECUTE FUNCTION public.homeward_job_definition_immutable();
CREATE FUNCTION public.homeward_job_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.occurrence_id IS DISTINCT FROM OLD.occurrence_id OR
       NEW.job_id IS DISTINCT FROM OLD.job_id OR NEW.definition_version IS DISTINCT FROM OLD.definition_version OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR NEW.command_id IS DISTINCT FROM OLD.command_id THEN
        RAISE EXCEPTION 'durable job occurrence identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER scheduled_job_identity_immutable BEFORE UPDATE ON public.scheduled_jobs
    FOR EACH ROW EXECUTE FUNCTION public.homeward_job_identity_immutable();
`
