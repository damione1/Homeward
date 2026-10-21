package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"github.com/damione1/Homeward/gen/home/history/v1/historyv1connect"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (h *historyHandler) QueryExecutionTraces(ctx context.Context, r *connect.Request[history.QueryExecutionTracesRequest]) (*connect.Response[history.QueryExecutionTracesResponse], error) {
	ctx, cancel := context.WithTimeout(ctx, historyquery.QueryTimeout)
	defer cancel()
	if r == nil {
		return nil, historyRPCError(historyquery.ErrInvalid)
	}
	q, token, err := contracts.AdmitHistoryRequest(r.Msg)
	if err != nil {
		return nil, historyRPCError(err)
	}
	leased, admitted, release, err := h.begin(ctx, q)
	if err != nil {
		return nil, historyRPCError(err)
	}
	defer release()
	procedure := historyv1connect.HistoryServiceQueryExecutionTracesProcedure
	var cursor *storage.HistoryPageCursor
	if token != "" {
		raw, e := h.runtime.codec.Open(procedure, q, admitted.binding, token)
		if e == nil {
			cursor, e = storage.ParseHistoryPageCursor(raw)
		}
		if e != nil {
			return historyResponse(leased, h, q, admitted.binding, &history.QueryExecutionTracesResponse{Result: &history.QueryExecutionTracesResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}})
		}
	}
	var epoch storage.HistoryEpoch
	if q.Epoch() != "" {
		epoch, err = storage.ParseHistoryEpoch(q.Epoch())
		if err != nil {
			return nil, historyRPCError(err)
		}
	}
	start, end := q.Bounds()
	page, err := h.runtime.reader.ReadExecutionHistoryPage(leased, storage.HistoryPageQuery{Epoch: epoch, Since: start, Until: end, ClipRetention: true, HoldReservation: true, Limit: q.Limit(), Cursor: cursor})
	defer page.Release()
	if cause := historyContextError(leased); cause != nil {
		return nil, historyRPCError(cause)
	}
	if errors.Is(err, storage.ErrHistoryRetentionAdvanced) {
		return historyResponse(leased, h, q, admitted.binding, &history.QueryExecutionTracesResponse{Result: &history.QueryExecutionTracesResponse_StaleCursor{StaleCursor: &emptypb.Empty{}}})
	}
	if err != nil {
		return nil, historyRPCError(err)
	}
	if _, err = h.capture(leased, q, &admitted.binding); err != nil {
		return nil, historyRPCError(err)
	}
	scope := historyScope(q, page.Cutoff, page.AsOf)
	response := &history.QueryExecutionTracesResponse{}
	if !end.After(page.Cutoff) {
		response.Result = &history.QueryExecutionTracesResponse_NotRetained{NotRetained: scope}
		return historyResponse(leased, h, q, admitted.binding, response)
	}
	wire := &history.ExecutionTracePage{Scope: scope, Epoch: page.Epoch.String(), InterruptedPredecessor: page.InterruptedPredecessor, EpochInterrupted: page.EpochState == storage.HistoryEpochInterrupted, ScopeReduced: page.ScopeReduced, Checkpoint: page.Checkpoint, CommitHigh: uint64(page.CommitHigh), Continuation: historyEnd()}
	if page.PreviousEpoch.Valid() {
		text := page.PreviousEpoch.String()
		wire.PreviousEpoch = &text
	}
	for _, record := range page.Records {
		point, err := historyTrace(record.Record)
		if err != nil {
			return nil, historyRPCError(err)
		}
		wire.Points = append(wire.Points, point)
	}
	for _, loss := range page.Losses {
		reason, err := historyLossReason(loss.Reason)
		if err != nil {
			return nil, historyRPCError(err)
		}
		wire.Losses = append(wire.Losses, &history.TraceLoss{Reason: reason, KnownCount: loss.KnownCount, From: timestamppb.New(loss.From), Through: timestamppb.New(loss.Through), UncertainEnvelope: loss.UncertainEnvelope})
	}
	if page.Next != nil {
		raw, err := page.Next.MarshalBinary()
		if err != nil {
			return nil, historyRPCError(err)
		}
		next, err := h.runtime.codec.Seal(procedure, q, admitted.binding, raw)
		if err != nil {
			return nil, historyRPCError(err)
		}
		wire.Continuation = &history.Continuation{Result: &history.Continuation_NextPageToken{NextPageToken: next}}
	}
	response.Result = &history.QueryExecutionTracesResponse_Page{Page: wire}
	return historyResponse(leased, h, q, admitted.binding, response)
}
