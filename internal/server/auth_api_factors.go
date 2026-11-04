package server

import (
	"context"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

func (h *authResourceHandler) ListFactorCredentials(ctx context.Context, request *connect.Request[api.ListFactorCredentialsRequest]) (*connect.Response[api.ListFactorCredentialsResponse], error) {
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ListFactorCredentialsRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	parent, err := h.names.account(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if r.Parent != parent.String() {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	size, err := authPageSize(r, r.PageSize, r.PageToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if h.coordinator.mfa == nil {
		return nil, authAPIError(ctx, storage.ErrAccountUnavailable)
	}
	records, err := h.coordinator.mfa.repository.ListFactors(ctx, owner, h.coordinator.clock.Now())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values := make([]*api.FactorCredential, 0, len(records))
	for _, record := range records {
		value, err := h.names.factorResource(ctx, record)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		values = append(values, value)
	}
	page, next, err := authPage(ctx, h.coordinator, h.pages, r, r.Parent, "ListFactorCredentials", r.PageToken, size, values)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.ListFactorCredentialsResponse{FactorCredentials: page, NextPageToken: next}), nil
}

func (h *authResourceHandler) RemoveFactorCredential(ctx context.Context, request *connect.Request[api.RemoveFactorCredentialRequest]) (*connect.Response[api.RemoveFactorCredentialResponse], error) {
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.RemoveFactorCredentialRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
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
	guard, err := (&accountHandler{store: h.store, coordinator: h.coordinator}).accountMutationGuard(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	_, err = h.coordinator.mfa.repository.RemoveFactor(ctx, owner, factor, h.coordinator.clock.Now())
	if err := finishMFAMutation(guard, ctx, err); err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.RemoveFactorCredentialResponse{}), nil
}

func (h *authResourceHandler) ReplaceRecoveryCodes(ctx context.Context, request *connect.Request[api.ReplaceRecoveryCodesRequest]) (*connect.Response[api.ReplaceRecoveryCodesResponse], error) {
	owner, err := h.selfMFASession(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ReplaceRecoveryCodesRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	parent, err := h.names.account(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if r.Name != parent.String() {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if h.coordinator.mfa == nil {
		return nil, authAPIError(ctx, storage.ErrAccountUnavailable)
	}
	permit, err := h.coordinator.mfa.crypto.Acquire(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer permit.Release()
	batch, candidates, codes, err := authmfa.GenerateRecoveryBatch(owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values, err := recoveryCodesWire(codes)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	persisted, err := storage.NewMFARecoveryBatch(owner.Account, batch, candidates)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := (&accountHandler{store: h.store, coordinator: h.coordinator}).accountMutationGuard(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	_, err = h.coordinator.mfa.repository.ReplaceRecoveryCodes(ctx, owner, persisted, h.coordinator.clock.Now())
	if err := finishMFAMutation(guard, ctx, err); err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&api.ReplaceRecoveryCodesResponse{RecoveryCodes: values}), nil
}
