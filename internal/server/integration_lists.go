package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

const integrationListResponseBytes = 512 << 10
const integrationCaptureBytes = 16 << 20
const integrationCaptureResources = 4096

func (h *integrationHandler) ListIntegrationTypes(ctx context.Context, request *connect.Request[api.ListIntegrationTypesRequest]) (response *connect.Response[api.ListIntegrationTypesResponse], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	invalid := func(field string, reason api.ValidationReason) error {
		return integrationInvalidField(&api.ListIntegrationTypesRequest{}, field, reason)
	}
	if request == nil || request.Msg == nil {
		return nil, invalid("parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	input := request.Msg
	home, err := apitypes.ParseHomeName(input.Parent)
	if err != nil {
		return nil, invalid("parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	if input.PageSize < 0 {
		return nil, invalid("page_size", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE)
	}
	if err := h.native.names.parent(permit.Context(), home.String()); err != nil {
		return nil, integrationFault(err)
	}
	binding, err := h.pageBinding(permit.Context(), home, integrationTypesPage)
	if err != nil {
		return nil, err
	}
	native, err := h.native.service.Types(permit.Context())
	if err != nil {
		return nil, integrationFault(err)
	}
	if len(native) > integrationCaptureResources {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	resources := make([]*api.IntegrationType, 0, len(native))
	total := 0
	for _, value := range native {
		resource, err := integrationTypeResource(home, value)
		if err != nil {
			return nil, err
		}
		if contracts.AdmitIntegrationType(resource, home) != nil {
			return nil, integrationResourceIntegrity()
		}
		total += proto.Size(resource)
		if total > integrationCaptureBytes {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		}
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Name < resources[j].Name })
	names, etags := make([]string, len(resources)), make([]string, len(resources))
	for i, value := range resources {
		names[i] = value.Name
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(value)
		if err != nil {
			return nil, integrationResourceIntegrity()
		}
		digest := sha256.Sum256(raw)
		etags[i] = hex.EncodeToString(digest[:])
		if i > 0 && names[i] == names[i-1] {
			return nil, integrationResourceIntegrity()
		}
	}
	cursor, err := h.decodePage(input.PageToken, integrationTypesPage, home, binding, integrationPageCapture(names, etags))
	if err != nil {
		return nil, err
	}
	result := &api.ListIntegrationTypesResponse{}
	size, used := integrationPageSize(input.PageSize), 0
	for index, value := range resources {
		if value.Name <= cursor.After {
			continue
		}
		bytes := proto.Size(value) + 16
		if len(result.IntegrationTypes) >= size || used+bytes > integrationListResponseBytes-2048 {
			if len(result.IntegrationTypes) == 0 {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
			}
			result.NextPageToken, err = h.encodePage(cursor, resources[index-1].Name)
			if err != nil {
				return nil, err
			}
			break
		}
		result.IntegrationTypes = append(result.IntegrationTypes, value)
		used += bytes
	}
	current, err := h.pageBinding(permit.Context(), home, integrationTypesPage)
	if err != nil || !bytes.Equal(current, binding) || !permit.Ready() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	return connect.NewResponse(result), nil
}

func (h *integrationHandler) ListIntegrationInstances(ctx context.Context, request *connect.Request[api.ListIntegrationInstancesRequest]) (response *connect.Response[api.ListIntegrationInstancesResponse], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, integrationFault(callErr))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, configurationOperationReadTimeout)
	defer cancel()
	permit, err := h.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	invalid := func(field string, reason api.ValidationReason) error {
		return integrationInvalidField(&api.ListIntegrationInstancesRequest{}, field, reason)
	}
	if request == nil || request.Msg == nil {
		return nil, invalid("parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	if contracts.AdmitIntegrationRequestShape(request.Msg) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	input := request.Msg
	home, err := apitypes.ParseHomeName(input.Parent)
	if err != nil {
		return nil, invalid("parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	if input.PageSize < 0 {
		return nil, invalid("page_size", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE)
	}
	if err := h.native.names.parent(permit.Context(), home.String()); err != nil {
		return nil, integrationFault(err)
	}
	binding, err := h.pageBinding(permit.Context(), home, integrationInstancesPage)
	if err != nil {
		return nil, err
	}
	native, snapshot, err := h.native.service.Instances(permit.Context())
	if err != nil {
		return nil, integrationFault(err)
	}
	if len(native) > integrationCaptureResources {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	resources := make([]*api.IntegrationInstance, 0, len(native))
	total := 0
	for _, value := range native {
		resource, err := h.native.integrationResource(permit.Context(), home, value, integrationSnapshotVersion(snapshot), true)
		if err != nil {
			return nil, err
		}
		if contracts.AdmitIntegrationInstance(resource, home) != nil {
			return nil, integrationResourceIntegrity()
		}
		total += proto.Size(resource)
		if total > integrationCaptureBytes {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		}
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Name < resources[j].Name })
	names, etags := make([]string, len(resources)), make([]string, len(resources))
	for i, value := range resources {
		names[i], etags[i] = value.Name, value.Etag
		if i > 0 && names[i] == names[i-1] {
			return nil, integrationResourceIntegrity()
		}
	}
	cursor, err := h.decodePage(input.PageToken, integrationInstancesPage, home, binding, integrationPageCapture(names, etags))
	if err != nil {
		return nil, err
	}
	result := &api.ListIntegrationInstancesResponse{}
	size, used := integrationPageSize(input.PageSize), 0
	for index, value := range resources {
		if value.Name <= cursor.After {
			continue
		}
		bytes := proto.Size(value) + 16
		if len(result.IntegrationInstances) >= size || used+bytes > integrationListResponseBytes-2048 {
			if len(result.IntegrationInstances) == 0 {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
			}
			result.NextPageToken, err = h.encodePage(cursor, resources[index-1].Name)
			if err != nil {
				return nil, err
			}
			break
		}
		result.IntegrationInstances = append(result.IntegrationInstances, value)
		used += bytes
	}
	current, err := h.pageBinding(permit.Context(), home, integrationInstancesPage)
	if err != nil || !bytes.Equal(current, binding) || !permit.Ready() {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	return connect.NewResponse(result), nil
}

func integrationPageSize(value int32) int {
	if value == 0 {
		return 100
	}
	if value > 500 {
		return 500
	}
	return int(value)
}
