package server

import (
	"context"
	"time"

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

type observationReader struct{ runtime *observationRuntime }

func (h *observationReader) withCurrent(ctx context.Context, family auth.ScopeFamily, expected *observability.ReadBinding, fn func(observability.ReadBinding) error) error {
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
func observationResponse[T any](message *T) (*T, error) {
	value, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitObservabilityResponse(value) != nil {
		return nil, authorization.ErrUnavailable
	}
	return message, nil
}
func (h *observationReader) GetHealth(ctx context.Context, r *obs.GetHealthRequest) (*obs.GetHealthResponse, error) {
	if r == nil || contracts.AdmitObservabilityRequest(r) != nil {
		return nil, contracts.ErrInvalidObservabilityContract
	}
	var result *obs.GetHealthResponse
	err := h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(observability.ReadBinding) error {
		var err error
		result, err = h.runtime.healthSnapshot()
		return err
	})
	if err != nil {
		return nil, err
	}
	return observationResponse(result)
}
func (h *observationReader) GetMetrics(ctx context.Context, r *obs.GetMetricsRequest) (*obs.GetMetricsResponse, error) {
	if r == nil || contracts.AdmitObservabilityRequest(r) != nil {
		return nil, contracts.ErrInvalidObservabilityContract
	}
	var result *obs.GetMetricsResponse
	err := h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(observability.ReadBinding) error {
		var err error
		result, err = h.runtime.metricsSnapshot()
		return err
	})
	if err != nil {
		return nil, err
	}
	return observationResponse(result)
}
func (h *observationReader) GetIntegrationHealth(ctx context.Context, r *obs.GetIntegrationHealthRequest) (*obs.GetIntegrationHealthResponse, error) {
	if r == nil || contracts.AdmitObservabilityRequest(r) != nil {
		return nil, contracts.ErrInvalidObservabilityContract
	}
	instance, err := domain.ParseIntegrationInstanceID(r.IntegrationInstanceId)
	if err != nil {
		return nil, contracts.ErrInvalidObservabilityContract
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
		return nil, err
	}
	return observationResponse(result)
}
func (h *observationReader) QueryRuntimeLogs(ctx context.Context, r *obs.QueryRuntimeLogsRequest) (*obs.QueryRuntimeLogsResponse, error) {
	if r == nil || contracts.AdmitObservabilityRequest(r) != nil {
		return nil, contracts.ErrInvalidObservabilityContract
	}
	size, err := observability.PageSizeFromOptional(r.PageSize)
	if err != nil {
		return nil, err
	}
	var result *obs.QueryRuntimeLogsResponse
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(binding observability.ReadBinding) error {
		var cursor observability.LogCursor
		if r.PageToken != nil {
			var err error
			cursor, err = h.runtime.codec.DecodeLogs(*r.PageToken, binding)
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
		return nil, err
	}
	return observationResponse(result)
}
func (h *observationReader) auditRoute(raw string, present bool) *obs.AuditRoute {
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
func (h *observationReader) QueryAudits(ctx context.Context, r *obs.QueryAuditsRequest) (*obs.QueryAuditsResponse, error) {
	if r == nil || contracts.AdmitObservabilityRequest(r) != nil {
		return nil, contracts.ErrInvalidObservabilityContract
	}
	size, err := observability.PageSizeFromOptional(r.PageSize)
	if err != nil {
		return nil, err
	}
	var binding observability.ReadBinding
	if err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(value observability.ReadBinding) error { binding = value; return nil }); err != nil {
		return nil, err
	}
	position := storage.FirstAuditPosition()
	var cursor observability.AuditCursor
	if r.PageToken != nil {
		cursor, err = h.runtime.codec.DecodeAudits(*r.PageToken, binding, r.Source)
		if err != nil {
			return observationResponse(&obs.QueryAuditsResponse{Result: &obs.QueryAuditsResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}})
		}
		upper, before, err := cursor.Position()
		if err != nil {
			return nil, err
		}
		position, err = storage.AuditPositionAfter(upper, before)
		if err != nil {
			return nil, err
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	page := &obs.AuditPage{Source: r.Source, Continuation: auditEnd()}
	var next storage.AuditPosition
	var more bool
	switch r.Source {
	case obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION:
		request, err := storage.NewAuthenticationAuditRead(size, position)
		if err != nil {
			return nil, err
		}
		stored, err := h.runtime.store.ReadAuthenticationAudits(bounded, request)
		if err != nil {
			return nil, err
		}
		page.UpperId, err = stored.Upper()
		if err != nil {
			return nil, err
		}
		for _, row := range stored.Rows() {
			value := &obs.AuditRow{Id: row.ID()}
			if invalid := row.MetadataInvalid(); invalid != nil {
				value.Result = &obs.AuditRow_StoredMetadataInvalid{StoredMetadataInvalid: invalid}
			} else {
				at, present := row.At()
				if !present {
					return nil, authorization.ErrUnavailable
				}
				author := &obs.AuditAuthor{Result: &obs.AuditAuthor_NotRecorded{NotRecorded: &emptypb.Empty{}}}
				if account, present := row.Actor(); present {
					text, err := account.Value()
					if err != nil {
						return nil, err
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
			return nil, err
		}
		stored, err := h.runtime.store.ReadSecretAccessAudits(bounded, request)
		if err != nil {
			return nil, err
		}
		page.UpperId, err = stored.Upper()
		if err != nil {
			return nil, err
		}
		for _, row := range stored.Rows() {
			value := &obs.AuditRow{Id: row.ID()}
			if invalid := row.MetadataInvalid(); invalid != nil {
				value.Result = &obs.AuditRow_StoredMetadataInvalid{StoredMetadataInvalid: invalid}
			} else {
				at, present := row.At()
				if !present {
					return nil, authorization.ErrUnavailable
				}
				requester, present := row.Requester()
				if !present {
					return nil, authorization.ErrUnavailable
				}
				reference, err := h.runtime.codec.IntegrationReference(requester)
				if err != nil {
					return nil, err
				}
				opaque, err := reference.Wire()
				if err != nil {
					return nil, err
				}
				value.Result = &obs.AuditRow_SecretAccess{SecretAccess: &obs.SecretAccessAudit{OccurredAt: timestamppb.New(at), Reason: row.Reason(), Author: &obs.AuditAuthor{Result: &obs.AuditAuthor_InternalIntegration{InternalIntegration: opaque}}}}
			}
			page.Rows = append(page.Rows, value)
		}
		next, more = stored.Next()
	default:
		return nil, contracts.ErrInvalidObservabilityContract
	}
	if more {
		if r.PageToken == nil {
			cursor, err = h.runtime.codec.BeginAudits(binding, r.Source, page.UpperId)
			if err != nil {
				return nil, err
			}
		}
		_, before, _, err := next.Values()
		if err != nil {
			return nil, err
		}
		cursor, err = cursor.After(before)
		if err != nil {
			return nil, err
		}
		token, err := h.runtime.codec.EncodeAudits(cursor)
		if err != nil {
			return nil, err
		}
		page.Continuation = &obs.PageContinuation{Result: &obs.PageContinuation_NextPageToken{NextPageToken: token}}
	}
	response := &obs.QueryAuditsResponse{Result: &obs.QueryAuditsResponse_Page{Page: page}}
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, &binding, func(observability.ReadBinding) error { return contracts.AdmitObservabilityResponse(response) })
	if err != nil {
		return nil, err
	}
	return observationResponse(response)
}
