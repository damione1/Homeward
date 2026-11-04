package server

import (
	"context"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/view"
)

func preparedCapabilities(page view.Page) ([]apiresources.Capability, error) {
	if len(page.Items) != 1 {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	values, prepared := page.Items[0].ProjectedCapabilities()
	if !prepared {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	return values, nil
}

func (h *homeResourceHandler) GetCapability(ctx context.Context, request *connect.Request[api.GetCapabilityRequest]) (*connect.Response[api.Capability], error) {
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.GetCapabilityRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	name, err := apitypes.ParseCapabilityName(request.Msg.Name)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	page, err := h.entityPage(ctx, name.Entity(), current)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, _ := coreRPCActor(ctx)
	defer h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken)
	values, err := preparedCapabilities(page)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	for _, capability := range values {
		resource := capability.Resource()
		if resource.Name == name.String() {
			return connect.NewResponse(resource), nil
		}
	}
	return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
}

func (h *homeResourceHandler) ListCapabilities(ctx context.Context, request *connect.Request[api.ListCapabilitiesRequest]) (*connect.Response[api.ListCapabilitiesResponse], error) {
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.ListCapabilitiesRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	parent, err := apitypes.ParseEntityName(r.Parent)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	size, err := homePageSize(r, r.PageSize)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	binding, err := homePageBinding(current, parent.String(), "", "ListCapabilities")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	var page view.Page
	offset := 0
	if r.PageToken == "" {
		page, err = h.entityPage(ctx, parent, current)
	} else {
		var cursor homePageCursor
		cursor, err = h.openPage(r.PageToken, binding)
		if err == nil && (cursor.Page != "" || cursor.Offset <= 0) {
			err = view.ErrPageToken
		}
		if err == nil {
			offset = cursor.Offset
			page, err = h.views.ReadFirstPage(ctx, actor, cursor.Snapshot, 1)
		}
	}
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	retain := r.PageToken != ""
	defer func() {
		if !retain {
			_ = h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken)
		}
	}()
	values, err := preparedCapabilities(page)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if offset > len(values) {
		return nil, apiRPCError(ctx, view.ErrPageToken)
	}
	end := min(offset+size, len(values))
	out := &api.ListCapabilitiesResponse{}
	for _, capability := range values[offset:end] {
		out.Capabilities = append(out.Capabilities, capability.Resource())
	}
	if end < len(values) {
		out.NextPageToken, err = h.sealPage(homePageCursor{Snapshot: page.SnapshotToken, Offset: end}, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	// A capture remains available until its bounded TTL even after the last page:
	// a lost response may be retried with the same opaque continuation.
	retain = retain || out.NextPageToken != ""
	return connect.NewResponse(out), nil
}
