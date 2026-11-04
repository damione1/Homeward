package storage

// configurationAPIRequestsSchemaSQL belongs to migration 29. This is the
// native activation owner's pre-effect request journal, not an activator or a
// candidate store. Private source bytes and credentials are absent; terminal
// results contain only the admitted, redacted public response.
// The one execution capability is issued only to a confirmed first reservation;
// a restarted process or duplicate caller can read, but cannot execute it.
const configurationAPIRequestsSchemaSQL = `
ALTER TABLE public.config_activation_attempts
    ADD COLUMN api_request_id UUID,
    ADD COLUMN api_request_method SMALLINT,
    ADD COLUMN api_request_create_time TIMESTAMPTZ,
    ADD CONSTRAINT config_attempt_api_request_pair CHECK (
        (api_request_id IS NULL AND api_request_method IS NULL AND api_request_create_time IS NULL)
        OR (api_request_method IS NOT NULL AND api_request_method BETWEEN 1 AND 7 AND api_request_create_time IS NOT NULL
            AND (api_request_id IS NOT NULL OR api_request_method BETWEEN 5 AND 7))
    );
ALTER TABLE public.config_activation_attempts
    ADD COLUMN api_integration_method SMALLINT,
    ADD COLUMN api_integration_instance_key TEXT,
    ADD COLUMN api_integration_credential_kind SMALLINT,
    ADD COLUMN api_integration_credential_id TEXT,
    ADD CONSTRAINT config_attempt_integration_credential CHECK (
        (api_integration_method IS NULL AND api_integration_credential_kind IS NULL AND api_integration_credential_id IS NULL)
        OR (api_integration_method IS NOT NULL AND api_request_method IS NOT NULL AND api_request_method=api_integration_method
            AND api_integration_credential_kind IS NOT NULL AND api_integration_credential_kind IN (1,2)
            AND api_integration_credential_id IS NOT NULL AND octet_length(api_integration_credential_id) BETWEEN 1 AND 255
            AND (api_integration_credential_kind<>2 OR octet_length(api_integration_credential_id)=32))
    ),
    ADD CONSTRAINT config_attempt_integration_target_pair CHECK (
        (api_integration_method IS NULL AND api_integration_instance_key IS NULL)
        OR (api_integration_method IS NOT NULL AND api_integration_method BETWEEN 5 AND 7 AND api_integration_instance_key IS NOT NULL
            AND octet_length(api_integration_instance_key) BETWEEN 1 AND 256)
    ),
    ADD CONSTRAINT config_attempt_integration_request_match CHECK (
        api_request_method IS NULL OR
        (api_request_method BETWEEN 1 AND 4 AND api_integration_method IS NULL) OR
        (api_request_method BETWEEN 5 AND 7 AND api_integration_method IS NOT NULL
            AND api_request_method=api_integration_method)
    );
CREATE INDEX config_attempt_api_request_lookup
    ON public.config_activation_attempts(author,api_request_id,api_request_method)
    WHERE api_request_id IS NOT NULL;
CREATE INDEX config_attempt_author_pagination
    ON public.config_activation_attempts(author,attempt_id) WHERE api_integration_method IS NULL;
CREATE INDEX config_attempt_integration_pagination
    ON public.config_activation_attempts(author,api_integration_credential_kind,api_integration_credential_id,attempt_id)
    WHERE api_integration_credential_kind IS NOT NULL;
CREATE TABLE public.config_api_requests (
    account_id TEXT NOT NULL CHECK (octet_length(account_id) BETWEEN 1 AND 255),
    method SMALLINT NOT NULL CHECK (method BETWEEN 1 AND 7),
    request_id UUID,
    CHECK(request_id IS NOT NULL OR method BETWEEN 5 AND 7),
    fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint)=32),
    attempt_id BYTEA PRIMARY KEY CHECK (octet_length(attempt_id)=16),
    execution_digest BYTEA NOT NULL CHECK (octet_length(execution_digest)=32),
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    expire_time TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()+interval '24 hours',
    failure_result BYTEA,
    failure_sha256 BYTEA,
    failure_at TIMESTAMPTZ,
    no_change_result BYTEA,
    no_change_sha256 BYTEA,
    no_change_at TIMESTAMPTZ,
    integration_instance_key TEXT,
    integration_credential_kind SMALLINT,
    integration_credential_id TEXT,
    CHECK ((method BETWEEN 1 AND 4 AND integration_credential_kind IS NULL AND integration_credential_id IS NULL)
        OR (method BETWEEN 5 AND 7 AND integration_credential_kind IS NOT NULL AND integration_credential_kind IN (1,2)
            AND integration_credential_id IS NOT NULL AND octet_length(integration_credential_id) BETWEEN 1 AND 255
            AND (integration_credential_kind<>2 OR octet_length(integration_credential_id)=32))),
    CHECK ((method BETWEEN 1 AND 4 AND integration_instance_key IS NULL)
        OR (method BETWEEN 5 AND 7 AND integration_instance_key IS NOT NULL
            AND octet_length(integration_instance_key) BETWEEN 1 AND 256)),
    UNIQUE(account_id,method,request_id),
    CHECK(expire_time=accepted_at+interval '24 hours'),
    CHECK (failure_result IS NULL OR no_change_result IS NULL),
    CHECK ((no_change_result IS NULL AND no_change_sha256 IS NULL AND no_change_at IS NULL)
        OR (method=6 AND no_change_result IS NOT NULL AND octet_length(no_change_result) BETWEEN 1 AND 1048576
            AND no_change_sha256 IS NOT NULL AND octet_length(no_change_sha256)=32
            AND no_change_sha256=sha256(no_change_result) AND no_change_at IS NOT NULL)),
    CHECK ((failure_result IS NULL AND failure_sha256 IS NULL AND failure_at IS NULL)
        OR (failure_result IS NOT NULL AND octet_length(failure_result) BETWEEN 1 AND 65536
            AND failure_sha256 IS NOT NULL AND octet_length(failure_sha256)=32
            AND failure_sha256=sha256(failure_result) AND failure_at IS NOT NULL))
);
CREATE INDEX config_api_requests_author_pagination ON public.config_api_requests(account_id,attempt_id);
CREATE INDEX config_api_requests_expiry ON public.config_api_requests(expire_time);
CREATE FUNCTION public.homeward_config_api_request_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' AND TG_LEVEL='ROW' THEN
        IF OLD.expire_time<=statement_timestamp() THEN RETURN OLD; END IF;
    ELSIF TG_OP='UPDATE' AND TG_LEVEL='ROW' THEN
        IF OLD.no_change_result IS NULL AND NEW.no_change_result IS NULL AND OLD.failure_result IS NULL AND OLD.failure_sha256 IS NULL AND OLD.failure_at IS NULL
            AND NEW.failure_result IS NOT NULL AND NEW.failure_sha256 IS NOT NULL AND NEW.failure_at IS NOT NULL
            AND (to_jsonb(NEW)-'failure_result'-'failure_sha256'-'failure_at') IS NOT DISTINCT FROM
                (to_jsonb(OLD)-'failure_result'-'failure_sha256'-'failure_at')
            AND NOT EXISTS(SELECT 1 FROM public.config_activation_attempts WHERE attempt_id=OLD.attempt_id)
        THEN RETURN NEW; END IF;
        IF OLD.no_change_result IS NULL AND OLD.no_change_sha256 IS NULL AND OLD.no_change_at IS NULL
            AND OLD.failure_result IS NULL AND NEW.failure_result IS NULL
            AND NEW.no_change_result IS NOT NULL AND NEW.no_change_sha256 IS NOT NULL AND NEW.no_change_at IS NOT NULL
            AND NEW.method=6
            AND (to_jsonb(NEW)-'no_change_result'-'no_change_sha256'-'no_change_at') IS NOT DISTINCT FROM
                (to_jsonb(OLD)-'no_change_result'-'no_change_sha256'-'no_change_at')
            AND NOT EXISTS(SELECT 1 FROM public.config_activation_attempts WHERE attempt_id=OLD.attempt_id)
        THEN RETURN NEW; END IF;
    END IF;
    RAISE EXCEPTION 'configuration request journal is immutable' USING ERRCODE='55000';
END
$$;
CREATE TRIGGER config_api_requests_immutable
BEFORE UPDATE OR DELETE ON public.config_api_requests
FOR EACH ROW EXECUTE FUNCTION public.homeward_config_api_request_immutable();
CREATE TRIGGER config_api_requests_no_truncate
BEFORE TRUNCATE ON public.config_api_requests
FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_api_request_immutable();
`
