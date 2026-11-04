package storage

// The API index belongs to the existing registry. It never rewrites native IDs,
// aliases, grants or command reservations. A duplicate public ID fails the
// unique constraint instead of resolving to a different physical target.
const apiRegistrySchemaSQL = `
CREATE FUNCTION public.homeward_registry_resource_id(native_id text, prefix text)
RETURNS text LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT prefix || '-' || substr(pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(native_id, 'UTF8')), 'hex'), 1, 48)
$$;

CREATE TABLE public.homeward_registry_home (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    resource_id text NOT NULL UNIQUE CHECK (resource_id ~ '^[0-9a-f]{32}$' AND resource_id <> repeat('0',32)),
    create_time timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO public.homeward_registry_home(singleton, resource_id)
VALUES (true, replace(pg_catalog.gen_random_uuid()::text, '-', ''));

ALTER TABLE public.registry_devices ADD COLUMN api_resource_id text
    GENERATED ALWAYS AS (public.homeward_registry_resource_id(id, 'device')) STORED;
ALTER TABLE public.registry_devices ADD CONSTRAINT registry_devices_api_resource_id_unique UNIQUE(api_resource_id);
ALTER TABLE public.registry_entities ADD COLUMN api_resource_id text
    GENERATED ALWAYS AS (public.homeward_registry_resource_id(id, 'entity')) STORED;
ALTER TABLE public.registry_entities ADD CONSTRAINT registry_entities_api_resource_id_unique UNIQUE(api_resource_id);
ALTER TABLE public.registry_entities ADD COLUMN api_display_name text NOT NULL DEFAULT ''
    CHECK (char_length(api_display_name) <= 63 AND api_display_name !~ '[[:cntrl:]]');
ALTER TABLE public.registry_entities ADD COLUMN api_metadata_revision bigint NOT NULL DEFAULT 1 CHECK (api_metadata_revision > 0);
ALTER TABLE public.registry_entities ADD COLUMN api_create_time timestamptz;
ALTER TABLE public.registry_entities ALTER COLUMN api_create_time SET DEFAULT clock_timestamp();
ALTER TABLE public.registry_entities ADD COLUMN api_update_time timestamptz;

-- Existing entities have no proven creation time; leave it NULL.
CREATE TABLE public.homeward_api_resource_names (
    resource_type text NOT NULL CHECK(resource_type <> ''),
    native_key text NOT NULL CHECK(native_key <> ''),
    resource_id text NOT NULL CHECK(resource_id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    PRIMARY KEY(resource_type,native_key),
    UNIQUE(resource_type,resource_id)
);
INSERT INTO public.homeward_api_resource_names(resource_type,native_key,resource_id)
SELECT 'configurationDocuments',id,'r-'||replace(pg_catalog.gen_random_uuid()::text,'-','')
FROM public.config_documents;

CREATE TABLE public.api_mutation_results (
    actor_id text NOT NULL CHECK(octet_length(actor_id) BETWEEN 1 AND 255),
    method text NOT NULL CHECK(octet_length(method) BETWEEN 1 AND 256),
    request_id uuid NOT NULL,
    fingerprint bytea NOT NULL CHECK(octet_length(fingerprint)=32),
    result bytea NOT NULL CHECK(octet_length(result)<=1048576),
    result_sha256 bytea NOT NULL CHECK(octet_length(result_sha256)=32),
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL CHECK(expire_time>create_time),
    PRIMARY KEY(actor_id,method,request_id)
);
CREATE INDEX api_mutation_results_expire_time_idx ON public.api_mutation_results(expire_time);

-- A durable terminal publication proof belongs to the existing activation
-- attempt. The timestamp records its first durable observation, not an
-- invented publication time. It outlives the short request deduplication TTL.
ALTER TABLE public.config_activation_attempts
    ADD COLUMN api_operation_result bytea,
    ADD COLUMN api_operation_done_at timestamptz,
    ADD CONSTRAINT config_activation_attempt_api_operation_result_valid CHECK (
        (api_operation_result IS NULL AND api_operation_done_at IS NULL) OR
        (api_operation_result IS NOT NULL AND octet_length(api_operation_result) BETWEEN 1 AND 1048576 AND api_operation_done_at IS NOT NULL)
    );

-- Bounded media leases retain references and caller/source bindings only.
-- Providers remain owners of frames, streams, artwork bytes and credentials.
CREATE TABLE public.api_media_sessions (
    session_id text PRIMARY KEY CHECK(session_id ~ '^s-[a-z2-7]{51}[aq]$'),
    actor_id text NOT NULL CHECK(octet_length(actor_id) BETWEEN 1 AND 255),
    credential_binding bytea NOT NULL CHECK(octet_length(credential_binding)=32),
    source_entity_id text NOT NULL CHECK(octet_length(source_entity_id) BETWEEN 1 AND 255),
    source_schema text NOT NULL CHECK(octet_length(source_schema) BETWEEN 1 AND 255),
    source_endpoint text NOT NULL CHECK(octet_length(source_endpoint) BETWEEN 1 AND 256),
    content_kind smallint NOT NULL CHECK(content_kind IN (1,2)),
    source_binding bytea NOT NULL CHECK(octet_length(source_binding)=32),
    artwork_reference bytea,
    event_revision bigint NOT NULL CHECK(event_revision>=0),
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL,
    retire_time timestamptz,
    discard_time timestamptz NOT NULL,
    CHECK(expire_time>create_time AND expire_time<=create_time+interval '60 seconds'),
    CHECK(discard_time=expire_time+interval '24 hours'),
    CHECK((content_kind=1 AND artwork_reference IS NOT NULL AND octet_length(artwork_reference) BETWEEN 1 AND 512)
        OR (content_kind=2 AND artwork_reference IS NULL))
);
CREATE INDEX api_media_sessions_actor_idx ON public.api_media_sessions(actor_id,credential_binding,session_id);
CREATE INDEX api_media_sessions_discard_idx ON public.api_media_sessions(discard_time);

CREATE OR REPLACE FUNCTION public.homeward_config_attempt_immutable()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME='config_activation_attempts' AND TG_OP='UPDATE' THEN
        IF OLD.api_operation_result IS NULL AND OLD.api_operation_done_at IS NULL
           AND NEW.api_operation_result IS NOT NULL AND NEW.api_operation_done_at IS NOT NULL
           AND (to_jsonb(NEW)-'api_operation_result'-'api_operation_done_at')
               IS NOT DISTINCT FROM (to_jsonb(OLD)-'api_operation_result'-'api_operation_done_at') THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'config activation evidence is immutable' USING ERRCODE='23514';
END
$$;
`
