package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (h *historyV2Handler) QueryExecutionTraces(ctx context.Context, r *connect.Request[api.QueryExecutionTracesRequest]) (response *connect.Response[api.QueryExecutionTracesResponse], callErr error) {
	defer func() {
		if callErr != nil {
			callErr = apiRPCError(ctx, historyFault(callErr))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, historyquery.QueryTimeout)
	retained := false
	defer func() {
		if !retained {
			cancel()
		}
	}()
	var request *api.QueryExecutionTracesRequest
	if r != nil {
		request = r.Msg
	}
	selection, entity, err := h.selectQuery(ctx, historyquery.ExecutionTraces, request)
	if err != nil {
		return nil, err
	}
	epochName, err := h.openEpoch(ctx, selection.Home(), selection.ExecutionEpoch())
	if err != nil {
		return nil, err
	}
	q, err := selection.NativeQuery(entity, epochName)
	if err != nil {
		return nil, err
	}
	leased, admitted, release, err := h.native.begin(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !retained {
			release()
		}
	}()
	procedure := apiv2connect.HistoryServiceQueryExecutionTracesProcedure
	var cursor *storage.HistoryPageCursor
	if selection.PageToken() != "" {
		raw, err := h.native.runtime.codec.Open(procedure, q, admitted.binding, selection.PageToken())
		if err != nil {
			return nil, historyquery.ErrStale
		}
		cursor, err = storage.ParseHistoryPageCursor(raw)
		if err != nil {
			return nil, historyquery.ErrStale
		}
	}
	var epoch storage.HistoryEpoch
	if epochName != "" {
		epoch, err = storage.ParseHistoryEpoch(epochName)
		if err != nil {
			return nil, historyquery.ErrInvalid
		}
	}
	start, end := q.Bounds()
	page, err := h.native.runtime.reader.ReadExecutionHistoryPage(leased, storage.HistoryPageQuery{Epoch: epoch, Since: start, Until: end, ClipRetention: true, HoldReservation: true, Limit: q.Limit(), Cursor: cursor})
	defer func() {
		if !retained {
			page.Release()
		}
	}()
	if cause := historyContextError(leased); cause != nil {
		return nil, cause
	}
	if errors.Is(err, storage.ErrHistoryRetentionAdvanced) {
		return nil, historyquery.ErrStale
	}
	if err != nil {
		return nil, err
	}
	if _, err = h.native.capture(leased, q, &admitted.binding); err != nil {
		return nil, err
	}
	out := &api.QueryExecutionTracesResponse{Scope: publicHistoryScope(q, page.Cutoff, page.AsOf)}
	if !end.After(page.Cutoff) {
		return historyV2Response(leased, h, q, admitted.binding, selection.Home(), out, admitted.ready, func() { page.Release(); release(); cancel() }, &retained)
	}
	out.ExecutionEpoch, err = h.epochToken(leased, selection.Home(), page.Epoch)
	if err != nil {
		return nil, err
	}
	if page.PreviousEpoch.Valid() {
		out.PreviousExecutionEpoch, err = h.epochToken(leased, selection.Home(), page.PreviousEpoch)
		if err != nil {
			return nil, err
		}
	}
	out.InterruptedPredecessor = page.InterruptedPredecessor
	out.EpochInterrupted = page.EpochState == storage.HistoryEpochInterrupted
	out.ScopeReduced = page.ScopeReduced
	out.Checkpoint = page.Checkpoint
	out.CommitHigh = uint64(page.CommitHigh)
	for _, record := range page.Records {
		if record.Record.ID().Epoch() != page.Epoch {
			return nil, historyquery.ErrIntegrity
		}
		point, err := publicHistoryTrace(selection.Home(), record.Record)
		if err != nil {
			return nil, err
		}
		out.ExecutionTraces = append(out.ExecutionTraces, point)
	}
	for _, loss := range page.Losses {
		reason, err := publicHistoryLossReason(loss.Reason)
		if err != nil {
			return nil, err
		}
		out.Losses = append(out.Losses, &api.HistoryTraceLoss{Reason: reason, KnownCount: loss.KnownCount, From: timestamppb.New(loss.From), Through: timestamppb.New(loss.Through), UncertainEnvelope: loss.UncertainEnvelope})
	}
	if page.Next != nil {
		raw, err := page.Next.MarshalBinary()
		if err != nil {
			return nil, err
		}
		out.NextPageToken, err = h.native.runtime.codec.Seal(procedure, q, admitted.binding, raw)
		if err != nil {
			return nil, err
		}
	}
	return historyV2Response(leased, h, q, admitted.binding, selection.Home(), out, admitted.ready, func() { page.Release(); release(); cancel() }, &retained)
}
