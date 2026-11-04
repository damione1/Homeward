package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func historyV2Response[T any](ctx context.Context, h *historyV2Handler, q historyquery.Query, binding historyquery.Binding, home apitypes.HomeName, message *T, ready func() bool, cleanup func(), retained *bool) (*connect.Response[T], error) {
	if _, err := h.native.capture(ctx, q, &binding); err != nil {
		return nil, err
	}
	wire, ok := any(message).(proto.Message)
	if !ok || contracts.AdmitHistoryV2Response(wire, home) != nil {
		return nil, historyquery.ErrIntegrity
	}
	if proto.Size(wire) > historyquery.MaxResponseBytes {
		return nil, historyquery.ErrBudget
	}
	// Bound the actual JSON representation before Connect starts writing it.
	// There is no success-shaped budget sentinel or oversized encoder fallback.
	encoded, err := protojson.Marshal(wire)
	if err != nil {
		return nil, historyquery.ErrIntegrity
	}
	if len(encoded) > historyquery.MaxResponseBytes {
		return nil, historyquery.ErrBudget
	}
	if cause := historyContextError(ctx); cause != nil {
		return nil, cause
	}
	transferred, err := retainPublicResponse(ctx, ctx, ready, cleanup)
	if err != nil {
		return nil, err
	}
	*retained = transferred
	return connect.NewResponse(message), nil
}
func (h *historyV2Handler) nextPage(procedure string, q historyquery.Query, binding historyquery.Binding, cursor *storage.RecorderQueryCursor) (string, error) {
	if cursor == nil {
		return "", nil
	}
	raw, err := cursor.MarshalBinary()
	if err != nil {
		return "", err
	}
	return h.native.runtime.codec.Seal(procedure, q, binding, raw)
}
func (h *historyV2Handler) QueryObservations(ctx context.Context, r *connect.Request[api.QueryObservationsRequest]) (response *connect.Response[api.QueryObservationsResponse], callErr error) {
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
	var request *api.QueryObservationsRequest
	if r != nil {
		request = r.Msg
	}
	selection, entity, err := h.selectQuery(ctx, historyquery.Observations, request)
	if err != nil {
		return nil, err
	}
	q, err := selection.NativeQuery(entity, "")
	if err != nil {
		return nil, err
	}
	procedure := apiv2connect.HistoryServiceQueryObservationsProcedure
	leased, admitted, page, release, err := h.native.recorderPage(ctx, q, selection.PageToken(), procedure)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !retained {
			release()
		}
	}()
	out := &api.QueryObservationsResponse{Scope: publicHistoryScope(q, page.RawCutoff, page.AsOf)}
	if !page.NotRetained {
		out.Coverage = publicHistoryCoverage(page.ProvenanceStartedAt)
		out.NextPageToken, err = h.nextPage(procedure, q, admitted.binding, page.Next)
		if err != nil {
			return nil, err
		}
		nativeEntity, identityErr := q.Entity().Value()
		if identityErr != nil {
			return nil, historyquery.ErrIntegrity
		}
		for _, point := range page.Observations {
			if point.State == nil || point.State.Entity == nil || point.State.Entity.GetEntityId() != nativeEntity {
				return nil, historyquery.ErrIntegrity
			}
			identity, err := h.observationIdentity(point.Epoch, point.Sequence)
			if err != nil {
				return nil, err
			}
			value, err := apiresources.ProjectHistoricalState(point.State)
			if err != nil {
				return nil, historyquery.ErrIntegrity
			}
			observation := &api.HistoryObservation{ObservationId: identity, State: value}
			if point.Descriptor != nil {
				observation.Descriptor_, err = publicHistoryDescriptor(*point.Descriptor)
				if err != nil {
					return nil, err
				}
			}
			out.Observations = append(out.Observations, observation)
		}
	}
	return historyV2Response(leased, h, q, admitted.binding, selection.Home(), out, admitted.ready, func() { release(); cancel() }, &retained)
}

func (h *historyV2Handler) QueryAggregates(ctx context.Context, r *connect.Request[api.QueryAggregatesRequest]) (response *connect.Response[api.QueryAggregatesResponse], callErr error) {
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
	var request *api.QueryAggregatesRequest
	if r != nil {
		request = r.Msg
	}
	selection, entity, err := h.selectQuery(ctx, historyquery.Aggregates, request)
	if err != nil {
		return nil, err
	}
	q, err := selection.NativeQuery(entity, "")
	if err != nil {
		return nil, err
	}
	procedure := apiv2connect.HistoryServiceQueryAggregatesProcedure
	leased, admitted, page, release, err := h.native.recorderPage(ctx, q, selection.PageToken(), procedure)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !retained {
			release()
		}
	}()
	out := &api.QueryAggregatesResponse{Scope: publicHistoryScope(q, page.AggregateCutoff, page.AsOf)}
	if !page.NotRetained {
		out.Coverage = publicHistoryCoverage(page.ProvenanceStartedAt)
		out.NextPageToken, err = h.nextPage(procedure, q, admitted.binding, page.Next)
		if err != nil {
			return nil, err
		}
		nativeEntity, identityErr := q.Entity().Value()
		if identityErr != nil {
			return nil, historyquery.ErrIntegrity
		}
		for _, record := range page.Aggregates {
			if record.Entity != nativeEntity {
				return nil, historyquery.ErrIntegrity
			}
			values := record.Values.Input()
			descriptor, err := publicHistoryDescriptor(record.Descriptor)
			if err != nil {
				return nil, err
			}
			quality, err := publicHistoryQuality(record.Availability, record.Freshness, record.Provenance)
			if err != nil || values.Duration < 0 {
				return nil, historyquery.ErrIntegrity
			}
			identity, err := h.aggregateIdentity(record)
			if err != nil {
				return nil, err
			}
			point := &api.HistoryAggregate{AggregateId: identity, Bucket: publicHistoryPeriod(record.Start, record.End), Descriptor_: descriptor, Quality: quality, DurationNanos: uint64(values.Duration)}
			if values.HasMeasurement {
				point.Minimum = proto.Float64(values.Minimum)
				point.Maximum = proto.Float64(values.Maximum)
			}
			if mean, present := record.Values.Mean(); present {
				point.Mean = proto.Float64(mean)
			}
			if values.HasContribution {
				point.Contribution = proto.Float64(values.Contribution)
			}
			point.Reset_, err = publicHistoryReset(values.Reset)
			if err != nil {
				return nil, err
			}
			point.Reasons, err = publicHistoryReasons(values.Reasons)
			if err != nil {
				return nil, err
			}
			out.Aggregates = append(out.Aggregates, point)
		}
	}
	return historyV2Response(leased, h, q, admitted.binding, selection.Home(), out, admitted.ready, func() { release(); cancel() }, &retained)
}

// The per-incarnation secret prevents deriving global sequence numbers even
// when another independently authorized diagnostic reveals the native epoch.
func (h *historyV2Handler) observationIdentity(epoch storage.RecorderEpoch, sequence uint64) (string, error) {
	text, err := epoch.Hex()
	if err != nil || sequence == 0 || h.observationKey == ([32]byte{}) {
		return "", historyquery.ErrIntegrity
	}
	mac := hmac.New(sha256.New, h.observationKey[:])
	mac.Write([]byte("history-observation-v2"))
	mac.Write([]byte(text))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], sequence)
	mac.Write(number[:])
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (h *historyV2Handler) aggregateIdentity(record storage.RecorderHourlyAggregate) (string, error) {
	if h.observationKey == ([32]byte{}) || record.GroupDigest == ([32]byte{}) {
		return "", historyquery.ErrIntegrity
	}
	mac := hmac.New(sha256.New, h.observationKey[:])
	mac.Write([]byte("history-aggregate-v2"))
	mac.Write([]byte(record.Entity))
	mac.Write(record.GroupDigest[:])
	var at [8]byte
	binary.BigEndian.PutUint64(at[:], uint64(record.Start.Unix()))
	mac.Write(at[:])
	return hex.EncodeToString(mac.Sum(nil)), nil
}
