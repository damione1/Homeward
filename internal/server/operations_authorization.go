package server

import (
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

// Operations multiplex native physical and configuration owners. The route
// accepts either existing coarse ceiling; the selected owner must independently
// authorize its actual target and original caller before exposing anything.
// No credential ceiling, role, object grant or scope is constructed here.
func authorizeOperationRoute(current authorization.Current, route RouteKey) (bool, error) {
	if route.Kind() != RouteKindConnect || route.Method() != RouteMethodPost {
		return false, nil
	}
	switch route.Path() {
	case longrunningpbconnect.OperationsGetOperationProcedure, longrunningpbconnect.OperationsListOperationsProcedure, longrunningpbconnect.OperationsWaitOperationProcedure, longrunningpbconnect.OperationsCancelOperationProcedure, longrunningpbconnect.OperationsDeleteOperationProcedure:
		read, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_READ)
		integrationAdmin, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
		for _, permission := range []authorization.Permission{read, homeAdminPermission(), integrationAdmin} {
			if current.AdmitRoute(permission) == nil {
				return true, nil
			}
		}
		return true, authorization.ErrDenied
	default:
		return false, nil
	}
}
