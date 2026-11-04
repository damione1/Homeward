package server

import (
	"context"
	"crypto/cipher"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

// The v2 resource boundary uses the same native service and mapping owner. It
// never broadens an INTEGRATIONS ceiling to GLOBAL_CONFIGURATION.
type integrationHandler struct {
	native     *integrationProjection
	authority  *authorization.Authority
	operations *configurationOperationProvider
	pages      cipher.AEAD
	now        func() time.Time
}

func (h *integrationHandler) permit(ctx context.Context, operation auth.ScopeOperation) (*authorization.Permit, error) {
	if h == nil || h.native == nil || h.native.service == nil || h.authority == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	actor, present := ActorFromContext(ctx)
	admitted, err := principal.Actor()
	if err != nil || !present || actor != admitted {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, operation)
	if err != nil {
		return nil, integrationFault(err)
	}
	object, err := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS)
	if err != nil {
		return nil, integrationFault(err)
	}
	permit, err := h.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, integrationFault(err)
	}
	if !permit.Ready() {
		permit.Release()
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	return permit, nil
}

func (h *integrationHandler) GetIntegrationType(ctx context.Context, request *connect.Request[api.GetIntegrationTypeRequest]) (response *connect.Response[api.IntegrationType], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if request == nil || request.Msg == nil {
		return nil, integrationInvalidField(&api.GetIntegrationTypeRequest{}, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	name, err := apitypes.ParseIntegrationTypeName(request.Msg.Name)
	if err != nil {
		return nil, integrationInvalidField(&api.GetIntegrationTypeRequest{}, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	if err := h.native.names.parent(permit.Context(), name.Home().String()); err != nil {
		return nil, integrationFault(err)
	}
	types, err := h.native.service.Types(permit.Context())
	if err != nil {
		return nil, integrationFault(err)
	}
	for _, native := range types {
		value, err := integrationTypeResource(name.Home(), native)
		if err != nil {
			return nil, integrationFault(err)
		}
		if contracts.AdmitIntegrationType(value, name.Home()) != nil {
			return nil, integrationResourceIntegrity()
		}
		if value.Name == name.String() {
			if !permit.Ready() {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
			}
			return connect.NewResponse(value), nil
		}
	}
	return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
}

func (h *integrationHandler) GetIntegrationInstance(ctx context.Context, request *connect.Request[api.GetIntegrationInstanceRequest]) (response *connect.Response[api.IntegrationInstance], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if request == nil || request.Msg == nil {
		return nil, integrationInvalidField(&api.GetIntegrationInstanceRequest{}, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	name, err := apitypes.ParseIntegrationInstanceName(request.Msg.Name)
	if err != nil {
		return nil, integrationInvalidField(&api.GetIntegrationInstanceRequest{}, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	key, err := h.native.names.resolve(permit.Context(), storage.APIIntegrationInstances, name.String())
	if err != nil {
		return nil, integrationFault(err)
	}
	definition, snapshot, err := h.native.service.Instance(permit.Context(), key)
	if err != nil {
		return nil, integrationFault(err)
	}
	value, err := h.native.integrationResource(permit.Context(), name.Home(), definition, integrationSnapshotVersion(snapshot), true)
	if err != nil {
		return nil, integrationFault(err)
	}
	if contracts.AdmitIntegrationInstance(value, name.Home()) != nil {
		return nil, integrationResourceIntegrity()
	}
	if !permit.Ready() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	return connect.NewResponse(value), nil
}
