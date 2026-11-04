package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	boundary "github.com/damione1/Homeward/contracts/apiv2"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apimutations"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/registry"
	"google.golang.org/protobuf/proto"
)

func (h *homeResourceHandler) UpdateEntity(ctx context.Context, request *connect.Request[api.UpdateEntityRequest]) (*connect.Response[api.Entity], error) {
	permission := homeAdminPermission()
	principal, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	object, _ := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION)
	// This permit is obtained before detailed representation diagnostics. Its
	// evidence is fenced again against SQL account+policy rows before mutation.
	permit, err := h.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	defer permit.Release()
	evidence, err := permit.EntityMetadataEvidence()
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	var message *api.UpdateEntityRequest
	if request != nil {
		message = request.Msg
	}
	update, err := boundary.ParseEntityUpdate(message)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	id, err := h.resolve(ctx, update.Target(), current)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	home := update.Target().Home()
	encode := func(metadata apitypes.EntityMetadata) ([]byte, error) {
		resource, err := encodeEntity(home, metadata)
		if err != nil {
			return nil, err
		}
		return proto.MarshalOptions{Deterministic: true}.Marshal(resource)
	}
	metadata, err := h.owner.UpdateEntityMetadata(permit.Context(), evidence, id, update, encode)
	var out *api.Entity
	var mutation *apimutations.Error
	_ = errors.As(err, &mutation)
	if replay, present := mutation.ReplayResult(); present {
		out = &api.Entity{}
		if proto.Unmarshal(replay, out) != nil || out.Name != update.Target().String() {
			return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
		}
	} else if err != nil {
		return nil, apiRPCError(ctx, homeMetadataFault(err))
	} else {
		out, err = encodeEntity(home, metadata)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	if err := permit.WithCurrent(func() error { return nil }); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

func homeMetadataFault(err error) error {
	var mutation *apimutations.Error
	if errors.As(err, &mutation) {
		switch mutation.Code() {
		case apimutations.RequestIDReused:
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_REQUEST_ID_REUSED)
		case apimutations.Capacity:
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		case apimutations.StoredResultCorrupt:
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		default:
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
	}
	switch {
	case errors.Is(err, registry.ErrMetadataConflict):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
	case errors.Is(err, registry.ErrMetadataCapacity):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	case errors.Is(err, registry.ErrMetadataCommitUnknown):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN)
	case errors.Is(err, registry.ErrProjectionUnavailable):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	case errors.Is(err, registry.ErrUnconfigured):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	default:
		return err
	}
}
