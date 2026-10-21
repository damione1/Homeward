package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type observationHandler struct{ runtime *observationRuntime }

func observationRPCError(err error) error {
	code, field, status := obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_NONE, connect.CodeUnavailable
	var admitted *observability.Error
	switch {
	case errors.As(err, &admitted):
		code, field = admitted.Code(), admitted.Wire().Field
	case errors.Is(err, context.DeadlineExceeded):
		code = obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_DEADLINE_EXCEEDED
	case errors.Is(err, authorization.ErrDenied):
		code = obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_DENIED
	case errors.Is(err, contracts.ErrInvalidObservabilityContract):
		code = obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT
	}
	switch code {
	case obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT:
		status = connect.CodeInvalidArgument
	case obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_DENIED:
		status = connect.CodePermissionDenied
	case obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAUTHENTICATED:
		status = connect.CodeUnauthenticated
	case obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CAPACITY:
		status = connect.CodeResourceExhausted
	case obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_STALE_CURSOR:
		status = connect.CodeFailedPrecondition
	case obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_DEADLINE_EXCEEDED:
		status = connect.CodeDeadlineExceeded
	}
	result := connect.NewError(status, errors.New("observability request rejected"))
	if detail, err := connect.NewErrorDetail(&obs.ObservabilityError{Code: code, Field: field}); err == nil {
		result.AddDetail(detail)
	}
	return result
}
func (h *observationHandler) withCurrent(ctx context.Context, family auth.ScopeFamily, expected *observability.ReadBinding, fn func(observability.ReadBinding) error) error {
	if h == nil || h.runtime == nil || h.runtime.authority == nil || ctx == nil {
		return authorization.ErrUnavailable
	}
	actor, present := ActorFromContext(ctx)
	if !present {
		return authorization.ErrDenied
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return authorization.ErrDenied
	}
	admitted, err := principal.Actor()
	if err != nil || actor != admitted {
		return authorization.ErrDenied
	}
	permission, err := authorization.NewPermission(family, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return err
	}
	object, err := authorization.AdministrativeObject(family)
	if err != nil {
		return err
	}
	return h.runtime.authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		if err := current.Authorize(permission, object); err != nil {
			return err
		}
		revision, digest, err := current.Binding()
		if err != nil {
			return err
		}
		binding, err := observability.NewReadBinding(revision, digest)
		if err != nil {
			return err
		}
		if expected != nil && !binding.Equal(*expected) {
			return authorization.ErrDenied
		}
		return fn(binding)
	})
}
func observationResponse[T any](message *T) (*connect.Response[T], error) {
	value, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitObservabilityResponse(value) != nil {
		return nil, observationRPCError(authorization.ErrUnavailable)
	}
	return connect.NewResponse(message), nil
}
func (h *observationHandler) GetHealth(ctx context.Context, r *connect.Request[obs.GetHealthRequest]) (*connect.Response[obs.GetHealthResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	var result *obs.GetHealthResponse
	err := h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(observability.ReadBinding) error {
		var err error
		result, err = h.runtime.healthSnapshot()
		return err
	})
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(result)
}
func (h *observationHandler) GetMetrics(ctx context.Context, r *connect.Request[obs.GetMetricsRequest]) (*connect.Response[obs.GetMetricsResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	var result *obs.GetMetricsResponse
	err := h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(observability.ReadBinding) error {
		var err error
		result, err = h.runtime.metricsSnapshot()
		return err
	})
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(result)
}
func (h *observationHandler) GetIntegrationHealth(ctx context.Context, r *connect.Request[obs.GetIntegrationHealthRequest]) (*connect.Response[obs.GetIntegrationHealthResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	instance, err := domain.ParseIntegrationInstanceID(r.Msg.IntegrationInstanceId)
	if err != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	result := &obs.GetIntegrationHealthResponse{}
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, nil, func(observability.ReadBinding) error {
		var err error
		result.Report, err = h.runtime.sequence.IntegrationHealth(instance)
		if err != nil {
			return err
		}
		result.Supervision, err = h.runtime.supervisor.ObservationIntegration(instance)
		return err
	})
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(result)
}
func (h *observationHandler) QueryRuntimeLogs(ctx context.Context, r *connect.Request[obs.QueryRuntimeLogsRequest]) (*connect.Response[obs.QueryRuntimeLogsResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	size, err := observability.PageSizeFromOptional(r.Msg.PageSize)
	if err != nil {
		return nil, observationRPCError(err)
	}
	var result *obs.QueryRuntimeLogsResponse
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(binding observability.ReadBinding) error {
		var cursor observability.LogCursor
		if r.Msg.PageToken != nil {
			var err error
			cursor, err = h.runtime.codec.DecodeLogs(*r.Msg.PageToken, binding)
			if err != nil {
				result = &obs.QueryRuntimeLogsResponse{Result: &obs.QueryRuntimeLogsResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}
				return nil
			}
		} else {
			snapshot, err := h.runtime.journal.Snapshot()
			if err != nil {
				return err
			}
			cursor, err = h.runtime.codec.BeginLogs(binding, snapshot.Through())
			if err != nil {
				return err
			}
		}
		request, err := observability.NewLogPageRequest(size, cursor)
		if err != nil {
			return err
		}
		page, err := h.runtime.journal.Read(request)
		if err != nil {
			return err
		}
		token := ""
		if next, more := page.Next(); more {
			token, err = h.runtime.codec.EncodeLogs(next)
			if err != nil {
				return err
			}
		}
		result, err = page.Wire(token)
		return err
	})
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(result)
}
func (h *observationHandler) auditRoute(raw string, present bool) *obs.AuditRoute {
	if !present {
		return &obs.AuditRoute{Result: &obs.AuditRoute_NotRecorded{NotRecorded: &emptypb.Empty{}}}
	}
	if h.runtime.public != nil {
		if route, present := h.runtime.public.routes[routeLookup{method: "POST", path: raw}]; present && route.key.Kind() == RouteKindConnect {
			return &obs.AuditRoute{Result: &obs.AuditRoute_RegisteredProcedure{RegisteredProcedure: route.key.Path()}}
		}
	}
	return &obs.AuditRoute{Result: &obs.AuditRoute_OtherRegisteredRoute{OtherRegisteredRoute: &emptypb.Empty{}}}
}
func auditEnd() *obs.PageContinuation {
	return &obs.PageContinuation{Result: &obs.PageContinuation_End{End: &emptypb.Empty{}}}
}
func (h *observationHandler) QueryAudits(ctx context.Context, r *connect.Request[obs.QueryAuditsRequest]) (*connect.Response[obs.QueryAuditsResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	size, err := observability.PageSizeFromOptional(r.Msg.PageSize)
	if err != nil {
		return nil, observationRPCError(err)
	}
	var binding observability.ReadBinding
	if err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(value observability.ReadBinding) error { binding = value; return nil }); err != nil {
		return nil, observationRPCError(err)
	}
	position := storage.FirstAuditPosition()
	var cursor observability.AuditCursor
	if r.Msg.PageToken != nil {
		cursor, err = h.runtime.codec.DecodeAudits(*r.Msg.PageToken, binding, r.Msg.Source)
		if err != nil {
			return observationResponse(&obs.QueryAuditsResponse{Result: &obs.QueryAuditsResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}})
		}
		upper, before, err := cursor.Position()
		if err != nil {
			return nil, observationRPCError(err)
		}
		position, err = storage.AuditPositionAfter(upper, before)
		if err != nil {
			return nil, observationRPCError(err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	page := &obs.AuditPage{Source: r.Msg.Source, Continuation: auditEnd()}
	var next storage.AuditPosition
	var more bool
	switch r.Msg.Source {
	case obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION:
		request, err := storage.NewAuthenticationAuditRead(size, position)
		if err != nil {
			return nil, observationRPCError(err)
		}
		stored, err := h.runtime.store.ReadAuthenticationAudits(bounded, request)
		if err != nil {
			return nil, observationRPCError(err)
		}
		page.UpperId, err = stored.Upper()
		if err != nil {
			return nil, observationRPCError(err)
		}
		for _, row := range stored.Rows() {
			value := &obs.AuditRow{Id: row.ID()}
			if invalid := row.MetadataInvalid(); invalid != nil {
				value.Result = &obs.AuditRow_StoredMetadataInvalid{StoredMetadataInvalid: invalid}
			} else {
				at, present := row.At()
				if !present {
					return nil, observationRPCError(authorization.ErrUnavailable)
				}
				author := &obs.AuditAuthor{Result: &obs.AuditAuthor_NotRecorded{NotRecorded: &emptypb.Empty{}}}
				if account, present := row.Actor(); present {
					text, err := account.Value()
					if err != nil {
						return nil, observationRPCError(err)
					}
					author.Result = &obs.AuditAuthor_HumanAccount{HumanAccount: &auth.AccountId{Value: text}}
				}
				route, present := row.RouteMetadata()
				value.Result = &obs.AuditRow_Authentication{Authentication: &obs.AuthenticationAudit{OccurredAt: timestamppb.New(at), Action: row.Action(), Outcome: row.Outcome(), Author: author, Route: h.auditRoute(route, present)}}
			}
			page.Rows = append(page.Rows, value)
		}
		next, more = stored.Next()
	case obs.AuditSource_AUDIT_SOURCE_SECRET_ACCESS:
		request, err := storage.NewSecretAccessAuditRead(size, position)
		if err != nil {
			return nil, observationRPCError(err)
		}
		stored, err := h.runtime.store.ReadSecretAccessAudits(bounded, request)
		if err != nil {
			return nil, observationRPCError(err)
		}
		page.UpperId, err = stored.Upper()
		if err != nil {
			return nil, observationRPCError(err)
		}
		for _, row := range stored.Rows() {
			value := &obs.AuditRow{Id: row.ID()}
			if invalid := row.MetadataInvalid(); invalid != nil {
				value.Result = &obs.AuditRow_StoredMetadataInvalid{StoredMetadataInvalid: invalid}
			} else {
				at, present := row.At()
				if !present {
					return nil, observationRPCError(authorization.ErrUnavailable)
				}
				requester, present := row.Requester()
				if !present {
					return nil, observationRPCError(authorization.ErrUnavailable)
				}
				reference, err := h.runtime.codec.IntegrationReference(requester)
				if err != nil {
					return nil, observationRPCError(err)
				}
				opaque, err := reference.Wire()
				if err != nil {
					return nil, observationRPCError(err)
				}
				value.Result = &obs.AuditRow_SecretAccess{SecretAccess: &obs.SecretAccessAudit{OccurredAt: timestamppb.New(at), Reason: row.Reason(), Author: &obs.AuditAuthor{Result: &obs.AuditAuthor_InternalIntegration{InternalIntegration: opaque}}}}
			}
			page.Rows = append(page.Rows, value)
		}
		next, more = stored.Next()
	default:
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	if more {
		if r.Msg.PageToken == nil {
			cursor, err = h.runtime.codec.BeginAudits(binding, r.Msg.Source, page.UpperId)
			if err != nil {
				return nil, observationRPCError(err)
			}
		}
		_, before, _, err := next.Values()
		if err != nil {
			return nil, observationRPCError(err)
		}
		cursor, err = cursor.After(before)
		if err != nil {
			return nil, observationRPCError(err)
		}
		token, err := h.runtime.codec.EncodeAudits(cursor)
		if err != nil {
			return nil, observationRPCError(err)
		}
		page.Continuation = &obs.PageContinuation{Result: &obs.PageContinuation_NextPageToken{NextPageToken: token}}
	}
	response := &obs.QueryAuditsResponse{Result: &obs.QueryAuditsResponse_Page{Page: page}}
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, &binding, func(observability.ReadBinding) error { return contracts.AdmitObservabilityResponse(response) })
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(response)
}
