package storage

// Migration 26 follows the delivered NHA036 migration25 parent. It extends the
// existing document authority; no plan table, active pointer or source store is
// added. Its native source kinds require the release26 reader and writer.
const configEditableSourcesSchemaSQL = `
ALTER TYPE public.config_document_kind ADD VALUE 'dashboard_source';
ALTER TYPE public.config_document_kind ADD VALUE 'automation_source';
ALTER TYPE public.config_document_kind ADD VALUE 'script_source';
ALTER TYPE public.config_document_kind ADD VALUE 'scene_source';
ALTER TYPE public.config_document_kind ADD VALUE 'ownership_source';
ALTER TYPE public.config_schema_version ADD VALUE 'homeward-dashboard-1';
ALTER TYPE public.config_schema_version ADD VALUE 'ha-logic-1';
ALTER TYPE public.config_schema_version ADD VALUE 'ha-scene-1';
ALTER TYPE public.config_schema_version ADD VALUE 'homeward.heating-ownership/1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'ha-2026.10/lovelace-1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'ha-2026.10/logic-1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'ha-2026.10/scene-1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'homeward.heating-ownership-json/1';
ALTER TABLE public.config_documents
  ADD COLUMN source_identity text,
  ADD COLUMN source_entry text,
  ADD CONSTRAINT config_document_source_descriptor CHECK (
    (source_identity IS NULL AND source_entry IS NULL AND
     kind::text NOT IN ('dashboard_source','automation_source','script_source','scene_source','ownership_source')) OR
    (kind::text IN ('dashboard_source','automation_source','script_source','scene_source','ownership_source') AND
     source_identity IS NOT NULL AND octet_length(source_identity) BETWEEN 1 AND 255 AND
     source_entry IS NOT NULL AND octet_length(source_entry) BETWEEN 1 AND 4096));

ALTER TABLE public.config_documents
 DROP CONSTRAINT config_documents_kind_logical_path_key;
-- PostgreSQL 14.17: retain legacy ungrouped uniqueness while giving each
-- admitted source identity its own native logical-path namespace.
CREATE UNIQUE INDEX config_documents_ungrouped_path_key
 ON public.config_documents(kind,logical_path)
 WHERE source_identity IS NULL;
CREATE UNIQUE INDEX config_documents_grouped_path_key
 ON public.config_documents(kind,source_identity,logical_path)
 WHERE source_identity IS NOT NULL;

CREATE OR REPLACE FUNCTION public.homeward_config_guard_document() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'config document identity and revision are protected' USING ERRCODE = '23514';
 END IF;
 IF NEW.id IS DISTINCT FROM OLD.id OR
    NEW.logical_path IS DISTINCT FROM OLD.logical_path OR
    NEW.kind IS DISTINCT FROM OLD.kind OR
    NEW.source_identity IS DISTINCT FROM OLD.source_identity OR
    NEW.source_entry IS DISTINCT FROM OLD.source_entry OR
    NEW.last_revision IS DISTINCT FROM OLD.last_revision + 1 THEN
  RAISE EXCEPTION 'config document identity and revision are protected' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END
$$;

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
       (document_kind = 'dashboard_source' AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'homeward-dashboard-1' AND NEW.syntax_profile::text = 'ha-2026.10/lovelace-1') OR
       (document_kind IN ('automation_source','script_source') AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'ha-logic-1' AND NEW.syntax_profile::text = 'ha-2026.10/logic-1') OR
       (document_kind = 'scene_source' AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'ha-scene-1' AND NEW.syntax_profile::text = 'ha-2026.10/scene-1') OR
       (document_kind = 'ownership_source' AND NEW.source_representation = 'plain_v1' AND
        NEW.schema_version::text = 'homeward.heating-ownership/1' AND NEW.syntax_profile::text = 'homeward.heating-ownership-json/1') OR
       (document_kind = 'operational_yaml' AND NEW.source_representation = 'protected_operational_v1' AND
        NEW.schema_version::text = 'homeward.operational/1' AND NEW.syntax_profile::text = 'homeward.operational-yaml/1')
    ) THEN
        RAISE EXCEPTION 'config source kind/profile mismatch' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

ALTER TABLE public.config_generations
 ADD COLUMN declarative_workspace_group text
   CHECK (declarative_workspace_group IS NULL OR octet_length(declarative_workspace_group) BETWEEN 1 AND 255),
 ADD COLUMN compilation_context bytea,
 ADD COLUMN compilation_context_hash bytea,
 ADD COLUMN compilation_source_hash bytea,
 ADD CONSTRAINT config_generation_compilation_context CHECK (
 (compilation_context IS NULL AND compilation_context_hash IS NULL AND compilation_source_hash IS NULL) OR
 (compilation_context IS NOT NULL AND octet_length(compilation_context) BETWEEN 1 AND 8388608 AND
  compilation_context_hash IS NOT NULL AND octet_length(compilation_context_hash)=32 AND
  compilation_source_hash IS NOT NULL AND compilation_source_hash=candidate_hash));

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
          NEW.declarative_workspace_group IS NOT DISTINCT FROM OLD.declarative_workspace_group AND
          NEW.compilation_context IS NOT DISTINCT FROM OLD.compilation_context AND
          NEW.compilation_context_hash IS NOT DISTINCT FROM OLD.compilation_context_hash AND
          NEW.compilation_source_hash IS NOT DISTINCT FROM OLD.compilation_source_hash AND
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
        IF OLD.declarative_workspace_group IS NOT NULL AND (
          OLD.manifest_format <> 'compiled_v2' OR
          (SELECT count(*) FROM public.config_generation_entries e
           JOIN public.config_documents d ON d.id=e.document_id
           WHERE e.generation_revision=OLD.revision AND d.kind='yaml_source' AND d.logical_path=OLD.entry_path) <> 1 OR
          (SELECT count(DISTINCT (v.schema_version::text,v.syntax_profile::text))
           FROM public.config_generation_entries e JOIN public.config_documents d ON d.id=e.document_id
           LEFT JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision
           WHERE e.generation_revision=OLD.revision AND d.kind='yaml_source') <> 1
        ) THEN
          RAISE EXCEPTION 'declarative workspace locator does not match its complete bundle' USING ERRCODE = '23514';
        END IF;
        IF EXISTS (
            SELECT 1 FROM public.config_generation_entries e
            JOIN public.config_documents d ON d.id=e.document_id
            WHERE e.generation_revision=OLD.revision AND d.source_identity IS NOT NULL
        ) AND OLD.compilation_context IS NULL THEN
            RAISE EXCEPTION 'editable source compilation context is missing' USING ERRCODE = '23514';
        END IF;
        IF actual_count <> OLD.entry_count OR source_bytes > 4194304 OR EXISTS (
            SELECT 1 FROM public.config_generation_entries e
            LEFT JOIN public.config_versions v
              ON v.document_id=e.document_id AND v.revision=e.version_revision
            JOIN public.config_documents d ON d.id=e.document_id
            WHERE e.generation_revision=OLD.revision
              AND (v.revision IS NULL OR v.source_hash <> e.source_hash OR
                   (OLD.manifest_format = 'legacy_v1' AND (d.kind <> 'yaml_source' OR v.source_representation <> 'plain_v1')) OR
                   (OLD.manifest_format = 'compiled_v2' AND d.kind::text NOT IN ('yaml_source','operational_yaml','dashboard_source','automation_source','script_source','scene_source','ownership_source')))
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

`
