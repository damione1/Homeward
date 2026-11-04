package server

import (
	"connectrpc.com/connect"
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

func (h *accountResourceHandler) GetInvitation(ctx context.Context, request *connect.Request[api.GetInvitationRequest]) (*connect.Response[api.Invitation], error) {
	admission, err := admitAuthAdministrativeRead(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer admission.close()
	ctx = admission.permit.Context()
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetInvitationRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseInvitationName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	id, err := h.names.resolveInvitation(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.GetInvitation(ctx, admission.actor, id)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	value, err := h.names.invitationResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return authReadResponse(ctx, admission, value)
}
func (h *authResourceHandler) GetSession(ctx context.Context, request *connect.Request[api.GetSessionRequest]) (*connect.Response[api.Session], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetSessionRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	name, err := apitypes.ParseSessionName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	target, err := authAccountTarget(ctx, h.coordinator, h.names, name.Account().String(), true, native.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	var admission *authReadAdmission
	if actor != target {
		admission, err = admitAuthAdministrativeRead(ctx, h.coordinator)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		defer admission.close()
		ctx = admission.permit.Context()
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	owner, err := h.names.resolveSession(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.GetSession(ctx, actor, owner, h.coordinator.clock.Now())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	value, err := h.names.sessionResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if admission != nil {
		return authReadResponse(ctx, admission, value)
	}
	return connect.NewResponse(value), nil
}
func (h *authResourceHandler) GetFactorCredential(ctx context.Context, request *connect.Request[api.GetFactorCredentialRequest]) (*connect.Response[api.FactorCredential], error) {
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetFactorCredentialRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	name, err := apitypes.ParseFactorCredentialName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	parent, err := h.names.account(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if name.Account() != parent {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	account, factor, err := h.names.resolveFactor(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if account != owner.Account {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	if h.coordinator.mfa == nil {
		return nil, authAPIError(ctx, storage.ErrAccountUnavailable)
	}
	record, found, err := h.coordinator.mfa.repository.GetFactor(ctx, owner, factor, h.coordinator.clock.Now())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if !found {
		return nil, authAPIError(ctx, storage.ErrAPIResourceNotFound)
	}
	value, err := h.names.factorResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(value), nil
}
func (h *mcpKeyResourceHandler) GetMcpKey(ctx context.Context, request *connect.Request[api.GetMcpKeyRequest]) (*connect.Response[api.McpKey], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetMcpKeyRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	name, err := apitypes.ParseMcpKeyName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	target, err := authAccountTarget(ctx, h.coordinator, h.names, name.Account().String(), true, native.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	var admission *authReadAdmission
	if actor != target {
		admission, err = admitAuthAdministrativeRead(ctx, h.coordinator)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		defer admission.close()
		ctx = admission.permit.Context()
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	owner, err := h.names.resolveMCPKey(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.GetMCPKey(ctx, actor, owner)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	value, err := h.names.mcpKeyResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if admission != nil {
		return authReadResponse(ctx, admission, value)
	}
	return connect.NewResponse(value), nil
}
