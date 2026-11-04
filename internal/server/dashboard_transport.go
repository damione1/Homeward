package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxDashboardResourceBytes = 1 << 20
const maxDashboardPageBytes = 2 << 20
const maxDashboardCatalogueBytes = 16 << 20

// Borrow existing native owners. This adapter retains no catalogue or effect
// ledger; its only private state is an ephemeral representation secret.
type dashboardHandler struct {
	apiv2connect.UnimplementedDashboardServiceHandler
	owner     *configactivation.Owner
	authority *authorization.Authority
	sequence  *sequencer.Sequencer
	home      *homeResourceHandler
	profiles  view.DisplayProfileSource
	secret    [32]byte
}
type dashboardCheckpoint struct {
	Digest  string
	Offset  int
	Expires int64
}

func (h *dashboardHandler) sealCheckpoint(value dashboardCheckpoint, binding []byte) (string, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, h.home.pages.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(h.home.pages.Seal(nonce, nonce, plain, binding)), nil
}
func (h *dashboardHandler) openCheckpoint(raw string, binding []byte) (dashboardCheckpoint, error) {
	var value dashboardCheckpoint
	if len(raw) == 0 || len(raw) > 8192 {
		return value, view.ErrPageToken
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) < h.home.pages.NonceSize()+h.home.pages.Overhead() {
		return value, view.ErrPageToken
	}
	plain, err := h.home.pages.Open(nil, data[:h.home.pages.NonceSize()], data[h.home.pages.NonceSize():], binding)
	if err != nil || json.Unmarshal(plain, &value) != nil || value.Expires <= time.Now().UnixNano() || len(value.Digest) != 43 || value.Offset < 0 || value.Offset > 128 {
		return dashboardCheckpoint{}, view.ErrViewChanged
	}
	return value, nil
}
func (h *dashboardHandler) opaque(parts ...[]byte) string {
	digest := hmac.New(sha256.New, h.secret[:])
	for _, part := range parts {
		_ = json.NewEncoder(digest).Encode(base64.RawURLEncoding.EncodeToString(part))
	}
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}
func dashboardPermission() authorization.Permission {
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, auth.ScopeOperation_SCOPE_OPERATION_READ)
	return permission
}
func (h *dashboardHandler) parent(ctx context.Context, message proto.Message, raw string) (apitypes.HomeName, error) {
	permission := dashboardPermission()
	if _, _, err := h.home.current(ctx, &permission); err != nil {
		return apitypes.HomeName{}, err
	}
	name, err := apitypes.ParseHomeName(raw)
	if err != nil {
		return name, homeValidation(message, "parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	home, err := h.home.owner.HomeName(ctx)
	if err != nil {
		return name, err
	}
	if name != home {
		return name, authorization.ErrDenied
	}
	return name, nil
}
func (h *dashboardHandler) name(ctx context.Context, message proto.Message, raw string) (apitypes.DashboardName, error) {
	permission := dashboardPermission()
	if _, _, err := h.home.current(ctx, &permission); err != nil {
		return apitypes.DashboardName{}, err
	}
	name, err := apitypes.ParseDashboardName(raw)
	if err != nil {
		return name, homeValidation(message, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
	}
	home, err := h.home.owner.HomeName(ctx)
	if err != nil {
		return name, err
	}
	if name.Home() != home {
		return name, authorization.ErrDenied
	}
	return name, nil
}
func (h *dashboardHandler) GetDashboard(ctx context.Context, request *connect.Request[api.GetDashboardRequest]) (*connect.Response[api.Dashboard], error) {
	r := new(api.GetDashboardRequest)
	if request != nil && request.Msg != nil {
		r = request.Msg
	}
	name, err := h.name(ctx, r, r.Name)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	capture, err := h.capture(ctx, name.Home(), &name, nil)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	defer capture.close()
	if len(capture.documents) != 1 {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
	}
	if err = capture.retain(ctx); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(capture.documents[0]), nil
}
func (h *dashboardHandler) ListDashboards(ctx context.Context, request *connect.Request[api.ListDashboardsRequest]) (*connect.Response[api.ListDashboardsResponse], error) {
	r := new(api.ListDashboardsRequest)
	if request != nil && request.Msg != nil {
		r = request.Msg
	}
	home, err := h.parent(ctx, r, r.Parent)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if r.PageSize < 0 {
		return nil, apiRPCError(ctx, homeValidation(r, "page_size", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE))
	}
	size := int(r.PageSize)
	if size == 0 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	capture, err := h.capture(ctx, home, nil, nil)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	defer capture.close()
	binding, err := homePageBinding(capture.current, home.String(), "", "ListDashboards")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	token := dashboardCheckpoint{Digest: capture.digest, Expires: time.Now().Add(view.SnapshotTTL).UnixNano()}
	if r.PageToken != "" {
		token, err = h.openCheckpoint(r.PageToken, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if token.Digest != capture.digest || token.Offset >= len(capture.documents) {
			return nil, apiRPCError(ctx, view.ErrViewChanged)
		}
	}
	out := new(api.ListDashboardsResponse)
	// Stop before an overflowing candidate and resume at that exact candidate.
	for token.Offset < len(capture.documents) && len(out.Dashboards) < size {
		out.Dashboards = append(out.Dashboards, capture.documents[token.Offset])
		if proto.Size(out) > maxDashboardPageBytes-8192 {
			out.Dashboards = out.Dashboards[:len(out.Dashboards)-1]
			break
		}
		token.Offset++
	}
	if token.Offset < len(capture.documents) {
		if len(out.Dashboards) == 0 {
			return nil, apiRPCError(ctx, view.ErrResourceExhausted)
		}
		out.NextPageToken, err = h.sealCheckpoint(token, binding)
	} else {
		token.Offset = 0
		binding, err = homePageBinding(capture.current, home.String(), "", "WatchDashboards")
		if err == nil {
			out.ResumeToken, err = h.sealCheckpoint(token, binding)
		}
	}
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if proto.Size(out) > maxDashboardPageBytes {
		return nil, apiRPCError(ctx, view.ErrResourceExhausted)
	}
	if err = capture.retain(ctx); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(out), nil
}
func (h *dashboardHandler) WatchDashboards(ctx context.Context, request *connect.Request[api.WatchDashboardsRequest], stream *connect.ServerStream[api.WatchDashboardsResponse]) error {
	r := new(api.WatchDashboardsRequest)
	if request != nil && request.Msg != nil {
		r = request.Msg
	}
	home, err := h.parent(ctx, r, r.Parent)
	if err != nil {
		return apiRPCError(ctx, err)
	}
	capture, err := h.capture(ctx, home, nil, nil)
	if err != nil {
		return apiRPCError(ctx, err)
	}
	binding, err := homePageBinding(capture.current, home.String(), "", "WatchDashboards")
	if err != nil {
		capture.close()
		return apiRPCError(ctx, err)
	}
	token, err := h.openCheckpoint(r.ResumeToken, binding)
	if err != nil || token.Offset != 0 || token.Digest != capture.digest {
		capture.close()
		if err == nil {
			err = view.ErrViewChanged
		}
		return apiRPCError(ctx, err)
	}
	for {
		select {
		case <-ctx.Done():
			capture.close()
			return apiRPCError(ctx, ctx.Err())
		case <-capture.ctx.Done():
		}
		capture.close()
		capture, err = h.capture(ctx, home, nil, nil)
		if err != nil {
			return apiRPCError(ctx, err)
		}
		after, bindErr := homePageBinding(capture.current, home.String(), "", "WatchDashboards")
		if bindErr != nil || !hmac.Equal(binding, after) {
			capture.close()
			return apiRPCError(ctx, authorization.ErrDenied)
		}
		if capture.digest == token.Digest {
			continue
		}
		defer capture.close()
		defer interruptCoreRPCWriteOnCancel(ctx, capture.ctx)()
		if !capture.ready() {
			return apiRPCError(ctx, view.ErrViewChanged)
		}
		return stream.Send(&api.WatchDashboardsResponse{Invalidated: true})
	}
}
func (h *dashboardHandler) QueryDashboard(ctx context.Context, request *connect.Request[api.QueryDashboardRequest]) (*connect.Response[api.QueryDashboardResponse], error) {
	r := new(api.QueryDashboardRequest)
	if request != nil && request.Msg != nil {
		r = request.Msg
	}
	name, err := h.name(ctx, r, r.Name)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	joined, err := h.captureBindings(ctx, r)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	defer joined.close()
	capture, err := h.capture(ctx, name.Home(), &name, joined)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	defer capture.close()
	if len(capture.documents) != 1 {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
	}
	if err = joined.retain(ctx); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if err = capture.retain(ctx); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	out := &api.QueryDashboardResponse{Dashboard: capture.documents[0], EntityView: r.EntityView, ResumeToken: r.ResumeToken, ExpireTime: timestamppb.New(joined.descriptor.ExpireTime)}
	_, rights, _ := capture.current.Binding()
	out.Binding = h.opaque([]byte("DashboardCapture/v2"), rights[:], []byte(out.Dashboard.Name), []byte(out.Dashboard.Etag), []byte(joined.descriptor.SnapshotToken), []byte(r.ResumeToken))
	return connect.NewResponse(out), nil
}
func registerDashboardRoutes(plan *PublicPlan, owner *configactivation.Owner, authority *authorization.Authority, sequence *sequencer.Sequencer, home *homeResourceHandler, profiles view.DisplayProfileSource) error {
	if plan == nil || owner == nil || authority == nil || sequence == nil || home == nil {
		return ErrInvalidPublicPlan
	}
	h := &dashboardHandler{owner: owner, authority: authority, sequence: sequence, home: home, profiles: profiles}
	if _, err := rand.Read(h.secret[:]); err != nil {
		return err
	}
	routes := []ConnectProcedure{
		{Path: apiv2connect.DashboardServiceListDashboardsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: apiv2connect.DashboardServiceGetDashboardProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: apiv2connect.DashboardServiceQueryDashboardProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: apiv2connect.DashboardServiceWatchDashboardsProcedure, Access: RouteAccessProtected, Flow: RouteFlowServerStream, Transport: RouteTransportTLSRequired},
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return apiv2connect.NewDashboardServiceHandler(h, options...)
	}, routes...); err != nil {
		return err
	}
	for _, route := range routes {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.Path, dashboardPermission()); err != nil {
			return err
		}
	}
	return nil
}
