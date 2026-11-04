package server

import (
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
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
	case apiv2connect.HomeServiceGetHomeProcedure, apiv2connect.HomeServiceListHomesProcedure:
		return authorization.Permission{}, true, true
	case apiv2connect.McpKeyServiceGetMcpKeyProcedure, apiv2connect.AuthServiceGetSessionProcedure, apiv2connect.AuthServiceGetFactorCredentialProcedure, apiv2connect.McpKeyServiceIssueMcpKeyProcedure, apiv2connect.McpKeyServiceListMcpKeysProcedure, apiv2connect.McpKeyServiceRevokeMcpKeyProcedure, apiv2connect.AuthServiceListSessionsProcedure, apiv2connect.AuthServiceRevokeSessionProcedure, apiv2connect.AuthServiceListFactorCredentialsProcedure, apiv2connect.AuthServiceRemoveFactorCredentialProcedure, apiv2connect.AuthServiceReplaceRecoveryCodesProcedure:
		return authorization.Permission{}, true, true // self ownership is rechecked by the session service/SQL
	case apiv2connect.AccountServiceGetInvitationProcedure, apiv2connect.AccountServiceGetAccountProcedure, apiv2connect.AccountServiceListAccountsProcedure, apiv2connect.AccountServiceListInvitationsProcedure, apiv2connect.AuthorizationPolicyServiceGetAuthorizationPolicyProcedure, apiv2connect.AccountServiceGetSecondFactorPolicyProcedure:
		operation = authv1.ScopeOperation_SCOPE_OPERATION_READ
	case apiv2connect.AccountServiceCreateAccountProcedure, apiv2connect.AccountServiceIssueInvitationProcedure, apiv2connect.AccountServiceRevokeInvitationProcedure:
		operation = authv1.ScopeOperation_SCOPE_OPERATION_WRITE
	case apiv2connect.AccountServiceUpdateAccountProcedure, apiv2connect.AccountServiceDeleteAccountProcedure, apiv2connect.AuthorizationPolicyServiceReplaceAuthorizationPolicyProcedure, apiv2connect.AccountServiceChangeSecondFactorPolicyProcedure:
		operation = authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER
	case apiv2connect.EntityViewServiceCreateEntityViewProcedure, apiv2connect.EntityViewServiceGetEntityViewProcedure, apiv2connect.EntityViewServiceListEntityViewsProcedure, apiv2connect.EntityViewServiceDeleteEntityViewProcedure, apiv2connect.EntityViewServiceQueryEntityViewProcedure, apiv2connect.EntityViewServiceWatchEntityViewProcedure:
		family = authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY
		operation = authv1.ScopeOperation_SCOPE_OPERATION_READ
	default:
		return authorization.Permission{}, false, false
	}
	permission, err := authorization.NewPermission(family, operation)
	return permission, false, err == nil
}
