package server

import (
	"bytes"
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/entityfilter"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func encodeEntityView(value view.EntityViewDescriptor) *api.EntityView {
	return &api.EntityView{Name: value.Name.String(), Filter: value.Filter.Source(), CreateTime: timestamppb.New(value.CreateTime), ExpireTime: timestamppb.New(value.ExpireTime)}
}
func (h *homeResourceHandler) entityView(ctx context.Context, raw string) (view.EntityViewDescriptor, authorization.Current, error) {
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return view.EntityViewDescriptor{}, current, err
	}
	name, err := apitypes.ParseEntityViewName(raw)
	if err != nil {
		return view.EntityViewDescriptor{}, current, homeValidation(&api.GetEntityViewRequest{Name: raw}, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	home, err := h.owner.HomeName(ctx)
	if err != nil || name.Home() != home {
		return view.EntityViewDescriptor{}, current, authorization.ErrDenied
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return view.EntityViewDescriptor{}, current, err
	}
	descriptor, err := h.views.GetEntityView(ctx, actor, name)
	return descriptor, current, err
}
func (h *homeResourceHandler) CreateEntityView(ctx context.Context, request *connect.Request[api.CreateEntityViewRequest]) (*connect.Response[api.EntityView], error) {
	permission := homeReadPermission()
	if _, _, err := h.current(ctx, &permission); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil || request.Msg.EntityView == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.CreateEntityViewRequest{}, "entity_view", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	parent, err := apitypes.ParseHomeName(r.Parent)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	home, err := h.owner.HomeName(ctx)
	if err != nil || parent != home {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	if proto.Size(r) > 8192 {
		return nil, apiRPCError(ctx, view.ErrResourceExhausted)
	}
	selected, err := entityfilter.Parse(r.EntityView.Filter)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "entity_view.filter", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	var requested *apitypes.EntityViewName
	if r.EntityViewId != "" {
		name, parseErr := apitypes.ParseEntityViewName(home.String() + "/entityViews/" + r.EntityViewId)
		if parseErr != nil {
			return nil, apiRPCError(ctx, homeValidation(r, "entity_view_id", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
		}
		requested = &name
	}
	descriptor, err := h.views.CreateEntityView(ctx, actor, home, selected, requested)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	final, _, err := h.entityView(ctx, descriptor.Name.String())
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if final.SnapshotToken != descriptor.SnapshotToken {
		return nil, apiRPCError(ctx, view.ErrViewChanged)
	}
	return connect.NewResponse(encodeEntityView(descriptor)), nil
}
func (h *homeResourceHandler) GetEntityView(ctx context.Context, request *connect.Request[api.GetEntityViewRequest]) (*connect.Response[api.EntityView], error) {
	raw := ""
	if request != nil && request.Msg != nil {
		raw = request.Msg.Name
	}
	descriptor, _, err := h.entityView(ctx, raw)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(encodeEntityView(descriptor)), nil
}
func (h *homeResourceHandler) DeleteEntityView(ctx context.Context, request *connect.Request[api.DeleteEntityViewRequest]) (*connect.Response[emptypb.Empty], error) {
	raw := ""
	if request != nil && request.Msg != nil {
		raw = request.Msg.Name
	}
	descriptor, _, err := h.entityView(ctx, raw)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if err = h.views.DeleteEntityView(ctx, actor, descriptor.Name); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}
func entityViewEntry(home apitypes.HomeName, item view.Item) (*api.EntityViewEntry, error) {
	entity, err := encodeEntity(home, item.Registry().Metadata())
	if err != nil {
		return nil, err
	}
	values, ok := item.ProjectedCapabilities()
	if !ok {
		return nil, view.ErrInvalidInput
	}
	entry := &api.EntityViewEntry{Entity: entity}
	for _, value := range values {
		entry.Capabilities = append(entry.Capabilities, value.Resource())
	}
	return entry, nil
}
func (h *homeResourceHandler) QueryEntityView(ctx context.Context, request *connect.Request[api.QueryEntityViewRequest]) (*connect.Response[api.QueryEntityViewResponse], error) {
	raw := ""
	if request != nil && request.Msg != nil {
		raw = request.Msg.Name
	}
	descriptor, current, err := h.entityView(ctx, raw)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	r := request.Msg
	size, err := homePageSize(r, r.PageSize)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	binding, err := homePageBinding(current, raw, descriptor.Filter.Source()+"\x00"+descriptor.SnapshotToken, "QueryEntityView")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	pageToken := ""
	if r.PageToken != "" {
		token, err := h.openEntityViewToken(r.PageToken, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if token.Page == "" || len(token.Resume) != 0 || len(token.Names) != 0 || len(token.Snapshots) != 0 || token.Offset != 0 {
			return nil, apiRPCError(ctx, view.ErrPageToken)
		}
		pageToken = token.Page
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	page, err := h.views.ReadEntityView(ctx, actor, descriptor.Name, pageToken, size)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	out := &api.QueryEntityViewResponse{}
	for _, item := range page.Items {
		entry, err := entityViewEntry(descriptor.Name.Home(), item)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		out.Entries = append(out.Entries, entry)
	}
	if page.NextPageToken != "" {
		out.NextPageToken, err = h.sealEntityViewToken(entityViewToken{Page: page.NextPageToken, Expires: descriptor.ExpireTime.UnixNano()}, binding)
	} else {
		resume, resumeErr := view.EntityViewResume(page.Cursor)
		if resumeErr != nil {
			return nil, apiRPCError(ctx, resumeErr)
		}
		binding, err = homePageBinding(current, raw, descriptor.Filter.Source()+"\x00"+descriptor.SnapshotToken, "WatchEntityView")
		if err == nil {
			out.ResumeToken, err = h.sealEntityViewToken(entityViewToken{Resume: resume, Expires: descriptor.ExpireTime.UnixNano()}, binding)
		}
	}
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if proto.Size(out) > view.MaxSnapshotBytes {
		return nil, apiRPCError(ctx, view.ErrResourceExhausted)
	}
	final, _, err := h.entityView(ctx, raw)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if final.SnapshotToken != descriptor.SnapshotToken {
		return nil, apiRPCError(ctx, view.ErrViewChanged)
	}
	return connect.NewResponse(out), nil
}

func (h *homeResourceHandler) ListEntityViews(ctx context.Context, request *connect.Request[api.ListEntityViewsRequest]) (*connect.Response[api.ListEntityViewsResponse], error) {
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.ListEntityViewsRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
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
	binding, err := homePageBinding(current, parent.String(), "", "ListEntityViews")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	token := entityViewToken{Expires: time.Now().Add(view.SnapshotTTL).UnixNano()}
	if r.PageToken != "" {
		token, err = h.openEntityViewToken(r.PageToken, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if token.Page != "" || len(token.Resume) != 0 || token.Offset < 1 || token.Offset >= len(token.Names) || len(token.Names) != len(token.Snapshots) {
			return nil, apiRPCError(ctx, view.ErrPageToken)
		}
	} else {
		values, err := h.views.ListEntityViews(ctx, actor, home)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		for _, value := range values {
			token.Names = append(token.Names, value.Name.String())
			token.Snapshots = append(token.Snapshots, value.SnapshotToken)
		}
	}
	out := &api.ListEntityViewsResponse{}
	for token.Offset < len(token.Names) && len(out.EntityViews) < size {
		name, err := apitypes.ParseEntityViewName(token.Names[token.Offset])
		token.Offset++
		if err != nil || name.Home() != home {
			return nil, apiRPCError(ctx, view.ErrPageToken)
		}
		value, err := h.views.GetEntityView(ctx, actor, name)
		if errors.Is(err, view.ErrSnapshotUnknown) || errors.Is(err, view.ErrSnapshotExpired) || errors.Is(err, view.ErrViewChanged) {
			continue
		}
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if value.SnapshotToken != token.Snapshots[token.Offset-1] {
			continue
		}
		out.EntityViews = append(out.EntityViews, encodeEntityView(value))
	}
	if token.Offset < len(token.Names) {
		out.NextPageToken, err = h.sealEntityViewToken(token, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	_, after, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	afterBinding, err := homePageBinding(after, parent.String(), "", "ListEntityViews")
	if err != nil || !bytes.Equal(binding, afterBinding) {
		return nil, apiRPCError(ctx, view.ErrViewChanged)
	}
	return connect.NewResponse(out), nil
}

func (h *homeResourceHandler) WatchEntityView(ctx context.Context, request *connect.Request[api.WatchEntityViewRequest], stream *connect.ServerStream[api.WatchEntityViewResponse]) error {
	raw := ""
	if request != nil && request.Msg != nil {
		raw = request.Msg.Name
	}
	descriptor, current, err := h.entityView(ctx, raw)
	if err != nil {
		return apiRPCError(ctx, err)
	}
	binding, err := homePageBinding(current, raw, descriptor.Filter.Source()+"\x00"+descriptor.SnapshotToken, "WatchEntityView")
	if err != nil {
		return apiRPCError(ctx, err)
	}
	token, err := h.openEntityViewToken(request.Msg.ResumeToken, binding)
	if err != nil {
		return apiRPCError(ctx, err)
	}
	if len(token.Resume) == 0 || token.Page != "" || len(token.Names) != 0 || len(token.Snapshots) != 0 || token.Offset != 0 {
		return apiRPCError(ctx, view.ErrPageToken)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return apiRPCError(ctx, err)
	}
	watch, err := h.views.WatchEntityViewResume(ctx, actor, descriptor.Name, token.Resume)
	if err != nil {
		return apiRPCError(ctx, err)
	}
	defer watch.Close()
	defer interruptCoreRPCWriteOnCancel(ctx, watch.Context())()
	for {
		change, err := watch.Next()
		if err != nil {
			return apiRPCError(ctx, err)
		}
		if change.Resync != 0 {
			return apiRPCError(ctx, view.ErrViewChanged)
		}
		resume, err := view.EntityViewResume(change.Cursor)
		if err != nil {
			return apiRPCError(ctx, err)
		}
		checkpoint, err := h.sealEntityViewToken(entityViewToken{Resume: resume, Expires: descriptor.ExpireTime.UnixNano()}, binding)
		if err != nil {
			return apiRPCError(ctx, err)
		}
		out := &api.WatchEntityViewResponse{ResumeToken: checkpoint}
		if change.Item != nil {
			entry, err := entityViewEntry(descriptor.Name.Home(), *change.Item)
			if err != nil {
				return apiRPCError(ctx, err)
			}
			out.Change = &api.WatchEntityViewResponse_Upsert{Upsert: entry}
		} else {
			out.Change = &api.WatchEntityViewResponse_Remove{Remove: change.Entity.String()}
		}
		if proto.Size(out) > view.MaxFrameBytes {
			return apiRPCError(ctx, view.ErrResourceExhausted)
		}
		final, after, err := h.entityView(ctx, raw)
		if err != nil {
			return apiRPCError(ctx, err)
		}
		if final.SnapshotToken != descriptor.SnapshotToken {
			return apiRPCError(ctx, view.ErrViewChanged)
		}
		afterBinding, err := homePageBinding(after, raw, final.Filter.Source()+"\x00"+final.SnapshotToken, "WatchEntityView")
		if err != nil || !bytes.Equal(binding, afterBinding) {
			return apiRPCError(ctx, view.ErrViewChanged)
		}
		if err = stream.Send(out); err != nil {
			return apiRPCError(ctx, err)
		}
	}
}
