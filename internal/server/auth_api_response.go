package server

import (
	"context"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

// Administrative reads retain the native permission from before materialization
// through the HTTP response. This is not a replacement principal or authority.
type authReadAdmission struct {
	actor       contracts.AccountID
	permit      *authorization.Permit
	transferred bool
}

func admitAuthAdministrativeRead(ctx context.Context, coordinator *sessionCoordinator) (*authReadAdmission, error) {
	actor, err := authAdministrativeActor(ctx, coordinator, native.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	permission, err := authorization.NewPermission(native.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, native.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	object, err := authorization.AdministrativeObject(native.ScopeFamily_SCOPE_FAMILY_ACCOUNTS)
	if err != nil {
		return nil, err
	}
	permit, err := coordinator.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, err
	}
	return &authReadAdmission{actor: actor, permit: permit}, nil
}
func (a *authReadAdmission) close() {
	if a != nil && a.permit != nil && !a.transferred {
		a.permit.Release()
	}
}
func authReadResponse[T any](ctx context.Context, admission *authReadAdmission, message *T) (*connect.Response[T], error) {
	if admission == nil || admission.permit == nil || !admission.permit.Ready() {
		return nil, authAPIError(ctx, authorization.ErrDenied)
	}
	retained, err := retainPublicResponse(ctx, admission.permit.Context(), admission.permit.Ready, admission.permit.Release)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	admission.transferred = retained
	return connect.NewResponse(message), nil
}
