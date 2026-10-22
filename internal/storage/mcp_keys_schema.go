package storage

const mcpKeysSchemaSQL = `
CREATE TABLE public.auth_mcp_keys (
 id text PRIMARY KEY CHECK(length(id)=32),
 account_id text NOT NULL REFERENCES public.auth_accounts(id) ON DELETE CASCADE,
 client_name text NOT NULL CHECK(length(client_name) BETWEEN 1 AND 128),
 token_digest bytea NOT NULL UNIQUE CHECK(octet_length(token_digest)=32),
 account_revision bigint NOT NULL CHECK(account_revision>0),
 rights_revision bigint NOT NULL CHECK(rights_revision>0),
 scopes jsonb NOT NULL CHECK(jsonb_typeof(scopes)='array' AND jsonb_array_length(scopes)<=39),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK(expires_at>created_at AND expires_at-created_at<=interval '8760 hours'),
 revoked_at timestamptz,
 last_used_at timestamptz
);
CREATE INDEX auth_mcp_keys_account ON public.auth_mcp_keys(account_id,created_at,id);
`
