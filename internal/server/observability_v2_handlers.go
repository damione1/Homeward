package server

import (
	"connectrpc.com/connect"
	"context"
	"errors"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	native "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"strconv"
	"strings"
)

type observationV2Handler struct {
	reader *observationReader
	names  administrationNames
}

func observationFault(err error) apierrors.Fault {
	var fault apierrors.Fault
	if errors.As(err, &fault) {
		return fault
	}
	if errors.Is(err, context.Canceled) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED)
	}
	if errors.Is(err, authorization.ErrCapacity) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RATE_LIMITED)
	}
	var rejected *observability.Error
	if errors.As(err, &rejected) {
		reason := api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		switch rejected.Code() {
		case native.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT:
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		case native.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_DENIED:
			reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
		case native.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAUTHENTICATED:
			reason = api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED
		case native.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CAPACITY:
			reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
		case native.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_STALE_CURSOR:
			reason = api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED
		case native.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_DEADLINE_EXCEEDED:
			reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_DEADLINE_EXCEEDED
		}
		return apierrors.New(reason)
	}
	if errors.Is(err, contracts.ErrInvalidObservabilityContract) || errors.Is(err, apitypes.ErrResourceName) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	return administrationFault(err)
}
func observationFailure(reason api.ApiErrorReason) error { return apierrors.New(reason) }
func observationPage(size int32, token string) (*uint32, *string, error) {
	if size < 0 || len(token) > 512 {
		return nil, nil, contracts.ErrInvalidObservabilityContract
	}
	if size == 0 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	n := uint32(size)
	if token == "" {
		return &n, nil, nil
	}
	return &n, &token, nil
}
func observationNext(value *native.PageContinuation) (string, error) {
	if value == nil {
		return "", storage.ErrAPIResourceIntegrity
	}
	switch v := value.Result.(type) {
	case *native.PageContinuation_End:
		return "", nil
	case *native.PageContinuation_NextPageToken:
		return v.NextPageToken, nil
	default:
		return "", storage.ErrAPIResourceIntegrity
	}
}
func observationRead[T any](ctx context.Context, h *observationV2Handler, family auth.ScopeFamily, request proto.Message, read func(*observationProjection) (*T, error)) (response *connect.Response[T], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, observationFault(callErr))
		}
	}()
	if h == nil || h.reader == nil || h.reader.runtime == nil || h.reader.runtime.authority == nil || nilReactiveInterface(h.names.home) {
		return nil, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, authorization.ErrDenied
	}
	actor, present := ActorFromContext(ctx)
	admitted, err := principal.Actor()
	if err != nil || !present || admitted != actor {
		return nil, authorization.ErrDenied
	}
	permission, err := authorization.NewPermission(family, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, err
	}
	object, err := authorization.AdministrativeObject(family)
	if err != nil {
		return nil, err
	}
	permit, err := h.reader.runtime.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			permit.Release()
		}
	}()
	if !permit.Ready() {
		return nil, authorization.ErrDenied
	}
	if err = contracts.AdmitObservationRequestShape(request); err != nil {
		return nil, err
	}
	home, err := h.names.home.HomeName(permit.Context())
	if err != nil {
		return nil, err
	}
	projection := &observationProjection{ctx: permit.Context(), names: h.names, home: home}
	value, err := read(projection)
	if err != nil {
		return nil, err
	}
	if projection.err != nil {
		return nil, projection.err
	}
	message, ok := any(value).(proto.Message)
	if !ok || value == nil || contracts.AdmitObservationResponse(message) != nil {
		return nil, storage.ErrAPIResourceIntegrity
	}
	encoded, err := protojson.Marshal(message)
	if err != nil || len(encoded) > 256<<10 {
		return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	if !permit.Ready() {
		return nil, authorization.ErrDenied
	}
	retained, err = retainPublicResponse(ctx, permit.Context(), permit.Ready, permit.Release)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(value), nil
}
func (p *observationProjection) parent(value string) error { return p.names.parent(p.ctx, value) }
func (p *observationProjection) singleton(value, suffix string) error {
	parent, ok := strings.CutSuffix(value, "/"+suffix)
	if !ok {
		return contracts.ErrInvalidObservabilityContract
	}
	return p.parent(parent)
}
func (h *observationV2Handler) GetHealth(ctx context.Context, r *connect.Request[api.GetHealthRequest]) (*connect.Response[api.Health], error) {
	var request *api.GetHealthRequest
	if r != nil {
		request = r.Msg
	}
	response, err := observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, request, func(p *observationProjection) (*api.Health, error) {
		if err := p.singleton(request.Name, "health"); err != nil {
			return nil, err
		}
		value, err := h.reader.GetHealth(p.ctx, &native.GetHealthRequest{})
		if err != nil {
			return nil, err
		}
		result := p.projectHealthSnapshot(value.Health)
		result.Name = request.Name
		return result, nil
	})
	if err == nil && h.reader.runtime.readOnly != nil {
		h.reader.runtime.readOnly.headers(response.Header())
		response.Header().Set("Homeward-Command-Driver-Entries", strconv.FormatUint(h.reader.runtime.host.CommandDriverEntries(), 10))
	}
	return response, err
}
func (h *observationV2Handler) GetMetrics(ctx context.Context, r *connect.Request[api.GetMetricsRequest]) (*connect.Response[api.Metrics], error) {
	var request *api.GetMetricsRequest
	if r != nil {
		request = r.Msg
	}
	return observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, request, func(p *observationProjection) (*api.Metrics, error) {
		if err := p.singleton(request.Name, "metrics"); err != nil {
			return nil, err
		}
		value, err := h.reader.GetMetrics(p.ctx, &native.GetMetricsRequest{})
		if err != nil {
			return nil, err
		}
		result := p.projectMetricsSnapshot(value.Metrics)
		result.Name = request.Name
		return result, nil
	})
}
func (h *observationV2Handler) GetIntegrationHealth(ctx context.Context, r *connect.Request[api.GetIntegrationHealthRequest]) (*connect.Response[api.IntegrationHealth], error) {
	var request *api.GetIntegrationHealthRequest
	if r != nil {
		request = r.Msg
	}
	return observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, request, func(p *observationProjection) (*api.IntegrationHealth, error) {
		text, ok := strings.CutSuffix(request.Name, "/health")
		if !ok {
			return nil, contracts.ErrInvalidObservabilityContract
		}
		name, err := apitypes.ParseIntegrationInstanceName(text)
		if err != nil {
			return nil, err
		}
		key, err := p.names.resolve(p.ctx, storage.APIIntegrationInstances, name.String())
		if err != nil {
			return nil, err
		}
		value, err := h.reader.GetIntegrationHealth(p.ctx, &native.GetIntegrationHealthRequest{IntegrationInstanceId: key})
		if err != nil {
			return nil, err
		}
		return &api.IntegrationHealth{Name: request.Name, Report: p.projectIntegrationReportPresence(value.Report), Supervision: p.projectIntegrationSupervision(value.Supervision)}, nil
	})
}
func (h *observationV2Handler) QueryRuntimeLogs(ctx context.Context, r *connect.Request[api.QueryRuntimeLogsRequest]) (*connect.Response[api.QueryRuntimeLogsResponse], error) {
	var request *api.QueryRuntimeLogsRequest
	if r != nil {
		request = r.Msg
	}
	return observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, request, func(p *observationProjection) (*api.QueryRuntimeLogsResponse, error) {
		if err := p.parent(request.Parent); err != nil {
			return nil, err
		}
		size, token, err := observationPage(request.PageSize, request.PageToken)
		if err != nil {
			return nil, err
		}
		value, err := h.reader.QueryRuntimeLogs(p.ctx, &native.QueryRuntimeLogsRequest{PageSize: size, PageToken: token})
		if err != nil {
			return nil, err
		}
		switch v := value.Result.(type) {
		case *native.QueryRuntimeLogsResponse_Page:
			next, err := observationNext(v.Page.Continuation)
			if err != nil {
				return nil, err
			}
			result := &api.QueryRuntimeLogsResponse{NextPageToken: next, ProcessEpoch: p.projectProcessObservationEpoch(v.Page.ProcessEpoch), UpperSequence: apitypes.DecimalUint64FromNative(v.Page.UpperSequence).String()}
			for _, row := range v.Page.Rows {
				result.Rows = append(result.Rows, p.projectRuntimeLogRow(row))
			}
			return result, nil
		case *native.QueryRuntimeLogsResponse_CursorEvicted, *native.QueryRuntimeLogsResponse_StaleCursor:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
		default:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
		}
	})
}
func (h *observationV2Handler) QueryAudits(ctx context.Context, r *connect.Request[api.QueryAuditsRequest]) (*connect.Response[api.QueryAuditsResponse], error) {
	var request *api.QueryAuditsRequest
	if r != nil {
		request = r.Msg
	}
	return observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, request, func(p *observationProjection) (*api.QueryAuditsResponse, error) {
		if err := p.parent(request.Parent); err != nil {
			return nil, err
		}
		size, token, err := observationPage(request.PageSize, request.PageToken)
		if err != nil {
			return nil, err
		}
		value, err := h.reader.QueryAudits(p.ctx, &native.QueryAuditsRequest{Source: native.AuditSource(request.Source), PageSize: size, PageToken: token})
		if err != nil {
			return nil, err
		}
		switch v := value.Result.(type) {
		case *native.QueryAuditsResponse_Page:
			next, err := observationNext(v.Page.Continuation)
			if err != nil {
				return nil, err
			}
			result := &api.QueryAuditsResponse{NextPageToken: next, Source: request.Source}
			for _, row := range v.Page.Rows {
				result.Rows = append(result.Rows, p.projectAuditRow(row))
			}
			return result, nil
		case *native.QueryAuditsResponse_StaleCursor:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
		default:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
		}
	})
}
func (h *observationV2Handler) QueryRunTrace(ctx context.Context, r *connect.Request[api.QueryRunTraceRequest]) (*connect.Response[api.QueryRunTraceResponse], error) {
	var request *api.QueryRunTraceRequest
	if r != nil {
		request = r.Msg
	}
	return observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, request, func(p *observationProjection) (*api.QueryRunTraceResponse, error) {
		name, err := apitypes.ParseRuntimeRunName(request.Parent)
		if err != nil {
			return nil, err
		}
		if err = p.parent(name.Controller().Home().String()); err != nil {
			return nil, err
		}
		p.controller = name.Controller()
		size, token, err := observationPage(request.PageSize, request.PageToken)
		if err != nil {
			return nil, err
		}
		identity := name.Controller().Identity()
		value, err := h.reader.ReadRunTrace(p.ctx, &native.ReadRunTraceRequest{Controller: &native.ControllerHandle{Value: identity[:]}, RunId: name.Run(), PageSize: size, PageToken: token})
		if err != nil {
			return nil, err
		}
		var page *native.TracePage
		result := &api.QueryRunTraceResponse{Run: request.Parent}
		switch v := value.Result.(type) {
		case *native.ReadRunTraceResponse_Page:
			page = v.Page
		case *native.ReadRunTraceResponse_Truncated:
			page = v.Truncated.Page
			reason := api.ObservationTraceTruncationReason(v.Truncated.Reason)
			result.TruncationReason = &reason
		case *native.ReadRunTraceResponse_NotRetained:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
		case *native.ReadRunTraceResponse_StaleCursor:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
		default:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
		}
		result.NextPageToken, err = observationNext(page.Continuation)
		if err != nil {
			return nil, err
		}
		result.Cuts = p.projectTraceCutMetadata(page.Cuts)
		for _, row := range page.Rows {
			result.Rows = append(result.Rows, p.projectTraceRow(row))
		}
		return result, nil
	})
}
func (h *observationV2Handler) QueryRecorderCoverage(ctx context.Context, r *connect.Request[api.QueryRecorderCoverageRequest]) (*connect.Response[api.QueryRecorderCoverageResponse], error) {
	var request *api.QueryRecorderCoverageRequest
	if r != nil {
		request = r.Msg
	}
	return observationRead(ctx, h, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, request, func(p *observationProjection) (*api.QueryRecorderCoverageResponse, error) {
		if err := p.parent(request.Parent); err != nil {
			return nil, err
		}
		if request.From == nil {
			return nil, contracts.ErrInvalidObservabilityContract
		}
		from, err := apitypes.ParseDecimalUint64(request.From.Sequence)
		if err != nil {
			return nil, contracts.ErrInvalidObservabilityContract
		}
		query := &native.ReadRecorderCoverageRequest{From: &native.AdmissionPosition{Epoch: append([]byte(nil), request.From.Epoch...), Sequence: from.Value()}}
		switch v := request.Through.(type) {
		case *api.QueryRecorderCoverageRequest_Position:
			if v == nil || v.Position == nil {
				return nil, contracts.ErrInvalidObservabilityContract
			}
			through, err := apitypes.ParseDecimalUint64(v.Position.Sequence)
			if err != nil {
				return nil, contracts.ErrInvalidObservabilityContract
			}
			query.Through = &native.ReadRecorderCoverageRequest_Position{Position: &native.AdmissionPosition{Epoch: append([]byte(nil), v.Position.Epoch...), Sequence: through.Value()}}
		case *api.QueryRecorderCoverageRequest_LatestInEpoch:
			if v == nil || v.LatestInEpoch == nil {
				return nil, contracts.ErrInvalidObservabilityContract
			}
			query.Through = &native.ReadRecorderCoverageRequest_LatestInEpoch{LatestInEpoch: &native.RecorderEpochIdentity{Value: append([]byte(nil), v.LatestInEpoch.Value...)}}
		default:
			return nil, contracts.ErrInvalidObservabilityContract
		}
		value, err := h.reader.ReadRecorderCoverage(p.ctx, query)
		if err != nil {
			return nil, err
		}
		switch v := value.Result.(type) {
		case *native.ReadRecorderCoverageResponse_Complete:
			return &api.QueryRecorderCoverageResponse{Coverage: p.projectRecorderCoverageSnapshot(v.Complete)}, nil
		case *native.ReadRecorderCoverageResponse_Truncated:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		case *native.ReadRecorderCoverageResponse_NotRetained:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
		default:
			return nil, observationFailure(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
		}
	})
}
