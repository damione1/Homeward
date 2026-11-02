package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/gen/home/dashboard/v1/dashboardv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Dashboard reads use only published, validated configuration. Raw source
// bytes belong to the separately authorized ConfigurationService.
type dashboardHandler struct {
	dashboardv1connect.UnimplementedDashboardServiceHandler
	owner     *configactivation.Owner
	authority *authorization.Authority
	sequence  *sequencer.Sequencer
	epoch     [16]byte
}

type dashboardCapture struct {
	documents []*wire.Dashboard
	token     string
	permits   []*authorization.Permit
}

func (c *dashboardCapture) release() {
	for _, permit := range c.permits {
		permit.Release()
	}
	c.permits = nil
}
func (c *dashboardCapture) ready() bool {
	for _, permit := range c.permits {
		if !permit.Ready() {
			return false
		}
	}
	return true
}

func (h *dashboardHandler) capture(ctx context.Context, support *wire.ClientLayoutSupport) (result dashboardCapture, err error) {
	defer func() {
		if err != nil {
			result.release()
		}
	}()
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	grant, err := h.authority.Current(principal)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	rights, rightsHash, err := grant.Binding()
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	definitions, stamp, err := h.owner.CurrentDashboards(ctx)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	revision, configHash, err := stamp.Components()
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	versions, err := h.owner.DashboardSourceVersions(ctx, stamp)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	if len(definitions) > 128 {
		return result, coreRPCError(ctx, authorization.ErrCapacity)
	}
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, auth.ScopeOperation_SCOPE_OPERATION_READ)
	authorized := make([]imported.Definition, 0, len(definitions))
	for _, definition := range definitions {
		// Only source-authored ownership is authoritative. Legacy unowned
		// documents cannot become owned by the account making this request.
		owner, present := definition.Owner()
		if !present {
			continue
		}
		object, objectErr := authorization.DashboardObject(definition.Source().ID(), owner)
		if objectErr != nil {
			return result, coreRPCError(ctx, objectErr)
		}
		permit, permitErr := h.authority.Permit(ctx, principal, permission, object)
		if errors.Is(permitErr, authorization.ErrDenied) {
			continue
		}
		if permitErr != nil {
			return result, coreRPCError(ctx, permitErr)
		}
		result.permits = append(result.permits, permit)
		authorized = append(authorized, definition)
	}
	index, err := imported.NewNavigationIndex(authorized)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	projection, err := h.sequence.CaptureBounded(8 << 20)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	bindings := make(map[string]domain.EntityID)
	statePermission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	for entity, record := range projection.Registry {
		if record.Retired() {
			continue
		}
		allowed := false
		for _, capability := range projection.Capabilities[entity] {
			slot, slotErr := authorization.NewCapabilitySlot(capability.SchemaId, capability.Endpoint)
			if slotErr != nil {
				return result, coreRPCError(ctx, slotErr)
			}
			object, objectErr := authorization.EntityObject(entity, slot)
			if objectErr != nil {
				return result, coreRPCError(ctx, objectErr)
			}
			if grant.Authorize(statePermission, object) == nil {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		_, _, aliases, aliasErr := record.Aliases()
		if aliasErr != nil {
			return result, coreRPCError(ctx, aliasErr)
		}
		for _, alias := range aliases {
			text, aliasErr := alias.Value()
			if aliasErr != nil {
				return result, coreRPCError(ctx, aliasErr)
			}
			if previous, exists := bindings[text]; exists && previous != entity {
				return result, coreRPCError(ctx, authorization.ErrUnavailable)
			}
			bindings[text] = entity
		}
	}
	resolve := func(alias domain.HAAlias) (domain.EntityID, bool) {
		text, err := alias.Value()
		entity, ok := bindings[text]
		return entity, err == nil && ok
	}
	token := sha256.New()
	token.Write(h.epoch[:])
	token.Write(rightsHash[:])
	token.Write(configHash[:])
	fmt.Fprintf(token, ":%d:%d:", revision, rights)
	aliases := make([]string, 0, len(bindings))
	for alias := range bindings {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		entity, _ := bindings[alias].Value()
		fmt.Fprintf(token, "%d:%s%d:%s", len(alias), alias, len(entity), entity)
	}
	for _, definition := range authorized {
		version, present := versions[definition.Source().ID()]
		if !present {
			return result, coreRPCError(ctx, authorization.ErrUnavailable)
		}
		id, _ := definition.Source().ID().Value()
		owner, _ := definition.Owner()
		ownerText, _ := owner.Value()
		digest := definition.SHA256()
		title := "Dashboard"
		if views := definition.Views(); len(views) != 0 && views[0].Title() != "" {
			title = views[0].Title()
		}
		metadata := &wire.Metadata{Id: &wire.DashboardId{Value: id}, Owner: &auth.AccountId{Value: ownerText}, Profile: definition.Profile(), Title: title,
			Version: &configuration.DocumentVersion{Revision: version.Revision, SourceSha256: version.SourceSHA256[:]}, UpdatedAt: timestamppb.New(version.UpdatedAt)}
		document := &wire.Dashboard{Metadata: metadata, NativeOverview: &wire.NativeOverview{Version: 1, ConfigRevision: revision, ConfigSha256: configHash[:], Title: title, Mode: wire.ServingMode_SERVING_MODE_CONFIGURED, AuthorizationRevision: rights}}
		// List and Get have the same replay identity regardless of negotiation.
		// The tree may contain inert unsupported nodes for an older client.
		document.NativeLayout, err = imported.ProjectNativeDefinition(definition, imported.LayoutProjection{Support: support, Entity: resolve, Navigation: &index})
		if err != nil {
			return result, coreRPCError(ctx, err)
		}
		encoded, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(metadata)
		if marshalErr != nil {
			return result, coreRPCError(ctx, marshalErr)
		}
		fmt.Fprintf(token, "%d:", len(encoded))
		token.Write(encoded)
		token.Write(digest[:])
		result.documents = append(result.documents, document)
	}
	after, err := h.owner.CurrentConfig(ctx)
	if err != nil || after != stamp {
		return result, coreRPCError(ctx, authorization.ErrUnavailable)
	}
	afterGrant, err := h.authority.Current(principal)
	if err != nil {
		return result, coreRPCError(ctx, err)
	}
	afterRights, afterHash, err := afterGrant.Binding()
	if err != nil || rights != afterRights || rightsHash != afterHash || !result.ready() {
		return result, coreRPCError(ctx, authorization.ErrDenied)
	}
	result.token = hex.EncodeToString(token.Sum(nil))
	return result, nil
}

func (h *dashboardHandler) ListDashboards(ctx context.Context, request *connect.Request[wire.ListDashboardsRequest]) (*connect.Response[wire.ListDashboardsResponse], error) {
	capture, err := h.capture(ctx, request.Msg.Support)
	if err != nil {
		return nil, err
	}
	defer capture.release()
	response := &wire.ListDashboardsResponse{ResumeToken: capture.token}
	for _, document := range capture.documents {
		response.Dashboards = append(response.Dashboards, document.Metadata)
	}
	if !capture.ready() {
		return nil, coreRPCError(ctx, authorization.ErrDenied)
	}
	return connect.NewResponse(response), nil
}
func (h *dashboardHandler) GetDashboard(ctx context.Context, request *connect.Request[wire.GetDashboardRequest]) (*connect.Response[wire.GetDashboardResponse], error) {
	if _, err := authorization.ParseDashboardID(request.Msg.GetId().GetValue()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid dashboard identity"))
	}
	capture, err := h.capture(ctx, request.Msg.Support)
	if err != nil {
		return nil, err
	}
	defer capture.release()
	if !capture.ready() {
		return nil, coreRPCError(ctx, authorization.ErrDenied)
	}
	for _, document := range capture.documents {
		if document.Metadata.Id.Value == request.Msg.Id.Value {
			return connect.NewResponse(&wire.GetDashboardResponse{Dashboard: document, ResumeToken: capture.token}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
}
func (h *dashboardHandler) WatchDashboards(ctx context.Context, request *connect.Request[wire.WatchDashboardsRequest], stream *connect.ServerStream[wire.WatchDashboardsResponse]) error {
	if len(request.Msg.ResumeToken) > 4096 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid dashboard cursor"))
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return coreRPCError(ctx, err)
	}
	configChanged, cancelConfig, err := h.owner.SubscribeConfigChanges(ctx)
	if err != nil {
		if errors.Is(err, configactivation.ErrActivationUnavailable) {
			return stream.Send(&wire.WatchDashboardsResponse{Result: &wire.WatchDashboardsResponse_ResyncRequired{ResyncRequired: &configuration.ResyncRequired{Reason: configuration.ResyncReason_RESYNC_REASON_ACTIVATION_SIGNAL}}})
		}
		return coreRPCError(ctx, err)
	}
	defer cancelConfig()
	rightsChanged, err := h.authority.Subscribe(ctx, principal)
	if err != nil {
		if errors.Is(err, authorization.ErrUnavailable) {
			return stream.Send(&wire.WatchDashboardsResponse{Result: &wire.WatchDashboardsResponse_ResyncRequired{ResyncRequired: &configuration.ResyncRequired{Reason: configuration.ResyncReason_RESYNC_REASON_AUTHORITY_CHANGED}}})
		}
		return coreRPCError(ctx, err)
	}
	defer rightsChanged.Close()
	initial, err := h.sequence.CaptureBounded(8 << 20)
	if err != nil {
		return stream.Send(&wire.WatchDashboardsResponse{Result: &wire.WatchDashboardsResponse_ResyncRequired{ResyncRequired: &configuration.ResyncRequired{Reason: configuration.ResyncReason_RESYNC_REASON_ACTIVATION_SIGNAL}}})
	}
	registryChanged, _, _, err := h.sequence.SubscribeViewAfter(initial.Cursor, 1024)
	if err != nil {
		return stream.Send(&wire.WatchDashboardsResponse{Result: &wire.WatchDashboardsResponse_ResyncRequired{ResyncRequired: &configuration.ResyncRequired{Reason: configuration.ResyncReason_RESYNC_REASON_HISTORY_LOST}}})
	}
	defer registryChanged.Close()
	for {
		capture, captureErr := h.capture(ctx, nil)
		if captureErr != nil {
			if connect.CodeOf(captureErr) == connect.CodeUnavailable {
				return stream.Send(&wire.WatchDashboardsResponse{Result: &wire.WatchDashboardsResponse_ResyncRequired{ResyncRequired: &configuration.ResyncRequired{Reason: configuration.ResyncReason_RESYNC_REASON_ACTIVATION_SIGNAL}}})
			}
			return captureErr
		}
		if capture.token != request.Msg.ResumeToken {
			response := &wire.ListDashboardsResponse{ResumeToken: capture.token}
			for _, document := range capture.documents {
				response.Dashboards = append(response.Dashboards, document.Metadata)
			}
			if !capture.ready() {
				capture.release()
				return coreRPCError(ctx, authorization.ErrDenied)
			}
			err = stream.Send(&wire.WatchDashboardsResponse{Result: &wire.WatchDashboardsResponse_Snapshot{Snapshot: response}})
			capture.release()
			return err
		}
		capture.release()
		// Drain the bounded mailbox before sleeping. State-only notifications
		// do not change the layout token; registry binding changes do.
		var wake <-chan struct{}
		drained := false
		for wake == nil {
			_, wake, err = registryChanged.Poll()
			if err != nil {
				return coreRPCError(ctx, authorization.ErrUnavailable)
			}
			if wake == nil {
				drained = true
			}
		}
		if drained {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-configChanged:
		case <-rightsChanged.Changes():
		case <-wake:
		}
	}
}

func registerDashboardRoutes(plan *PublicPlan, owner *configactivation.Owner, authority *authorization.Authority, sequence *sequencer.Sequencer) error {
	if plan == nil || owner == nil || authority == nil || sequence == nil {
		return ErrInvalidPublicPlan
	}
	// Explicit custom RuntimePlan routes remain available to isolated transport
	// tests. Default Run always installs this production implementation.
	plan.mu.Lock()
	_, custom := plan.routes[routeLookup{method: http.MethodPost, path: dashboardv1connect.DashboardServiceListDashboardsProcedure}]
	plan.mu.Unlock()
	if custom {
		return nil
	}
	h := &dashboardHandler{owner: owner, authority: authority, sequence: sequence}
	if _, err := rand.Read(h.epoch[:]); err != nil {
		return err
	}
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
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, auth.ScopeOperation_SCOPE_OPERATION_READ)
	for _, route := range routes {
		if err := plan.BindPermission(RouteKindConnect, RouteMethodPost, route.Path, permission); err != nil {
			return err
		}
	}
	return nil
}
