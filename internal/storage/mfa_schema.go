package storage

// NHA-142 follows the delivered solar migration 17. Account state remains
// unchanged until the explicit versioned-policy bootstrap before admissions.
const mfaMigration18SQL = `
ALTER TYPE public.config_document_kind ADD VALUE 'mfa_policy';
ALTER TYPE public.config_schema_version ADD VALUE 'homeward.mfa-policy/1';
ALTER TYPE public.config_syntax_profile ADD VALUE 'homeward.mfa-policy-yaml/1';
CREATE TABLE public.auth_mfa_policy (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 document_id text NOT NULL UNIQUE REFERENCES public.config_documents(id),
 version_revision bigint NOT NULL CHECK(version_revision>0),
 require_admin_factor boolean NOT NULL
);
CREATE FUNCTION public.homeward_mfa_guard_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(481316,151);
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'MFA policy is protected' USING ERRCODE='23514'; END IF;
 IF NEW.document_id<>'security:mfa-policy' OR NOT EXISTS (
  SELECT 1 FROM public.config_versions v JOIN public.config_documents d ON d.id=v.document_id
  WHERE v.document_id=NEW.document_id AND v.revision=NEW.version_revision
  AND d.kind::text='mfa_policy' AND d.logical_path='security/mfa-policy.yaml'
  AND v.schema_version::text='homeward.mfa-policy/1' AND v.syntax_profile::text='homeward.mfa-policy-yaml/1'
  AND convert_from(v.yaml_source,'UTF8')='schema: homeward.mfa-policy/1'||chr(10)||'require_admin_factor: '||CASE WHEN NEW.require_admin_factor THEN 'true' ELSE 'false' END||chr(10)
 ) THEN RAISE EXCEPTION 'MFA policy source mismatch' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND (NEW.singleton IS DISTINCT FROM OLD.singleton OR NEW.document_id IS DISTINCT FROM OLD.document_id OR NEW.version_revision<=OLD.version_revision)
 THEN RAISE EXCEPTION 'MFA policy revision must advance' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER auth_mfa_policy_guard BEFORE INSERT OR UPDATE OR DELETE ON public.auth_mfa_policy FOR EACH ROW EXECUTE FUNCTION public.homeward_mfa_guard_policy();
CREATE FUNCTION public.homeward_mfa_guard_active_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(481316,151);
 IF EXISTS(SELECT 1 FROM public.auth_mfa_policy p WHERE p.document_id=OLD.document_id AND p.version_revision=OLD.revision)
 THEN RAISE EXCEPTION 'active MFA policy is protected' USING ERRCODE='23514'; END IF;
 RETURN OLD;
END $$;
CREATE TRIGGER config_mfa_active_version BEFORE DELETE ON public.config_versions FOR EACH ROW EXECUTE FUNCTION public.homeward_mfa_guard_active_version();
CREATE TABLE public.auth_mfa_instance (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 instance_id text NOT NULL UNIQUE CHECK(octet_length(instance_id) BETWEEN 1 AND 128),
 rp_id text NOT NULL CHECK(octet_length(rp_id) BETWEEN 1 AND 253),
 origin text NOT NULL CHECK(octet_length(origin) BETWEEN 1 AND 4096)
);
CREATE TABLE public.auth_mfa_users (
 account_id text PRIMARY KEY REFERENCES public.auth_accounts(id) ON DELETE CASCADE,
 instance_id text NOT NULL REFERENCES public.auth_mfa_instance(instance_id),
 user_handle bytea NOT NULL UNIQUE CHECK(octet_length(user_handle)=32)
);
CREATE TABLE public.auth_mfa_credentials (
 account_id text NOT NULL REFERENCES public.auth_accounts(id) ON DELETE CASCADE,
 id text NOT NULL CHECK(octet_length(id) BETWEEN 1 AND 128),
 instance_id text NOT NULL REFERENCES public.auth_mfa_instance(instance_id),
 rp_id text NOT NULL CHECK(octet_length(rp_id) BETWEEN 1 AND 253),
 record_version bigint NOT NULL CHECK(record_version>0),
 key_id bytea NOT NULL CHECK(octet_length(key_id)=32),
 cipher_suite text NOT NULL CHECK(cipher_suite='aes256gcm_hkdf_sha256_v1'),
 salt bytea NOT NULL UNIQUE REFERENCES public.secret_salts(salt),
 ciphertext bytea NOT NULL CHECK(octet_length(ciphertext) BETWEEN 29 AND 65564),
 created_at timestamptz NOT NULL,last_used_at timestamptz,removed_at timestamptz,
 PRIMARY KEY(account_id,id)
);
CREATE TABLE public.auth_mfa_recovery_batches (
 account_id text NOT NULL REFERENCES public.auth_accounts(id) ON DELETE CASCADE,
 id text NOT NULL CHECK(octet_length(id) BETWEEN 1 AND 128),
 created_at timestamptz NOT NULL,retired_at timestamptz,
 PRIMARY KEY(account_id,id)
);
CREATE UNIQUE INDEX auth_mfa_one_recovery_batch ON public.auth_mfa_recovery_batches(account_id) WHERE retired_at IS NULL;
CREATE TABLE public.auth_mfa_recovery_codes (
 account_id text NOT NULL,batch_id text NOT NULL,id text NOT NULL CHECK(octet_length(id) BETWEEN 1 AND 128),
 digest bytea NOT NULL CHECK(octet_length(digest)=32),consumed_at timestamptz,
 PRIMARY KEY(account_id,batch_id,id),UNIQUE(account_id,batch_id,digest),
 FOREIGN KEY(account_id,batch_id) REFERENCES public.auth_mfa_recovery_batches(account_id,id) ON DELETE CASCADE
);
CREATE TABLE public.auth_mfa_ceremonies (
 account_id text NOT NULL REFERENCES public.auth_accounts(id) ON DELETE CASCADE,
 kind smallint NOT NULL CHECK(kind IN(1,2)),
 id text NOT NULL UNIQUE CHECK(octet_length(id) BETWEEN 1 AND 128),
 account_revision bigint NOT NULL CHECK(account_revision>0),
 policy_revision bigint NOT NULL CHECK(policy_revision>0),policy_hash bytea NOT NULL CHECK(octet_length(policy_hash)=32),
 instance_id text NOT NULL REFERENCES public.auth_mfa_instance(instance_id),rp_id text NOT NULL,
 authority smallint NOT NULL CHECK(authority IN(1,2,3)),session_id text,grant_digest bytea,
 options_hash bytea NOT NULL CHECK(octet_length(options_hash)=32),
 key_id bytea NOT NULL CHECK(octet_length(key_id)=32),cipher_suite text NOT NULL CHECK(cipher_suite='aes256gcm_hkdf_sha256_v1'),
 salt bytea NOT NULL UNIQUE REFERENCES public.secret_salts(salt),ciphertext bytea NOT NULL CHECK(octet_length(ciphertext) BETWEEN 29 AND 16412),
 expires_at timestamptz NOT NULL,attempts smallint NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),consumed_at timestamptz,
 PRIMARY KEY(account_id,kind),
 CHECK((authority=1 AND session_id IS NOT NULL AND grant_digest IS NULL AND kind=2) OR
       (authority=2 AND session_id IS NULL AND grant_digest IS NOT NULL AND octet_length(grant_digest)=32 AND kind=2) OR
       (authority=3 AND session_id IS NULL AND grant_digest IS NULL AND kind=1))
);
CREATE INDEX auth_mfa_ceremonies_expiry ON public.auth_mfa_ceremonies(expires_at);
CREATE TABLE public.auth_mfa_budgets (
 account_id text PRIMARY KEY REFERENCES public.auth_accounts(id) ON DELETE CASCADE,
 begins timestamptz[] NOT NULL DEFAULT '{}' CHECK(cardinality(begins)<=3),
 proofs timestamptz[] NOT NULL DEFAULT '{}' CHECK(cardinality(proofs)<=10)
);
ALTER TABLE public.auth_sessions ADD COLUMN satisfaction smallint NOT NULL DEFAULT 0 CHECK(satisfaction BETWEEN 0 AND 3),
 ADD COLUMN authenticated_at timestamptz,ADD COLUMN factor_id text,ADD COLUMN recovery_batch_id text,ADD COLUMN recovery_code_id text,
 ADD CONSTRAINT auth_session_original_authentication CHECK(
 (satisfaction=0 AND authenticated_at IS NULL AND factor_id IS NULL AND recovery_batch_id IS NULL AND recovery_code_id IS NULL) OR
 (authenticated_at IS NOT NULL AND authenticated_at=created_at AND (
  (satisfaction=1 AND factor_id IS NULL AND recovery_batch_id IS NULL AND recovery_code_id IS NULL) OR
  (satisfaction=2 AND factor_id IS NOT NULL AND recovery_batch_id IS NULL AND recovery_code_id IS NULL) OR
  (satisfaction=3 AND factor_id IS NULL AND recovery_batch_id IS NOT NULL AND recovery_code_id IS NOT NULL))));
ALTER TABLE public.auth_audit DROP CONSTRAINT auth_audit_action_check;
ALTER TABLE public.auth_audit ADD CONSTRAINT auth_audit_action_check CHECK(action BETWEEN 1 AND 29);
`
