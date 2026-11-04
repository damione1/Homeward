package server

import (
	"context"
	"crypto/cipher"
	"errors"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
)

type accountResourceHandler struct {
	*accountHandler
	names authResourceNames
	pages cipher.AEAD
}

func (h *accountResourceHandler) CreateAccount(ctx context.Context, request *connect.Request[api.CreateAccountRequest]) (*connect.Response[api.Account], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil || request.Msg.Account == nil {
		return nil, authAPIError(ctx, authInvalid(&api.CreateAccountRequest{}, "account", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := h.names.parent(ctx, r.Parent); err != nil {
		return nil, authAPIError(ctx, err)
	}
	role, err := nativeAccountRole(r.Account.Role)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "account.role", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE))
	}
	if r.Account.Access != api.AccountAccess_ACCOUNT_ACCESS_UNSPECIFIED && r.Account.Access != api.AccountAccess_ACCOUNT_ACCESS_ENABLED {
		return nil, authAPIError(ctx, authInvalid(r, "account.access", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE))
	}
	input, err := newAccountInput(ctx, r.Account.LoginName, r.Account.DisplayName, r.Account.InitialPassword, role)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if r.AccountId != "" {
		chosen, parseErr := storage.ParseAPIRequestedResourceID(r.AccountId)
		if parseErr != nil {
			return nil, authAPIError(ctx, authInvalid(r, "account_id", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
		}
		input.PublicID = &chosen
	}
	guard, err := h.accountCreationGuard(ctx, input.ID)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.CreateAccount(ctx, actor, input)
	if err := finishAccountMutation(guard, ctx, err, false); err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.accountResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(resource), nil
}

func (h *accountResourceHandler) GetAccount(ctx context.Context, request *connect.Request[api.GetAccountRequest]) (*connect.Response[api.Account], error) {
	admission, err := admitAuthAdministrativeRead(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer admission.close()
	ctx = admission.permit.Context()
	actor := admission.actor
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetAccountRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseAccountName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	target, err := h.names.resolveAccount(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.GetAccount(ctx, actor, target)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.accountResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return authReadResponse(ctx, admission, resource)
}

func (h *accountResourceHandler) ListAccounts(ctx context.Context, request *connect.Request[api.ListAccountsRequest]) (*connect.Response[api.ListAccountsResponse], error) {
	admission, err := admitAuthAdministrativeRead(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer admission.close()
	ctx = admission.permit.Context()
	actor := admission.actor
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ListAccountsRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := h.names.parent(ctx, r.Parent); err != nil {
		return nil, authAPIError(ctx, err)
	}
	size, err := authPageSize(r, r.PageSize, r.PageToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	records, err := h.store.ListAccounts(ctx, actor)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values := make([]*api.Account, 0, len(records))
	for _, record := range records {
		resource, err := h.names.accountResource(ctx, record)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		values = append(values, resource)
	}
	page, next, err := authPage(ctx, h.coordinator, h.pages, r, r.Parent, "ListAccounts", r.PageToken, size, values)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return authReadResponse(ctx, admission, &api.ListAccountsResponse{Accounts: page, NextPageToken: next})
}

func (h *accountResourceHandler) UpdateAccount(ctx context.Context, request *connect.Request[api.UpdateAccountRequest]) (*connect.Response[api.Account], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil || request.Msg.Account == nil {
		return nil, authAPIError(ctx, authInvalid(&api.UpdateAccountRequest{}, "account", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseAccountName(r.Account.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "account.name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	target, err := h.names.resolveAccount(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	current, err := h.store.GetAccount(ctx, actor, target)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	changes, err := accountAPIChanges(r, current)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.accountMutationGuard(ctx, target)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	outcome, err := h.store.UpdateAccountWithOutcome(ctx, actor, target, changes)
	var record storage.AccountRecord
	var rightsChanged bool
	if err == nil {
		record, rightsChanged, err = outcome.Result()
		if err != nil {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
			guard.release()
			return nil, authAPIError(ctx, err)
		}
	}
	if err := finishAccountMutation(guard, ctx, err, rightsChanged); err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.accountResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(resource), nil
}

func (h *accountResourceHandler) DeleteAccount(ctx context.Context, request *connect.Request[api.DeleteAccountRequest]) (*connect.Response[emptypb.Empty], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.DeleteAccountRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseAccountName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	expected, err := storage.ParseAccountVersion(r.Etag)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "etag", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	target, err := h.names.resolveAccount(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.accountMutationGuard(ctx, target)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	err = h.store.DeleteAccountIfCurrent(ctx, actor, target, expected, r.Force)
	if err := finishAccountMutation(guard, ctx, err, true); err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (h *accountResourceHandler) IssueInvitation(ctx context.Context, request *connect.Request[api.IssueInvitationRequest]) (*connect.Response[api.IssueInvitationResponse], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.IssueInvitationRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := h.names.parent(ctx, r.Parent); err != nil {
		return nil, authAPIError(ctx, err)
	}
	login, err := accounts.ParseLoginName(r.LoginName)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "login_name", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	role, err := nativeAccountRole(r.Role)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "role", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE))
	}
	if r.ExpireTime == nil || r.ExpireTime.CheckValid() != nil || !r.ExpireTime.AsTime().After(h.coordinator.clock.Now()) {
		return nil, authAPIError(ctx, authInvalid(r, "expire_time", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE))
	}
	id, err := accounts.NewInvitationID()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	token, err := accounts.NewInvitationToken()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	digest, err := token.Digest()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.CreateInvitation(ctx, actor, storage.NewInvitation{ID: id, Login: login, Role: role, Digest: digest, ExpiresAt: r.ExpireTime.AsTime()})
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.invitationResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	secret, err := token.Text()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.IssueInvitationResponse{Invitation: resource, InvitationToken: secret}), nil
}

func (h *accountResourceHandler) AcceptInvitation(ctx context.Context, request *connect.Request[api.AcceptInvitationRequest]) (*connect.Response[api.AcceptInvitationResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, errSessionUnauthenticated)
	}
	r := request.Msg
	token, err := accounts.ParseInvitationToken(r.InvitationToken)
	if err != nil {
		return nil, authAPIError(ctx, errSessionUnauthenticated)
	}
	digest, err := token.Digest()
	if err != nil {
		return nil, authAPIError(ctx, errSessionUnauthenticated)
	}
	if err := h.store.AdmitInvitationBearer(ctx, digest); err != nil {
		if errors.Is(err, storage.ErrInvitationUnavailable) {
			err = errSessionUnauthenticated
		}
		return nil, authAPIError(ctx, err)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	display, err := accounts.ParseDisplayName(r.DisplayName)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "display_name", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	verifier, err := accounts.HashNewPassword(ctx, r.Password)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	id, err := accounts.NewAccountID()
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.accountCreationGuard(ctx, id)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.AcceptInvitation(ctx, digest, id, display, verifier)
	if err := finishAccountMutation(guard, ctx, err, false); err != nil {
		if errors.Is(err, storage.ErrInvitationNotFound) || errors.Is(err, storage.ErrInvitationExpired) || errors.Is(err, storage.ErrInvitationUnavailable) {
			err = errSessionUnauthenticated
		}
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.accountResource(ctx, record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.AcceptInvitationResponse{Account: resource}), nil
}

func (h *accountResourceHandler) ListInvitations(ctx context.Context, request *connect.Request[api.ListInvitationsRequest]) (*connect.Response[api.ListInvitationsResponse], error) {
	admission, err := admitAuthAdministrativeRead(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer admission.close()
	ctx = admission.permit.Context()
	actor := admission.actor
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ListInvitationsRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := h.names.parent(ctx, r.Parent); err != nil {
		return nil, authAPIError(ctx, err)
	}
	size, err := authPageSize(r, r.PageSize, r.PageToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	records, err := h.store.ListInvitations(ctx, actor)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values := make([]*api.Invitation, 0, len(records))
	for _, record := range records {
		value, err := h.names.invitationResource(ctx, record)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		values = append(values, value)
	}
	page, next, err := authPage(ctx, h.coordinator, h.pages, r, r.Parent, "ListInvitations", r.PageToken, size, values)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return authReadResponse(ctx, admission, &api.ListInvitationsResponse{Invitations: page, NextPageToken: next})
}

func (h *accountResourceHandler) RevokeInvitation(ctx context.Context, request *connect.Request[api.RevokeInvitationRequest]) (*connect.Response[api.RevokeInvitationResponse], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_WRITE)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.RevokeInvitationRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
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
	if err := h.store.RevokeInvitation(ctx, actor, id); err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.RevokeInvitationResponse{}), nil
}
