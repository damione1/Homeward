package server

import (
	"context"
	"crypto/cipher"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Explicit checked mappings keep the private native authority and public wire
// vocabulary separate. Unknown numeric values never become granted permissions.
var authScopeFamilies = map[api.ScopeFamily]native.ScopeFamily{
	api.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY:       native.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY,
	api.ScopeFamily_SCOPE_FAMILY_HISTORY:              native.ScopeFamily_SCOPE_FAMILY_HISTORY,
	api.ScopeFamily_SCOPE_FAMILY_MEDIA:                native.ScopeFamily_SCOPE_FAMILY_MEDIA,
	api.ScopeFamily_SCOPE_FAMILY_LOCATION:             native.ScopeFamily_SCOPE_FAMILY_LOCATION,
	api.ScopeFamily_SCOPE_FAMILY_ACTIONS:              native.ScopeFamily_SCOPE_FAMILY_ACTIONS,
	api.ScopeFamily_SCOPE_FAMILY_DASHBOARDS:           native.ScopeFamily_SCOPE_FAMILY_DASHBOARDS,
	api.ScopeFamily_SCOPE_FAMILY_CALENDARS:            native.ScopeFamily_SCOPE_FAMILY_CALENDARS,
	api.ScopeFamily_SCOPE_FAMILY_LISTS:                native.ScopeFamily_SCOPE_FAMILY_LISTS,
	api.ScopeFamily_SCOPE_FAMILY_LOGIC:                native.ScopeFamily_SCOPE_FAMILY_LOGIC,
	api.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS:         native.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS,
	api.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION: native.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION,
	api.ScopeFamily_SCOPE_FAMILY_ACCOUNTS:             native.ScopeFamily_SCOPE_FAMILY_ACCOUNTS,
	api.ScopeFamily_SCOPE_FAMILY_BACKUPS:              native.ScopeFamily_SCOPE_FAMILY_BACKUPS,
}
var authScopeOperations = map[api.ScopeOperation]native.ScopeOperation{
	api.ScopeOperation_SCOPE_OPERATION_READ:       native.ScopeOperation_SCOPE_OPERATION_READ,
	api.ScopeOperation_SCOPE_OPERATION_WRITE:      native.ScopeOperation_SCOPE_OPERATION_WRITE,
	api.ScopeOperation_SCOPE_OPERATION_ADMINISTER: native.ScopeOperation_SCOPE_OPERATION_ADMINISTER,
}

func nativeAuthScope(value *api.Scope) (authorization.Permission, error) {
	if value == nil {
		return authorization.Permission{}, mcpkeys.ErrInvalid
	}
	family, foundFamily := authScopeFamilies[value.Family]
	operation, foundOperation := authScopeOperations[value.Operation]
	if !foundFamily || !foundOperation {
		return authorization.Permission{}, mcpkeys.ErrInvalid
	}
	return authorization.NewPermission(family, operation)
}
func publicAuthScope(permission authorization.Permission) (*api.Scope, error) {
	family, operation, err := permission.Values()
	if err != nil {
		return nil, err
	}
	result := &api.Scope{}
	for public, private := range authScopeFamilies {
		if private == family {
			result.Family = public
			break
		}
	}
	for public, private := range authScopeOperations {
		if private == operation {
			result.Operation = public
			break
		}
	}
	if result.Family == api.ScopeFamily_SCOPE_FAMILY_UNSPECIFIED || result.Operation == api.ScopeOperation_SCOPE_OPERATION_UNSPECIFIED {
		return nil, storage.ErrAPIResourceIntegrity
	}
	return result, nil
}

func (n authResourceNames) mcpKeyResource(ctx context.Context, record storage.MCPKeyRecord) (*api.McpKey, error) {
	name, err := n.mcpKey(ctx, record.Owner)
	if err != nil {
		return nil, err
	}
	client, err := record.Client.Value()
	if err != nil {
		return nil, err
	}
	result := &api.McpKey{Name: name.String(), ClientName: client, Audience: api.CredentialAudience_CREDENTIAL_AUDIENCE_MCP, CreateTime: timestamppb.New(record.CreatedAt), ExpireTime: timestamppb.New(record.ExpiresAt)}
	for _, permission := range record.Scopes {
		scope, err := publicAuthScope(permission)
		if err != nil {
			return nil, err
		}
		result.Scopes = append(result.Scopes, scope)
	}
	if record.RevokedAt != nil {
		result.RevokeTime = timestamppb.New(*record.RevokedAt)
	}
	if record.LastUsedAt != nil {
		result.LastUseTime = timestamppb.New(*record.LastUsedAt)
	}
	return result, nil
}

type mcpKeyResourceHandler struct {
	store       *storage.Store
	coordinator *sessionCoordinator
	names       authResourceNames
	pages       cipher.AEAD
}

func (h *mcpKeyResourceHandler) IssueMcpKey(ctx context.Context, request *connect.Request[api.IssueMcpKeyRequest]) (*connect.Response[api.IssueMcpKeyResponse], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.IssueMcpKeyRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if _, err := authAccountTarget(ctx, h.coordinator, h.names, r.Parent, false, native.ScopeOperation_SCOPE_OPERATION_WRITE); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	client, err := mcpkeys.ParseClientName(r.ClientName)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "client_name", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	lifetime, err := contracts.AdmitMcpKeyLifetime(r.Lifetime)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "lifetime", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE))
	}
	permissions := make([]authorization.Permission, 0, len(r.Scopes))
	seen := make(map[authorization.Permission]bool, len(r.Scopes))
	for _, scope := range r.Scopes {
		permission, err := nativeAuthScope(scope)
		if err != nil || seen[permission] {
			return nil, authAPIError(ctx, authInvalid(r, "scopes", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE))
		}
		seen[permission] = true
		permissions = append(permissions, permission)
	}
	id, err := mcpkeys.NewID()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	secret, err := mcpkeys.NewSecret()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	digest, err := secret.Digest()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.coordinator.enter(ctx, actor)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer guard.release()
	record, err := h.store.CreateMCPKey(ctx, storage.MCPKeyCreation{ID: id, Account: actor, Client: client, Digest: digest, Scopes: permissions, Lifetime: lifetime, Now: h.coordinator.clock.Now()})
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.mcpKeyResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	text, err := secret.Text()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.IssueMcpKeyResponse{McpKey: resource, Secret: text}), nil
}

func (h *mcpKeyResourceHandler) ListMcpKeys(ctx context.Context, request *connect.Request[api.ListMcpKeysRequest]) (*connect.Response[api.ListMcpKeysResponse], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ListMcpKeysRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	target, err := authAccountTarget(ctx, h.coordinator, h.names, r.Parent, true, native.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	size, err := authPageSize(r, r.PageSize, r.PageToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	records, err := h.store.ListMCPKeys(ctx, actor, target)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values := make([]*api.McpKey, 0, len(records))
	for _, record := range records {
		value, err := h.names.mcpKeyResource(ctx, record)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		values = append(values, value)
	}
	page, next, err := authPage(ctx, h.coordinator, h.pages, r, r.Parent, "ListMcpKeys", r.PageToken, size, values)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.ListMcpKeysResponse{McpKeys: page, NextPageToken: next}), nil
}

func (h *mcpKeyResourceHandler) RevokeMcpKey(ctx context.Context, request *connect.Request[api.RevokeMcpKeyRequest]) (*connect.Response[api.RevokeMcpKeyResponse], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.RevokeMcpKeyRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	name, err := apitypes.ParseMcpKeyName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	if _, err := authAccountTarget(ctx, h.coordinator, h.names, name.Account().String(), true, native.ScopeOperation_SCOPE_OPERATION_ADMINISTER); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	owner, err := h.names.resolveMCPKey(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer guard.release()
	err = h.store.RevokeMCPKey(ctx, actor, owner, h.coordinator.clock.Now())
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard.cancelMCPLeases(owner.Key)
	return connect.NewResponse(&api.RevokeMcpKeyResponse{}), nil
}
