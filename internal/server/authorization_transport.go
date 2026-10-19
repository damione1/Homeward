package server

import (
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/authorization"
)

// BindPermission binds an already registered protected route to a known
// business permission. It is trusted composition, never a transport input.
// Handlers must still evaluate actual objects with the same authority.
func (p *PublicPlan) BindPermission(kind RouteKind, method RouteMethod, path string, permission authorization.Permission) error {
	if p == nil {
		return ErrInvalidPublicPlan
	}
	if _, _, err := permission.Values(); err != nil {
		return ErrInvalidPublicPlan
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.valid || p.routes == nil {
		return ErrInvalidPublicPlan
	}
	if p.frozen {
		return ErrPublicPlanFrozen
	}
	key := routeLookup{method: method.httpMethod(), path: path}
	route, ok := p.routes[key]
	if !ok || route.key.kind != kind || route.access != RouteAccessProtected {
		return ErrInvalidPublicPlan
	}
	route.key.permission = permission
	p.routes[key] = route
	return nil
}

// The native provider has no prefix, family-name, or arbitrary-path fallback.
func nativeRoutePermission(route RouteKey) (authorization.Permission, bool, bool) {
	if _, _, err := route.permission.Values(); err == nil {
		return route.permission, false, true
	}
	if route.Kind() != RouteKindConnect || route.Method() != RouteMethodPost {
		return authorization.Permission{}, false, false
	}
	family := authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS
	var operation authv1.ScopeOperation
	switch route.Path() {
	case authv1connect.AuthServiceListSessionsProcedure, authv1connect.AuthServiceRevokeSessionProcedure:
		return authorization.Permission{}, true, true // self ownership is rechecked by the session service/SQL
	case authv1connect.AccountServiceGetAccountProcedure, authv1connect.AccountServiceListAccountsProcedure, authv1connect.AccountServiceListInvitationsProcedure, authv1connect.AuthorizationPolicyServiceGetPolicyProcedure:
		operation = authv1.ScopeOperation_SCOPE_OPERATION_READ
	case authv1connect.AccountServiceCreateAccountProcedure, authv1connect.AccountServiceCreateInvitationProcedure, authv1connect.AccountServiceRevokeInvitationProcedure:
		operation = authv1.ScopeOperation_SCOPE_OPERATION_WRITE
	case authv1connect.AccountServiceUpdateAccountProcedure, authv1connect.AccountServiceDeleteAccountProcedure, authv1connect.AuthorizationPolicyServiceReplacePolicyProcedure:
		operation = authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER
	case statev1connect.StateServiceOpenViewProcedure, statev1connect.StateServiceReadPageProcedure, statev1connect.StateServiceWatchViewProcedure:
		family = authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY
		operation = authv1.ScopeOperation_SCOPE_OPERATION_READ
	default:
		return authorization.Permission{}, false, false
	}
	permission, err := authorization.NewPermission(family, operation)
	return permission, false, err == nil
}
