package server

import (
	"context"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
)

func (n authResourceNames) resolveAccount(ctx context.Context, name apitypes.AccountName) (contracts.AccountID, error) {
	if !name.Valid() {
		return contracts.AccountID{}, storage.ErrAPIResourceIdentity
	}
	key, err := n.resolve(ctx, storage.APIAccounts, name.String())
	if err != nil {
		return contracts.AccountID{}, err
	}
	return contracts.ParseAccountID(key)
}
func (n authResourceNames) invitation(ctx context.Context, id contracts.InvitationID) (apitypes.InvitationName, error) {
	key, err := id.Value()
	if err != nil {
		return apitypes.InvitationName{}, err
	}
	value, err := n.name(ctx, storage.APIInvitations, key)
	if err != nil {
		return apitypes.InvitationName{}, err
	}
	return apitypes.ParseInvitationName(value)
}
func (n authResourceNames) resolveInvitation(ctx context.Context, name apitypes.InvitationName) (contracts.InvitationID, error) {
	if !name.Valid() {
		return contracts.InvitationID{}, storage.ErrAPIResourceIdentity
	}
	key, err := n.resolve(ctx, storage.APIInvitations, name.String())
	if err != nil {
		return contracts.InvitationID{}, err
	}
	return contracts.ParseInvitationID(key)
}
func (n authResourceNames) child(ctx context.Context, account contracts.AccountID, kind storage.APIResourceType, key, collection string) (string, error) {
	parent, err := n.account(ctx, account)
	if err != nil {
		return "", err
	}
	id, err := n.store.LookupAPIResourceIdentity(ctx, kind, key)
	if err != nil {
		return "", err
	}
	return parent.String() + "/" + collection + "/" + id.ResourceID(), nil
}
func (n authResourceNames) session(ctx context.Context, owner storage.SessionOwner) (apitypes.SessionName, error) {
	key, err := owner.Session.Value()
	if err != nil {
		return apitypes.SessionName{}, err
	}
	value, err := n.child(ctx, owner.Account, storage.APISessions, key, "sessions")
	if err != nil {
		return apitypes.SessionName{}, err
	}
	return apitypes.ParseSessionName(value)
}
func (n authResourceNames) resolveSession(ctx context.Context, name apitypes.SessionName) (storage.SessionOwner, error) {
	account, err := n.resolveAccount(ctx, name.Account())
	if err != nil {
		return storage.SessionOwner{}, err
	}
	id, err := n.store.ResolveAPIResourceIdentity(ctx, storage.APISessions, name.ResourceID())
	if err != nil {
		return storage.SessionOwner{}, err
	}
	session, err := contracts.ParseSessionID(id.NativeKey())
	if err != nil {
		return storage.SessionOwner{}, storage.ErrAPIResourceIntegrity
	}
	owner, err := n.store.LookupSessionOwner(ctx, session)
	if err != nil {
		return storage.SessionOwner{}, err
	}
	if owner.Account != account {
		return storage.SessionOwner{}, storage.ErrAPIResourceNotFound
	}
	return owner, nil
}
func (n authResourceNames) factor(ctx context.Context, account contracts.AccountID, factor contracts.FactorCredentialID) (apitypes.FactorCredentialName, error) {
	key, err := storage.FactorAPIIdentityKey(account, factor)
	if err != nil {
		return apitypes.FactorCredentialName{}, err
	}
	value, err := n.child(ctx, account, storage.APIFactors, key, "factorCredentials")
	if err != nil {
		return apitypes.FactorCredentialName{}, err
	}
	return apitypes.ParseFactorCredentialName(value)
}
func (n authResourceNames) resolveFactor(ctx context.Context, name apitypes.FactorCredentialName) (contracts.AccountID, contracts.FactorCredentialID, error) {
	account, err := n.resolveAccount(ctx, name.Account())
	if err != nil {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, err
	}
	id, err := n.store.ResolveAPIResourceIdentity(ctx, storage.APIFactors, name.ResourceID())
	if err != nil {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, err
	}
	owner, factor, err := storage.ParseFactorAPIIdentityKey(id.NativeKey())
	if err != nil {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, err
	}
	if owner != account {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, storage.ErrAPIResourceNotFound
	}
	return owner, factor, nil
}
func (n authResourceNames) mcpKey(ctx context.Context, owner storage.MCPKeyOwner) (apitypes.McpKeyName, error) {
	key, err := owner.Key.Value()
	if err != nil {
		return apitypes.McpKeyName{}, err
	}
	value, err := n.child(ctx, owner.Account, storage.APIMcpKeys, key, "mcpKeys")
	if err != nil {
		return apitypes.McpKeyName{}, err
	}
	return apitypes.ParseMcpKeyName(value)
}
func (n authResourceNames) resolveMCPKey(ctx context.Context, name apitypes.McpKeyName) (storage.MCPKeyOwner, error) {
	account, err := n.resolveAccount(ctx, name.Account())
	if err != nil {
		return storage.MCPKeyOwner{}, err
	}
	id, err := n.store.ResolveAPIResourceIdentity(ctx, storage.APIMcpKeys, name.ResourceID())
	if err != nil {
		return storage.MCPKeyOwner{}, err
	}
	key, err := contracts.ParseMcpKeyID(id.NativeKey())
	if err != nil {
		return storage.MCPKeyOwner{}, storage.ErrAPIResourceIntegrity
	}
	owner, err := n.store.MCPKeyOwner(ctx, key)
	if err != nil {
		return storage.MCPKeyOwner{}, err
	}
	if owner.Account != account {
		return storage.MCPKeyOwner{}, storage.ErrAPIResourceNotFound
	}
	return owner, nil
}
