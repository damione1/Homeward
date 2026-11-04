package storage

// apiAuthResourceNamesSQL runs in the same migration transaction as the existing
// API name index. Native identities, credentials, expirations and revocation
// evidence are untouched. Names are allocated once, including revoked metadata.
// Factor IDs are unique within an account, so their native key is a byte-length
// prefixed pair. It is reversible for every admitted historical UTF-8 identity.
const apiAuthResourceNamesSQL = `
INSERT INTO public.homeward_api_resource_names(resource_type,native_key,resource_id)
SELECT 'accounts',id,'r-'||replace(pg_catalog.gen_random_uuid()::text,'-','')
FROM public.auth_accounts
ON CONFLICT(resource_type,native_key) DO NOTHING;
INSERT INTO public.homeward_api_resource_names(resource_type,native_key,resource_id)
SELECT 'sessions',id,'r-'||replace(pg_catalog.gen_random_uuid()::text,'-','')
FROM public.auth_sessions
ON CONFLICT(resource_type,native_key) DO NOTHING;
INSERT INTO public.homeward_api_resource_names(resource_type,native_key,resource_id)
SELECT 'invitations',id,'r-'||replace(pg_catalog.gen_random_uuid()::text,'-','')
FROM public.auth_invitations
ON CONFLICT(resource_type,native_key) DO NOTHING;
INSERT INTO public.homeward_api_resource_names(resource_type,native_key,resource_id)
SELECT 'mcpKeys',id,'r-'||replace(pg_catalog.gen_random_uuid()::text,'-','')
FROM public.auth_mcp_keys
ON CONFLICT(resource_type,native_key) DO NOTHING;
INSERT INTO public.homeward_api_resource_names(resource_type,native_key,resource_id)
SELECT 'factors',octet_length(account_id)::text||':'||account_id||id,
       'r-'||replace(pg_catalog.gen_random_uuid()::text,'-','')
FROM public.auth_mfa_credentials
ON CONFLICT(resource_type,native_key) DO NOTHING;
`
