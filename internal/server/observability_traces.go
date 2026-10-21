package server

import (
	"bytes"
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/types/known/emptypb"
)

func (h *observationHandler) ReadRunTrace(ctx context.Context, r *connect.Request[obs.ReadRunTraceRequest]) (*connect.Response[obs.ReadRunTraceResponse], error) {
	if r == nil || contracts.AdmitObservabilityRequest(r.Msg) != nil {
		return nil, observationRPCError(contracts.ErrInvalidObservabilityContract)
	}
	requested, err := observability.ParseControllerHandle(r.Msg.Controller)
	if err != nil {
		return nil, observationRPCError(err)
	}
	size, err := observability.PageSizeFromOptional(r.Msg.PageSize)
	if err != nil {
		return nil, observationRPCError(err)
	}
	var response *obs.ReadRunTraceResponse
	err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(binding observability.ReadBinding) error {
		process, err := h.runtime.epoch.Wire()
		if err != nil {
			return err
		}
		if !bytes.Equal(r.Msg.Controller.Value[:16], process.Value) {
			response = &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}
			return nil
		}
		if h.runtime.runs == nil {
			response = &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_NotRetained{NotRetained: &emptypb.Empty{}}}
			return nil
		}
		current, err := h.runtime.runs.ObservationHandle()
		if err != nil {
			return err
		}
		if !current.Equal(requested) {
			response = &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}
			return nil
		}
		var request observability.TracePageRequest
		if r.Msg.PageToken != nil {
			cursor, err := h.runtime.codec.DecodeTrace(*r.Msg.PageToken, binding, requested, r.Msg.RunId)
			if err != nil {
				response = &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}
				return nil
			}
			request, err = observability.TracePageRequestFromCursor(size, cursor)
			if err != nil {
				return err
			}
		} else {
			// Reserve the entire physical image allowance before even the safe native
			// projection allocates its first row. Copies share this one private state.
			reservation, err := h.runtime.captures.Reserve(binding, requested, r.Msg.RunId)
			if err != nil {
				return err
			}
			defer reservation.Release()
			rows, cuts, truncated, err := h.runtime.runs.ObservationTrace(automationruns.RunID(r.Msg.RunId))
			if err != nil {
				reservation.Release()
				return err
			}
			if len(rows) == 0 {
				reservation.Release()
				response = &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_NotRetained{NotRetained: &emptypb.Empty{}}}
				return nil
			}
			var handle observability.CaptureHandle
			if truncated {
				handle, err = reservation.CommitTruncated(rows, cuts, obs.TraceTruncationReason_TRACE_TRUNCATION_REASON_ROW_LIMIT)
			} else {
				handle, err = reservation.Commit(rows, cuts)
			}
			if err != nil {
				reservation.Release()
				return err
			}
			request, err = observability.NewTracePageRequest(size, handle)
			if err != nil {
				reservation.Release()
				return err
			}
		}
		page, err := h.runtime.captures.Read(request, binding)
		if err != nil {
			var stale *observability.Error
			if errors.As(err, &stale) && stale.Code() == obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_STALE_CURSOR {
				response = &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}}
				return nil
			}
			return err
		}
		token := ""
		if next, more := page.Next(); more {
			token, err = h.runtime.codec.EncodeTrace(next)
			if err != nil {
				return err
			}
		}
		response, err = page.Wire(token)
		return err
	})
	if err != nil {
		return nil, observationRPCError(err)
	}
	return observationResponse(response)
}
