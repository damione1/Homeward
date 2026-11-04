package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
)

func (h *authResourceHandler) ListSessions(ctx context.Context, request *connect.Request[api.ListSessionsRequest]) (*connect.Response[api.ListSessionsResponse], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ListSessionsRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	target, err := authAccountTarget(ctx, h.coordinator, h.names, r.Parent, true, native.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	var administrative *authReadAdmission
	if actor != target {
		administrative, err = admitAuthAdministrativeRead(ctx, h.coordinator)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		defer administrative.close()
		ctx = administrative.permit.Context()
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	size, err := authPageSize(r, r.PageSize, r.PageToken)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	records, err := h.store.ListSessions(ctx, actor, target, h.coordinator.clock.Now())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	values := make([]*api.Session, 0, len(records))
	for _, record := range records {
		value, err := h.names.sessionResource(ctx, record)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
		values = append(values, value)
	}
	page, next, err := authPage(ctx, h.coordinator, h.pages, r, r.Parent, "ListSessions", r.PageToken, size, values)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	response := &api.ListSessionsResponse{Sessions: page, NextPageToken: next}
	if administrative != nil {
		return authReadResponse(ctx, administrative, response)
	}
	return connect.NewResponse(response), nil
}

func (h *authResourceHandler) RevokeSession(ctx context.Context, request *connect.Request[api.RevokeSessionRequest]) (*connect.Response[api.RevokeSessionResponse], error) {
	actor, err := authInteractiveActor(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.RevokeSessionRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	name, err := apitypes.ParseSessionName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	if _, err := authAccountTarget(ctx, h.coordinator, h.names, name.Account().String(), true, native.ScopeOperation_SCOPE_OPERATION_ADMINISTER); err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	owner, err := h.names.resolveSession(ctx, name)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer guard.release()
	err = h.store.RevokeSession(ctx, actor, owner, h.coordinator.clock.Now())
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard.cancelLeases(ctx, &owner.Session)
	return connect.NewResponse(&api.RevokeSessionResponse{}), nil
}
