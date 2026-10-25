package server

// A read-only DashboardService fixture, compiled only into the explicitly
// selected SQL/TLS qualification server. homed has no fallback dashboard route.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	configurationv1 "github.com/damione1/Homeward/gen/home/configuration/v1"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/gen/home/dashboard/v1/dashboardv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf005Dashboard struct {
	dashboardv1connect.UnimplementedDashboardServiceHandler
	file                 string
	catalogue            *nhf007Catalogue
	authority            *authorization.Authority
	activation           *configactivation.Owner
	mu                   sync.Mutex
	owner                contracts.AccountID
	bindings             map[string]domain.EntityID
	lastHash             [32]byte
	revision             int64
	epoch                [16]byte
	lists, gets, watches atomic.Uint64
}

const nhf005DashboardID = "synthetic-dashboard"

func (h *nhf005Dashboard) install(owner string, bindings map[string]domain.EntityID) error {
	id, err := contracts.ParseAccountID(owner)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owner, h.bindings = id, bindings
	return nil
}
func (h *nhf005Dashboard) permit(ctx context.Context) (*authorization.Permit, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	h.mu.Lock()
	owner := h.owner
	h.mu.Unlock()
	id, err := authorization.ParseDashboardID(nhf005DashboardID)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	object, err := authorization.DashboardObject(id, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("synthetic dashboard not ready"))
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	permit, err := h.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	return permit, nil
}
func (h *nhf005Dashboard) capture(ctx context.Context) (*dashboardv1.Dashboard, string, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	rightsRevision, rightsHash, err := current.Binding()
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}

	stamp, err := h.activation.CurrentConfig(ctx)
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	id, _ := authorization.ParseDashboardID(nhf005DashboardID)
	source, err := imported.NewSource(id, "synthetic", filepath.Base(h.file))
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	result, err := imported.Import(filepath.Dir(h.file), source)
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	definition, present := result.Definition()
	raw, hasSource := result.EntryBytes()
	if !present || !hasSource || result.Status() != imported.StatusSupported || len(result.Sources()) != 1 || len(raw) > 1<<20 {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("unsupported synthetic Lovelace source"))
	}
	hash := sha256.Sum256(raw)
	h.mu.Lock()
	defer h.mu.Unlock()
	overview, err := projectDashboardOverview(definition, stamp, rightsRevision, dashboardv1.ServingMode_SERVING_MODE_SYNTHETIC, func(alias domain.HAAlias) (domain.EntityID, bool) {
		value, err := alias.Value()
		if err != nil {
			return domain.EntityID{}, false
		}
		entity, present := h.bindings[value]
		return entity, present
	})
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	after, err := h.activation.CurrentConfig(ctx)
	if err != nil || after != stamp {
		return nil, "", connect.NewError(connect.CodeUnavailable, errors.New("configuration changed during read"))
	}
	afterRights, err := h.authority.Current(principal)
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	afterRevision, afterHash, err := afterRights.Binding()
	if err != nil || afterRevision != rightsRevision || afterHash != rightsHash {
		return nil, "", connect.NewError(connect.CodeUnavailable, errors.New("authorization changed during read"))
	}

	if h.lastHash != hash {
		h.revision++
		h.lastHash = hash
	}
	owner, err := h.owner.Value()
	if err != nil {
		return nil, "", coreRPCError(ctx, err)
	}
	metadata := &dashboardv1.Metadata{Id: &dashboardv1.DashboardId{Value: nhf005DashboardID}, Owner: &authv1.AccountId{Value: owner}, Profile: imported.Profile,
		Version: &configurationv1.DocumentVersion{Revision: h.revision, SourceSha256: hash[:]}, UpdatedAt: timestamppb.New(time.Unix(h.revision, 0))}
	doc := &dashboardv1.Dashboard{Metadata: metadata, NativeOverview: overview,
		Definition: &dashboardv1.Definition{Id: metadata.Id, Owner: metadata.Owner, UrlPath: "synthetic", Entry: filepath.Base(h.file), Files: []*dashboardv1.SourceFile{{Path: filepath.Base(h.file), ExactSource: raw}}}}
	// Include both document and active-configuration references. No client is
	// allowed to infer or increment this opaque replay token.
	token := hex.EncodeToString(h.epoch[:]) + ":" + strconv.FormatInt(h.revision, 10) + ":" + hex.EncodeToString(hash[:]) + ":" + strconv.FormatUint(overview.ConfigRevision, 10) + ":" + hex.EncodeToString(overview.ConfigSha256) + ":" + strconv.FormatUint(rightsRevision, 10)
	return doc, token, nil
}
func (h *nhf005Dashboard) ListDashboards(ctx context.Context, _ *connect.Request[dashboardv1.ListDashboardsRequest]) (*connect.Response[dashboardv1.ListDashboardsResponse], error) {
	h.lists.Add(1)
	if h.catalogue != nil {
		return h.listCatalogue(ctx)
	}
	permit, err := h.permit(ctx)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	doc, token, err := h.capture(permit.Context())
	if err != nil {
		return nil, err
	}
	if !permit.Ready() {
		return nil, coreRPCError(ctx, authorization.ErrDenied)
	}
	return connect.NewResponse(&dashboardv1.ListDashboardsResponse{Dashboards: []*dashboardv1.Metadata{doc.Metadata}, ResumeToken: token}), nil
}
func (h *nhf005Dashboard) GetDashboard(ctx context.Context, request *connect.Request[dashboardv1.GetDashboardRequest]) (*connect.Response[dashboardv1.GetDashboardResponse], error) {
	h.gets.Add(1)
	if h.catalogue != nil {
		return h.getCatalogue(ctx, request.Msg)
	}
	permit, err := h.permit(ctx)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if request.Msg.GetId().GetValue() != nhf005DashboardID {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
	}
	doc, token, err := h.capture(permit.Context())
	if err != nil {
		return nil, err
	}
	if !permit.Ready() {
		return nil, coreRPCError(ctx, authorization.ErrDenied)
	}
	return connect.NewResponse(&dashboardv1.GetDashboardResponse{Dashboard: doc, ResumeToken: token}), nil
}
func (h *nhf005Dashboard) WatchDashboards(ctx context.Context, request *connect.Request[dashboardv1.WatchDashboardsRequest], stream *connect.ServerStream[dashboardv1.WatchDashboardsResponse]) error {
	h.watches.Add(1)
	if h.catalogue != nil {
		return h.watchCatalogue(ctx, request.Msg, stream)
	}
	permit, err := h.permit(ctx)
	if err != nil {
		return err
	}
	defer permit.Release()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		doc, token, err := h.capture(permit.Context())
		if err != nil {
			return err
		}
		if !permit.Ready() {
			return coreRPCError(ctx, authorization.ErrDenied)
		}
		if request.Msg.ResumeToken != token {
			return stream.Send(&dashboardv1.WatchDashboardsResponse{Result: &dashboardv1.WatchDashboardsResponse_Snapshot{Snapshot: &dashboardv1.ListDashboardsResponse{Dashboards: []*dashboardv1.Metadata{proto.CloneOf(doc.Metadata)}, ResumeToken: token}}})
		}
		select {
		case <-permit.Context().Done():
			return coreRPCError(ctx, authorization.ErrDenied)
		case <-ticker.C:
		}
	}
}
func (h *nhf005Dashboard) register(plan *PublicPlan) error {
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return err
	}
	h.mu.Lock()
	h.epoch = epoch
	h.mu.Unlock()
	routes := []ConnectProcedure{
		{Path: dashboardv1connect.DashboardServiceListDashboardsProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: dashboardv1connect.DashboardServiceGetDashboardProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		{Path: dashboardv1connect.DashboardServiceWatchDashboardsProcedure, Access: RouteAccessProtected, Flow: RouteFlowServerStream, Transport: RouteTransportTLSRequired},
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return dashboardv1connect.NewDashboardServiceHandler(h, options...)
	}, routes...); err != nil {
		return err
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.Path, permission); err != nil {
			return err
		}
	}
	return nil
}
func nhf005DashboardFixture() (*nhf005Dashboard, error) {
	file := os.Getenv("HOMEWARD_NHF005_DASHBOARD")
	catalogue := os.Getenv("HOMEWARD_NHF007_CATALOGUE")
	if catalogue != "" {
		if file != "" || !filepath.IsAbs(catalogue) {
			return nil, errors.New("synthetic dashboard catalogue must be absolute and exclusive")
		}
		return &nhf005Dashboard{catalogue: &nhf007Catalogue{file: catalogue, versions: make(map[string]nhf007Version)}}, nil
	}
	if file == "" {
		return nil, nil
	}
	if !filepath.IsAbs(file) {
		return nil, errors.New("synthetic dashboard source must be absolute")
	}
	return &nhf005Dashboard{file: file}, nil
}
