package storage

// Migration 22 adds only new activation forms. Versions 1–21 and their stored
// source/manifests remain byte-for-byte valid.
const configActivationSchemaSQL = `
-- The same real source can have its historical declarative document and a
-- distinct exact operational document. Identity remains unique per kind/path.
ALTER TABLE public.config_documents DROP CONSTRAINT config_documents_logical_path_key;
ALTER TABLE public.config_documents ADD CONSTRAINT config_documents_kind_logical_path_key UNIQUE(kind,logical_path);

ALTER TYPE public.config_document_kind ADD VALUE 'operational_yaml';
ALTER TYPE public.config_document_kind ADD VALUE 'bootstrap_launch';
ALTER TYPE public.config_schema_version ADD VALUE 'homeward.operational/1';
ALTER TYPE public.config_schema_version ADD VALUE 'homeward.bootstrap-launch/1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'homeward.operational-yaml/1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'homeward.bootstrap-launch-yaml/1';

CREATE TYPE public.config_source_representation AS ENUM ('plain_v1', 'protected_operational_v1');
ALTER TABLE public.config_versions
    ALTER COLUMN yaml_source DROP NOT NULL,
    ADD COLUMN source_representation public.config_source_representation NOT NULL DEFAULT 'plain_v1',
    ADD COLUMN ciphertext bytea,
    ADD COLUMN key_id bytea REFERENCES public.secret_bootstrap(key_id),
    ADD COLUMN suite public.secret_cipher_suite,
    ADD COLUMN salt bytea UNIQUE REFERENCES public.secret_salts(salt);
ALTER TABLE public.config_versions DROP CONSTRAINT config_version_source_hash_matches_bytes;
ALTER TABLE public.config_versions ADD CONSTRAINT config_version_source_hash_matches_bytes CHECK (
    (source_representation = 'plain_v1' AND yaml_source IS NOT NULL AND
     ciphertext IS NULL AND key_id IS NULL AND suite IS NULL AND salt IS NULL AND
     source_hash = sha256(yaml_source)) OR
    (source_representation = 'protected_operational_v1' AND yaml_source IS NULL AND
     ciphertext IS NOT NULL AND octet_length(ciphertext) BETWEEN 29 AND 1048604 AND
     key_id IS NOT NULL AND octet_length(key_id) = 32 AND
     suite IS NOT NULL AND suite = 'aes256gcm_hkdf_sha256_v1' AND
     salt IS NOT NULL AND octet_length(salt) = 32)
);

-- All source kinds and their validators remain closed. A policy or bootstrap
-- document cannot be disguised as an encrypted operational source.
CREATE OR REPLACE FUNCTION public.homeward_config_guard_version_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE allocated bigint;
DECLARE document_kind text;
BEGIN
    SELECT last_revision, kind::text INTO allocated, document_kind
      FROM public.config_documents WHERE id = NEW.document_id;
    IF NEW.revision IS DISTINCT FROM allocated OR
       (NEW.revision = 1 AND NEW.parent_revision IS NOT NULL) OR
       (NEW.revision > 1 AND NEW.parent_revision IS DISTINCT FROM NEW.revision - 1) THEN
        RAISE EXCEPTION 'config revision was not allocated' USING ERRCODE = '23514';
    END IF;
    IF NOT (
       (document_kind = 'yaml_source' AND NEW.source_representation = 'plain_v1' AND
        ((NEW.schema_version::text = 'ha-declarative-1' AND NEW.syntax_profile::text = 'ha-2026.10/declarative-1') OR
         (NEW.schema_version::text = 'ha-declarative-2' AND NEW.syntax_profile::text = 'ha-2026.10/declarative-2'))) OR
       (document_kind = 'authorization_policy' AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'homeward.authorization/1' AND NEW.syntax_profile::text = 'homeward.authorization-yaml/1') OR
       (document_kind = 'mfa_policy' AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'homeward.mfa-policy/1' AND NEW.syntax_profile::text = 'homeward.mfa-policy-yaml/1') OR
       (document_kind = 'bootstrap_launch' AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'homeward.bootstrap-launch/1' AND NEW.syntax_profile::text = 'homeward.bootstrap-launch-yaml/1') OR
       (document_kind = 'operational_yaml' AND NEW.source_representation = 'protected_operational_v1' AND
        NEW.schema_version::text = 'homeward.operational/1' AND NEW.syntax_profile::text = 'homeward.operational-yaml/1')
    ) THEN
        RAISE EXCEPTION 'config source kind/profile mismatch' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE TYPE public.config_manifest_format AS ENUM ('legacy_v1', 'compiled_v2');
ALTER TABLE public.config_generations
    ADD COLUMN manifest_format public.config_manifest_format NOT NULL DEFAULT 'legacy_v1';
CREATE OR REPLACE FUNCTION public.homeward_config_guard_manifest() RETURNS trigger LANGUAGE plpgsql AS $$
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
          NEW.manifest_format IS NOT DISTINCT FROM OLD.manifest_format AND
          NEW.created_at IS NOT DISTINCT FROM OLD.created_at THEN
        PERFORM pg_advisory_xact_lock(481316, 151);
        SELECT count(*), COALESCE(sum(CASE v.source_representation
            WHEN 'protected_operational_v1' THEN octet_length(v.ciphertext)-28
            ELSE octet_length(v.yaml_source) END),0)
          INTO actual_count, source_bytes
          FROM public.config_generation_entries e
          LEFT JOIN public.config_versions v
            ON v.document_id=e.document_id AND v.revision=e.version_revision
          WHERE e.generation_revision=OLD.revision;
        IF actual_count <> OLD.entry_count OR source_bytes > 4194304 OR EXISTS (
            SELECT 1 FROM public.config_generation_entries e
            LEFT JOIN public.config_versions v
              ON v.document_id=e.document_id AND v.revision=e.version_revision
            JOIN public.config_documents d ON d.id=e.document_id
            WHERE e.generation_revision=OLD.revision
              AND (v.revision IS NULL OR v.source_hash <> e.source_hash OR
                   (OLD.manifest_format = 'legacy_v1' AND (d.kind <> 'yaml_source' OR v.source_representation <> 'plain_v1')) OR
                   (OLD.manifest_format = 'compiled_v2' AND d.kind NOT IN ('yaml_source','operational_yaml')))
        ) OR (OLD.manifest_format = 'compiled_v2' AND
              (SELECT count(*) FROM public.config_generation_entries e
               JOIN public.config_documents d ON d.id=e.document_id
               WHERE e.generation_revision=OLD.revision AND d.kind='operational_yaml') <> 1
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

CREATE TABLE public.config_bootstrap_active (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    document_id text,
    version_revision bigint,
    source_hash bytea,
    activated_at timestamptz,
    FOREIGN KEY (document_id,version_revision) REFERENCES public.config_versions(document_id,revision),
    CHECK ((document_id IS NULL AND version_revision IS NULL AND source_hash IS NULL AND activated_at IS NULL) OR
           (document_id IS NOT NULL AND version_revision IS NOT NULL AND version_revision > 0 AND
            source_hash IS NOT NULL AND octet_length(source_hash)=32 AND activated_at IS NOT NULL))
);
INSERT INTO public.config_bootstrap_active(singleton) VALUES(true);
CREATE FUNCTION public.homeward_config_guard_bootstrap_active() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_kind text;
DECLARE actual_hash bytea;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bootstrap pointer cannot be removed' USING ERRCODE='23514';
    END IF;
    PERFORM pg_advisory_xact_lock(481316,151);
    IF OLD.version_revision IS NOT NULL AND
       (NEW.document_id IS DISTINCT FROM OLD.document_id OR NEW.version_revision <= OLD.version_revision) THEN
        RAISE EXCEPTION 'bootstrap revision must advance' USING ERRCODE='23514';
    END IF;
    IF NEW.version_revision IS NOT NULL THEN
        SELECT d.kind::text, v.source_hash INTO actual_kind, actual_hash
          FROM public.config_versions v JOIN public.config_documents d ON d.id=v.document_id
          WHERE v.document_id=NEW.document_id AND v.revision=NEW.version_revision AND
                v.source_representation='plain_v1';
        IF actual_kind IS DISTINCT FROM 'bootstrap_launch' OR actual_hash IS DISTINCT FROM NEW.source_hash THEN
            RAISE EXCEPTION 'bootstrap source mismatch' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER config_bootstrap_active_integrity BEFORE UPDATE OR DELETE ON public.config_bootstrap_active
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_guard_bootstrap_active();
CREATE TRIGGER config_bootstrap_active_no_insert BEFORE INSERT ON public.config_bootstrap_active
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_no_insert();
CREATE TRIGGER config_bootstrap_active_no_truncate BEFORE TRUNCATE ON public.config_bootstrap_active
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_no_truncate();

-- The active launch document is retained even at the 20-version boundary.
CREATE OR REPLACE FUNCTION public.homeward_config_guard_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'config version is immutable' USING ERRCODE = '23514';
    END IF;
    PERFORM pg_advisory_xact_lock(481316,151);
    IF current_setting('homeward.config_gc',true) IS DISTINCT FROM 'on' THEN
        RAISE EXCEPTION 'config version deletion requires collection' USING ERRCODE='23514';
    END IF;
    IF EXISTS (SELECT 1 FROM public.config_active a
               JOIN public.config_generation_entries e ON e.generation_revision=a.generation_revision
               WHERE a.singleton AND e.document_id=OLD.document_id AND e.version_revision=OLD.revision)
       OR EXISTS (SELECT 1 FROM public.authorization_policy_active p
                  WHERE p.document_id=OLD.document_id AND p.version_revision=OLD.revision)
       OR EXISTS (SELECT 1 FROM public.config_bootstrap_active b WHERE b.singleton AND
                  b.document_id=OLD.document_id AND b.version_revision=OLD.revision) THEN
        RAISE EXCEPTION 'active config version is protected' USING ERRCODE='23514';
    END IF;
    RETURN OLD;
END
$$;

CREATE TABLE public.config_activation_attempts (
    attempt_id bytea PRIMARY KEY CHECK(octet_length(attempt_id)=16),
    expected_generation bigint,
    expected_manifest_hash bytea,
    target_generation bigint NOT NULL UNIQUE REFERENCES public.config_generations(revision),
    target_manifest_hash bytea NOT NULL CHECK(octet_length(target_manifest_hash)=32),
    author text NOT NULL CHECK(author<>'' AND octet_length(author)<=255),
    source text NOT NULL CHECK(source<>'' AND octet_length(source)<=255),
    committed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((expected_generation IS NULL AND expected_manifest_hash IS NULL) OR
           (expected_generation IS NOT NULL AND expected_generation>0 AND
            expected_manifest_hash IS NOT NULL AND octet_length(expected_manifest_hash)=32))
);
CREATE TABLE public.config_activation_policy_changes (
    attempt_id bytea NOT NULL REFERENCES public.config_activation_attempts(attempt_id),
    account_id text NOT NULL REFERENCES public.auth_accounts(id),
    before_revision bigint,
    before_hash bytea,
    after_revision bigint NOT NULL,
    after_hash bytea NOT NULL CHECK(octet_length(after_hash)=32),
    PRIMARY KEY(attempt_id,account_id),
    CHECK ((before_revision IS NULL AND before_hash IS NULL) OR
           (before_revision IS NOT NULL AND before_revision>0 AND
            before_hash IS NOT NULL AND octet_length(before_hash)=32))
);
CREATE FUNCTION public.homeward_config_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'config activation audit is immutable' USING ERRCODE='23514';
END
$$;
CREATE TRIGGER config_attempt_immutable BEFORE UPDATE OR DELETE ON public.config_activation_attempts
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_attempt_immutable();
CREATE TRIGGER config_policy_change_immutable BEFORE UPDATE OR DELETE ON public.config_activation_policy_changes
    FOR EACH ROW EXECUTE FUNCTION public.homeward_config_attempt_immutable();
CREATE TRIGGER config_attempt_no_truncate BEFORE TRUNCATE ON public.config_activation_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_attempt_immutable();
CREATE TRIGGER config_policy_change_no_truncate BEFORE TRUNCATE ON public.config_activation_policy_changes
    FOR EACH STATEMENT EXECUTE FUNCTION public.homeward_config_attempt_immutable();

CREATE TYPE public.command_origin_kind AS ENUM ('external_actor','internal_automation');
ALTER TABLE public.command_intents
    ALTER COLUMN actor_id DROP NOT NULL,
    ADD COLUMN origin_kind public.command_origin_kind NOT NULL DEFAULT 'external_actor',
    ADD COLUMN origin_key text,
    ADD CONSTRAINT command_origin_exactly_one CHECK (
        (origin_kind='external_actor' AND actor_id IS NOT NULL AND actor_id<>'' AND origin_key IS NULL) OR
        (origin_kind='internal_automation' AND actor_id IS NULL AND origin_key IS NOT NULL AND
         origin_key<>'' AND octet_length(origin_key)<=256)
    );
CREATE OR REPLACE FUNCTION public.homeward_command_guard_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.command_id IS DISTINCT FROM OLD.command_id OR NEW.actor_id IS DISTINCT FROM OLD.actor_id OR
       NEW.origin_kind IS DISTINCT FROM OLD.origin_kind OR NEW.origin_key IS DISTINCT FROM OLD.origin_key OR
       NEW.admission_epoch IS DISTINCT FROM OLD.admission_epoch OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint OR
       NEW.authorization_entity_id IS DISTINCT FROM OLD.authorization_entity_id OR
       NEW.authorization_schema_id IS DISTINCT FROM OLD.authorization_schema_id OR
       NEW.authorization_endpoint IS DISTINCT FROM OLD.authorization_endpoint THEN
        RAISE EXCEPTION 'command intention identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;
`
