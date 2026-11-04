package storage

// ROOT composes this delta into migration 29. Existing native intentions remain
// intact; only newly admitted public commands have API provenance.
const physicalOperationSchemaSQL = `
ALTER TABLE public.command_intents ADD COLUMN api_capability_name text;
ALTER TABLE public.command_intents ADD COLUMN api_request_id uuid;
ALTER TABLE public.command_intents ADD COLUMN api_fingerprint bytea;
ALTER TABLE public.command_intents ADD COLUMN api_acknowledged_response bytea CHECK (api_acknowledged_response IS NULL OR (api_capability_name IS NOT NULL AND octet_length(api_acknowledged_response) BETWEEN 1 AND 65536));
ALTER TABLE public.command_intents ADD COLUMN api_terminal_response bytea CHECK (api_terminal_response IS NULL OR (api_capability_name IS NOT NULL AND octet_length(api_terminal_response) BETWEEN 1 AND 65536));
ALTER TABLE public.command_intents ADD CONSTRAINT command_api_metadata CHECK (
 (api_capability_name IS NULL AND api_request_id IS NULL AND api_fingerprint IS NULL) OR
 (origin_kind='external_actor' AND api_capability_name IS NOT NULL AND
 api_capability_name ~ '^homes/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/entities/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/capabilities/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND
 api_fingerprint IS NOT NULL AND octet_length(api_fingerprint)=32 AND
 (api_request_id IS NULL OR api_request_id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))
);
CREATE UNIQUE INDEX command_api_request_identity ON public.command_intents(actor_id,api_request_id) WHERE api_request_id IS NOT NULL;
CREATE FUNCTION public.command_api_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.api_acknowledged_response IS NOT NULL AND NEW.api_acknowledged_response IS DISTINCT FROM OLD.api_acknowledged_response THEN
  RAISE EXCEPTION 'immutable command API acknowledgement proof';
 END IF;
 IF OLD.api_terminal_response IS NOT NULL AND NEW.api_terminal_response IS DISTINCT FROM OLD.api_terminal_response THEN
  RAISE EXCEPTION 'immutable command API terminal proof';
 END IF;
 IF NEW.api_capability_name IS DISTINCT FROM OLD.api_capability_name OR
    NEW.api_request_id IS DISTINCT FROM OLD.api_request_id OR
    NEW.api_fingerprint IS DISTINCT FROM OLD.api_fingerprint OR
    (OLD.api_capability_name IS NOT NULL AND NEW.accepted_at IS DISTINCT FROM OLD.accepted_at) THEN
  RAISE EXCEPTION 'immutable command API provenance';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER command_api_immutable BEFORE UPDATE ON public.command_intents
 FOR EACH ROW EXECUTE FUNCTION public.command_api_immutable();
`
