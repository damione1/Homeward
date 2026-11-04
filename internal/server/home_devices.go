package server

import (
	"context"
	"net/http"
	"sort"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/entityfilter"
	"github.com/damione1/Homeward/internal/view"
)

func registerDeviceRoutes(plan *PublicPlan, handler *homeResourceHandler) error {
	if plan == nil || handler == nil || handler.views == nil {
		return ErrInvalidPublicPlan
	}
	paths := []string{apiv2connect.DeviceServiceGetDeviceProcedure, apiv2connect.DeviceServiceListDevicesProcedure}
	procedures := make([]ConnectProcedure, 0, len(paths))
	for _, path := range paths {
		procedures = append(procedures, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewDeviceServiceHandler(handler, options...)
	}, procedures...); err != nil {
		return err
	}
	for _, path := range paths {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, path, homeReadPermission()); err != nil {
			return err
		}
	}
	return nil
}

func (h *homeResourceHandler) GetDevice(ctx context.Context, request *connect.Request[api.GetDeviceRequest]) (*connect.Response[api.Device], error) {
	ctx, finishResponse, responseErr := h.beginReadResponse(ctx)
	if responseErr != nil {
		return nil, apiRPCError(ctx, responseErr)
	}
	defer finishResponse()
	permission := homeReadPermission()
	if _, _, err := h.current(ctx, &permission); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.GetDeviceRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	name, err := apitypes.ParseDeviceName(request.Msg.Name)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	home, err := h.owner.HomeName(ctx)
	if err != nil || home != name.Home() {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	filter, err := entityfilter.Parse(`device("` + name.String() + `")`)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	page, err := h.views.OpenEntityView(ctx, actor, home, filter, 1)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	defer releaseReadResponse(ctx, func() { _ = h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken) })
	if len(page.Items) == 0 {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	if err := h.retainReadCapture(ctx, page.SnapshotToken); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(&api.Device{Name: name.String()}), nil
}

func (h *homeResourceHandler) ListDevices(ctx context.Context, request *connect.Request[api.ListDevicesRequest]) (*connect.Response[api.ListDevicesResponse], error) {
	ctx, finishResponse, responseErr := h.beginReadResponse(ctx)
	if responseErr != nil {
		return nil, apiRPCError(ctx, responseErr)
	}
	defer finishResponse()
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.ListDevicesRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	parent, err := apitypes.ParseHomeName(r.Parent)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	home, err := h.owner.HomeName(ctx)
	if err != nil || home != parent {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	size, err := homePageSize(r, r.PageSize)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	binding, err := homePageBinding(current, parent.String(), "", "ListDevices")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	var page view.Page
	offset := 0
	if r.PageToken != "" {
		cursor, openErr := h.openPage(r.PageToken, binding)
		if openErr != nil {
			return nil, apiRPCError(ctx, openErr)
		}
		if cursor.Page != "" || cursor.Offset < 1 {
			return nil, apiRPCError(ctx, view.ErrPageToken)
		}
		offset = cursor.Offset
		page, err = h.views.ReadFirstPage(ctx, actor, cursor.Snapshot, 500)
	} else {
		selected, _ := entityfilter.Parse("")
		page, err = h.views.OpenEntityView(ctx, actor, home, selected, 500)
	}
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	snapshot := page.SnapshotToken
	retain := r.PageToken != ""
	defer func() {
		if !retain {
			releaseReadResponse(ctx, func() { _ = h.views.ReleaseView(context.WithoutCancel(ctx), actor, snapshot) })
		}
	}()
	// Work is bounded by the existing native capture's item/byte budgets. Each
	// read rechecks authority; this set contains only authorized associations.
	names := make(map[string]struct{})
	for {
		for _, item := range page.Items {
			device, _, idErr := item.Registry().IDs()
			if idErr != nil {
				return nil, apiRPCError(ctx, idErr)
			}
			name, nameErr := apitypes.DeviceNameForIdentity(home, device)
			if nameErr != nil {
				return nil, apiRPCError(ctx, nameErr)
			}
			names[name.String()] = struct{}{}
		}
		if page.NextPageToken == "" {
			break
		}
		page, err = h.views.ReadPageSize(ctx, actor, snapshot, page.NextPageToken, 500)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	if offset > len(ordered) {
		return nil, apiRPCError(ctx, view.ErrPageToken)
	}
	end := min(offset+size, len(ordered))
	out := &api.ListDevicesResponse{}
	for _, name := range ordered[offset:end] {
		out.Devices = append(out.Devices, &api.Device{Name: name})
	}
	if end < len(ordered) {
		out.NextPageToken, err = h.sealPage(homePageCursor{Snapshot: snapshot, Offset: end}, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	retain = retain || out.NextPageToken != ""
	if err := h.retainReadCapture(ctx, snapshot); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(out), nil
}
