package storage

// remoteCalendarSchemaSQL is reserved for migration23. Only the authoritative
// migrations owner registers it, after the real migration22 has been merged.
const remoteCalendarSchemaSQL = `
CREATE TABLE public.remote_calendar_cache (
    entity_id text PRIMARY KEY REFERENCES public.registry_source_bindings(entity_id),
    instance_id text NOT NULL,
    source_device text NOT NULL,
    source_entity text NOT NULL,
    origin smallint NOT NULL CHECK (origin=2),
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 512),
    revision bigint NOT NULL CHECK (revision>0),
    updated_at timestamptz NOT NULL,
    source_scope bytea CHECK (octet_length(source_scope)=32),
    state smallint NOT NULL CHECK (state IN (1,2,3)),
    representation_scope bytea CHECK (octet_length(representation_scope)=32),
    validated_at timestamptz,
    attempted_at timestamptz,
    fresh_until timestamptz,
    reason smallint NOT NULL CHECK (reason BETWEEN 0 AND 7),
    etag bytea CHECK (octet_length(etag) BETWEEN 1 AND 1024),
    last_modified timestamptz,
    http_metadata bytea CHECK (octet_length(http_metadata) BETWEEN 69 AND 32768),
    conditional_state smallint NOT NULL DEFAULT 2 CHECK (conditional_state IN (1,2)),
    payload_version smallint,
    payload_profile smallint,
    payload bytea CHECK (octet_length(payload) BETWEEN 20 AND 33554432),
    digest bytea CHECK (octet_length(digest)=32),
    event_count bigint,
    zone_count bigint,
    point_count bigint,
    exception_count bigint,
    text_bytes bigint,
    source_bytes bigint,
    observance_count bigint,
    rdate_count bigint,
    image_charge bigint,
    FOREIGN KEY(instance_id,source_device,source_entity)
        REFERENCES public.registry_source_bindings(instance_id,source_device,source_entity),
    CHECK ((state=1 AND representation_scope IS NULL AND validated_at IS NULL AND fresh_until IS NULL
        AND payload_version IS NULL AND payload_profile IS NULL AND payload IS NULL AND digest IS NULL
        AND event_count IS NULL AND zone_count IS NULL AND point_count IS NULL AND exception_count IS NULL
        AND text_bytes IS NULL AND source_bytes IS NULL AND observance_count IS NULL AND rdate_count IS NULL
        AND image_charge IS NULL AND etag IS NULL AND last_modified IS NULL AND http_metadata IS NULL AND conditional_state=2)
      OR (state IN (2,3) AND source_scope IS NOT NULL AND representation_scope IS NOT NULL AND validated_at IS NOT NULL
        AND attempted_at IS NOT NULL AND fresh_until IS NOT NULL AND payload_version IS NOT NULL
        AND payload_profile IS NOT NULL AND payload IS NOT NULL AND digest IS NOT NULL
        AND event_count IS NOT NULL AND zone_count IS NOT NULL AND point_count IS NOT NULL
        AND exception_count IS NOT NULL AND text_bytes IS NOT NULL AND source_bytes IS NOT NULL
        AND observance_count IS NOT NULL AND rdate_count IS NOT NULL AND image_charge IS NOT NULL
        AND event_count BETWEEN 0 AND 1024 AND zone_count BETWEEN 0 AND 72
        AND point_count BETWEEN 0 AND 524288 AND exception_count BETWEEN 0 AND 262144
        AND text_bytes BETWEEN 0 AND 33554432 AND source_bytes BETWEEN 0 AND 524288
        AND observance_count BETWEEN 0 AND 128 AND rdate_count BETWEEN 0 AND 32768
        AND image_charge BETWEEN 2097152 AND 33554432)),
    CHECK (conditional_state<>1 OR (state IN (2,3) AND http_metadata IS NOT NULL)),
    CHECK (state<>2 OR (source_scope IS NOT NULL AND source_scope=representation_scope AND reason=0 AND fresh_until>=validated_at))
);
CREATE FUNCTION public.homeward_remote_calendar_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.registry_source_bindings b
        JOIN public.registry_entities e ON e.id=b.entity_id
        WHERE b.entity_id=NEW.entity_id AND b.instance_id=NEW.instance_id
          AND b.source_device=NEW.source_device AND b.source_entity=NEW.source_entity AND NOT e.tombstoned) THEN
        RAISE EXCEPTION 'remote calendar binding mismatch' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (NEW.entity_id IS DISTINCT FROM OLD.entity_id OR
        NEW.instance_id IS DISTINCT FROM OLD.instance_id OR NEW.source_device IS DISTINCT FROM OLD.source_device OR
        NEW.source_entity IS DISTINCT FROM OLD.source_entity OR NEW.origin IS DISTINCT FROM OLD.origin) THEN
        RAISE EXCEPTION 'remote calendar binding is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER remote_calendar_binding BEFORE INSERT OR UPDATE ON public.remote_calendar_cache
    FOR EACH ROW EXECUTE FUNCTION public.homeward_remote_calendar_binding();
`
