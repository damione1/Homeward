package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"github.com/damione1/Homeward/gen/home/history/v1/historyv1connect"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/historycoverage"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type historyHandler struct{ runtime *historyRuntime }

func historyRPCError(err error) error {
	code, status := history.ErrorCode_ERROR_CODE_UNAVAILABLE, connect.CodeUnavailable
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code, status = history.ErrorCode_ERROR_CODE_DEADLINE, connect.CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		code, status = history.ErrorCode_ERROR_CODE_UNAVAILABLE, connect.CodeCanceled
	case errors.Is(err, authorization.ErrDenied):
		code, status = history.ErrorCode_ERROR_CODE_DENIED, connect.CodePermissionDenied
	case errors.Is(err, historyquery.ErrInvalid), errors.Is(err, contracts.ErrInvalidHistoryContract), errors.Is(err, storage.ErrHistoryInvalid):
		code, status = history.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, connect.CodeInvalidArgument
	case errors.Is(err, historyquery.ErrPointTooLarge):
		code, status = history.ErrorCode_ERROR_CODE_POINT_TOO_LARGE, connect.CodeResourceExhausted
	case errors.Is(err, historyquery.ErrBudget), errors.Is(err, authorization.ErrCapacity):
		code, status = history.ErrorCode_ERROR_CODE_CAPACITY, connect.CodeResourceExhausted
	case errors.Is(err, historyquery.ErrIntegrity), errors.Is(err, storage.ErrHistoryIntegrity):
		code, status = history.ErrorCode_ERROR_CODE_INTEGRITY, connect.CodeDataLoss
	}
	result := connect.NewError(status, errors.New("history request rejected"))
	if detail, e := connect.NewErrorDetail(&history.HistoryError{Code: code}); e == nil {
		result.AddDetail(detail)
	}
	return result
}
func historyResponse[T any](ctx context.Context, h *historyHandler, q historyquery.Query, binding historyquery.Binding, message *T) (*connect.Response[T], error) {
	if _, err := h.capture(ctx, q, &binding); err != nil {
		return nil, historyRPCError(err)
	}
	wire, ok := any(message).(proto.Message)
	if ok && proto.Size(wire) > historyquery.MaxResponseBytes {
		return nil, historyRPCError(historyquery.ErrBudget)
	}
	if !ok || contracts.AdmitHistoryResponse(wire) != nil {
		return nil, historyRPCError(historyquery.ErrIntegrity)
	}
	if cause := historyContextError(ctx); cause != nil {
		return nil, historyRPCError(cause)
	}
	return connect.NewResponse(message), nil
}
func historyPeriod(start, end time.Time) *history.Period {
	return &history.Period{Start: timestamppb.New(start), End: timestamppb.New(end)}
}
func historyScope(q historyquery.Query, cutoff, asOf time.Time) *history.ReadScope {
	start, end := q.Bounds()
	resolution := history.Resolution_RESOLUTION_RAW
	maxPeriod := uint32(168)
	maxPoints := uint32(historyquery.MaxPoints)
	retainedStart := cutoff
	if q.Kind() == historyquery.Aggregates {
		resolution = history.Resolution_RESOLUTION_HOUR
		maxPeriod = 500
		retainedStart = cutoff.Truncate(time.Hour)
	}
	if q.Kind() == historyquery.ExecutionTraces {
		maxPoints = historyquery.MaxTracePoints
	}
	scope := &history.ReadScope{Requested: historyPeriod(start, end), RetentionCutoff: timestamppb.New(cutoff), AsOf: timestamppb.New(asOf), Resolution: resolution, Limits: &history.Limits{MaxPoints: maxPoints, MaxResponseBytes: historyquery.MaxResponseBytes, MaxTokenBytes: historyquery.MaxTokenBytes, QueryTimeoutMillis: 2000, MaxReaders: 2, MaxPeriodHours: maxPeriod}}
	if start.Before(retainedStart) {
		until := end
		if retainedStart.Before(until) {
			until = retainedStart
		}
		scope.NotRetainedPrefix = historyPeriod(start, until)
		start = retainedStart
	}
	if start.Before(end) {
		scope.Retained = historyPeriod(start, end)
	}
	return scope
}
func historyEnd() *history.Continuation {
	return &history.Continuation{Result: &history.Continuation_End{End: &emptypb.Empty{}}}
}
func historyDescriptor(n recordermaintenance.NumericInput) *history.NumericDescriptor {
	return &history.NumericDescriptor{Unit: n.Unit, CanonicalUnit: n.CanonicalUnit, Scale: n.Scale, Offset: n.Offset, Dimension: history.Dimension(n.Dimension), Precision: int32(n.Precision), Semantic: registry.MeasurementSemantic(n.Semantic), Aggregation: registry.MeasurementAggregation(n.Aggregation)}
}
func historyCoverage(snapshot storage.RecorderCoverageSnapshot, tail bool) (*history.Coverage, error) {
	converted, err := observationCoverage(snapshot)
	if err != nil {
		return nil, historyquery.ErrIntegrity
	}
	if converted.GetTruncated() != nil {
		return nil, historyquery.ErrBudget
	}
	projected, err := historycoverage.FromRecorderSnapshot(snapshot, nil)
	if err != nil {
		return nil, historyquery.ErrIntegrity
	}
	verdict := history.CoverageVerdict_COVERAGE_VERDICT_INCOMPLETE
	if projected.Verdict() == historycoverage.CompleteUnderPolicy && !tail {
		verdict = history.CoverageVerdict_COVERAGE_VERDICT_COMPLETE_UNDER_POLICY
	}
	coverage := &history.Coverage{Logical: converted.GetComplete(), Verdict: verdict, AnalyticalTailUnproven: tail}
	for _, issue := range projected.Issues() {
		if issue.Kind() == historycoverage.UncommittedTail {
			from, through := issue.Bounds()
			a, err := observationCoveragePosition(from.Epoch(), from.Sequence())
			if err != nil {
				return nil, err
			}
			b, err := observationCoveragePosition(through.Epoch(), through.Sequence())
			if err != nil {
				return nil, err
			}
			coverage.RecorderTail = &history.RecorderTail{From: a, Through: b, UnknownUtc: &emptypb.Empty{}}
			coverage.Logical.Through = a
			coverage.Verdict = history.CoverageVerdict_COVERAGE_VERDICT_INCOMPLETE
		}
	}
	for _, r := range snapshot.Retention() {
		from, through := r.Bounds()
		a, err := observationCoveragePosition(r.Epoch(), from)
		if err != nil {
			return nil, err
		}
		b, err := observationCoveragePosition(r.Epoch(), through)
		if err != nil {
			return nil, err
		}
		coverage.Retention = append(coverage.Retention, &history.RetentionRange{From: a, Through: b, Snapshot: r.Snapshot()})
	}
	return coverage, nil
}
func (h *historyHandler) recorderPage(ctx context.Context, q historyquery.Query, token, procedure string) (context.Context, historyAdmission, storage.RecorderQueryPage, func(), error) {
	leased, admitted, release, err := h.begin(ctx, q)
	if err != nil {
		return nil, admitted, storage.RecorderQueryPage{}, nil, err
	}
	var cursor *storage.RecorderQueryCursor
	if token != "" {
		raw, e := h.runtime.codec.Open(procedure, q, admitted.binding, token)
		if e == nil {
			cursor, e = storage.ParseRecorderQueryCursor(raw)
		}
		if e != nil {
			release()
			return nil, admitted, storage.RecorderQueryPage{}, nil, historyquery.ErrStale
		}
	}
	page, err := h.runtime.reader.ReadHistoricalRecorderPageAt(leased, q, cursor, admitted.liveHigh)
	if cause := historyContextError(leased); cause != nil {
		page.Release()
		release()
		return nil, admitted, storage.RecorderQueryPage{}, nil, cause
	}

	if err != nil {
		page.Release()
		release()
		return nil, admitted, storage.RecorderQueryPage{}, nil, err
	}
	if _, err = h.capture(leased, q, &admitted.binding); err != nil {
		page.Release()
		release()
		return nil, admitted, storage.RecorderQueryPage{}, nil, err
	}
	return leased, admitted, page, func() { page.Release(); release() }, nil
}
func (h *historyHandler) continuation(procedure string, q historyquery.Query, binding historyquery.Binding, cursor *storage.RecorderQueryCursor) (*history.Continuation, error) {
	if cursor == nil {
		return historyEnd(), nil
	}
	raw, err := cursor.MarshalBinary()
	if err != nil {
		return nil, err
	}
	token, err := h.runtime.codec.Seal(procedure, q, binding, raw)
	if err != nil {
		return nil, err
	}
	return &history.Continuation{Result: &history.Continuation_NextPageToken{NextPageToken: token}}, nil
}
func (h *historyHandler) QueryObservations(ctx context.Context, r *connect.Request[history.QueryObservationsRequest]) (*connect.Response[history.QueryObservationsResponse], error) {
	ctx, cancel := context.WithTimeout(ctx, historyquery.QueryTimeout)
	defer cancel()
	if r == nil {
		return nil, historyRPCError(historyquery.ErrInvalid)
	}
	q, token, err := contracts.AdmitHistoryRequest(r.Msg)
	if err != nil {
		return nil, historyRPCError(err)
	}
	procedure := historyv1connect.HistoryServiceQueryObservationsProcedure
	leased, admitted, page, release, err := h.recorderPage(ctx, q, token, procedure)
	if errors.Is(err, historyquery.ErrStale) {
		return connect.NewResponse(&history.QueryObservationsResponse{Result: &history.QueryObservationsResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}), nil
	}
	if err != nil {
		return nil, historyRPCError(err)
	}
	defer release()
	scope := historyScope(q, page.RawCutoff, page.AsOf)
	response := &history.QueryObservationsResponse{}
	if page.NotRetained {
		response.Result = &history.QueryObservationsResponse_NotRetained{NotRetained: scope}
	} else {
		coverage, err := historyCoverage(page.Coverage, false)
		if errors.Is(err, historyquery.ErrBudget) {
			response.Result = &history.QueryObservationsResponse_Budget{Budget: &history.BudgetExceeded{Reason: history.BudgetReason_BUDGET_REASON_COVERAGE}}
			return historyResponse(leased, h, q, admitted.binding, response)
		}
		if err != nil {
			return nil, historyRPCError(err)
		}
		wire := &history.ObservationPage{Scope: scope, Coverage: coverage}
		wire.Continuation, err = h.continuation(procedure, q, admitted.binding, page.Next)
		if err != nil {
			return nil, historyRPCError(err)
		}
		for _, point := range page.Observations {
			position, err := observationCoveragePosition(point.Epoch, point.Sequence)
			if err != nil {
				return nil, historyRPCError(err)
			}
			value := &history.Observation{Position: position, State: point.State}
			if point.Descriptor != nil {
				value.Descriptor_ = historyDescriptor(*point.Descriptor)
			}
			wire.Points = append(wire.Points, value)
		}
		response.Result = &history.QueryObservationsResponse_Page{Page: wire}
	}
	return historyResponse(leased, h, q, admitted.binding, response)
}
func (h *historyHandler) QueryAggregates(ctx context.Context, r *connect.Request[history.QueryAggregatesRequest]) (*connect.Response[history.QueryAggregatesResponse], error) {
	ctx, cancel := context.WithTimeout(ctx, historyquery.QueryTimeout)
	defer cancel()
	if r == nil {
		return nil, historyRPCError(historyquery.ErrInvalid)
	}
	q, token, err := contracts.AdmitHistoryRequest(r.Msg)
	if err != nil {
		return nil, historyRPCError(err)
	}
	procedure := historyv1connect.HistoryServiceQueryAggregatesProcedure
	leased, admitted, page, release, err := h.recorderPage(ctx, q, token, procedure)
	if errors.Is(err, historyquery.ErrStale) {
		return connect.NewResponse(&history.QueryAggregatesResponse{Result: &history.QueryAggregatesResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}), nil
	}
	if err != nil {
		return nil, historyRPCError(err)
	}
	defer release()
	scope := historyScope(q, page.AggregateCutoff, page.AsOf)
	response := &history.QueryAggregatesResponse{}
	if page.NotRetained {
		response.Result = &history.QueryAggregatesResponse_NotRetained{NotRetained: scope}
	} else {
		// Analytical high-water may lag the Recorder. Even no recorded issue does
		// not prove this unprocessed suffix; keep it explicit in every page.
		tail := page.AnalyticalTailUnproven
		coverage, err := historyCoverage(page.Coverage, tail)
		if errors.Is(err, historyquery.ErrBudget) {
			response.Result = &history.QueryAggregatesResponse_Budget{Budget: &history.BudgetExceeded{Reason: history.BudgetReason_BUDGET_REASON_COVERAGE}}
			return historyResponse(leased, h, q, admitted.binding, response)
		}
		if err != nil {
			return nil, historyRPCError(err)
		}
		wire := &history.AggregatePage{Scope: scope, Coverage: coverage}
		wire.Continuation, err = h.continuation(procedure, q, admitted.binding, page.Next)
		if err != nil {
			return nil, historyRPCError(err)
		}
		for _, a := range page.Aggregates {
			values := a.Values.Input()
			point := &history.Aggregate{Bucket: historyPeriod(a.Start, a.End), Descriptor_: historyDescriptor(a.Descriptor), Quality: &state.Quality{Availability: state.Availability(a.Availability), Freshness: state.Freshness(a.Freshness), Provenance: state.Provenance(a.Provenance)}, DurationNanos: uint64(values.Duration)}
			if values.HasMeasurement {
				point.Minimum = proto.Float64(values.Minimum)
				point.Maximum = proto.Float64(values.Maximum)
			}
			if mean, present := a.Values.Mean(); present {
				point.Mean = proto.Float64(mean)
			}
			if values.HasContribution {
				point.Contribution = proto.Float64(values.Contribution)
			}
			point.Reset_, err = historyReset(values.Reset)
			if err != nil {
				return nil, historyRPCError(err)
			}
			point.Reasons, err = historyReasons(values.Reasons)
			if err != nil {
				return nil, historyRPCError(err)
			}
			wire.Points = append(wire.Points, point)
		}
		response.Result = &history.QueryAggregatesResponse_Page{Page: wire}
	}
	return historyResponse(leased, h, q, admitted.binding, response)
}
