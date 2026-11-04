package contracts

import (
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/proto"
)

// HistorySelection contains only admitted public selection. Resolving an entity
// remains the registry owner's job after the native authorization boundary.
type HistorySelection struct {
	kind                      historyquery.Kind
	home                      apitypes.HomeName
	entity                    apitypes.EntityName
	start, end                time.Time
	size                      int
	pageToken, executionEpoch string
}

func (s HistorySelection) Kind() historyquery.Kind             { return s.kind }
func (s HistorySelection) Home() apitypes.HomeName             { return s.home }
func (s HistorySelection) Entity() (apitypes.EntityName, bool) { return s.entity, s.entity.Valid() }
func (s HistorySelection) PageToken() string                   { return s.pageToken }
func (s HistorySelection) ExecutionEpoch() string              { return s.executionEpoch }

// NativeQuery accepts only the native identity resolved for the public parent.
// Execution epoch tokens must first be opened under their purpose/home binding;
// neither token contents nor public entity strings are passed through as IDs.
func (s HistorySelection) NativeQuery(entity domain.EntityID, epoch string) (historyquery.Query, error) {
	if !s.home.Valid() {
		return historyquery.Query{}, ErrInvalidHistoryContract
	}
	name := ""
	if s.kind != historyquery.ExecutionTraces {
		projected, err := apitypes.EntityNameForIdentity(s.home, entity)
		if err != nil || projected != s.entity || epoch != "" {
			return historyquery.Query{}, ErrInvalidHistoryContract
		}
		name, err = entity.Value()
		if err != nil {
			return historyquery.Query{}, ErrInvalidHistoryContract
		}
	} else if s.entity.Valid() {
		return historyquery.Query{}, ErrInvalidHistoryContract
	}
	return historyquery.NewQuery(s.kind, name, s.start, s.end, s.size, epoch)
}

func AdmitHistoryV2Selection(message proto.Message) (HistorySelection, error) {
	var out HistorySelection
	if observationNil(message) || proto.Size(message) > 8<<10 || !historyTree(message.ProtoReflect(), 0) {
		return out, ErrInvalidHistoryContract
	}
	var parent string
	var period *api.HistoryPeriod
	var size int32
	switch request := message.(type) {
	case *api.QueryObservationsRequest:
		out.kind = historyquery.Observations
		parent, period, size, out.pageToken = request.Parent, request.Period, request.PageSize, request.PageToken
	case *api.QueryAggregatesRequest:
		out.kind = historyquery.Aggregates
		parent, period, size, out.pageToken = request.Parent, request.Period, request.PageSize, request.PageToken
	case *api.QueryExecutionTracesRequest:
		out.kind = historyquery.ExecutionTraces
		parent, period, size, out.pageToken, out.executionEpoch = request.Parent, request.Period, request.PageSize, request.PageToken, request.ExecutionEpoch
	default:
		return HistorySelection{}, ErrInvalidHistoryContract
	}
	if size < 0 || len(out.pageToken) > historyquery.MaxTokenBytes || len(out.executionEpoch) > 512 || period == nil || period.Start == nil || period.End == nil || period.Start.CheckValid() != nil || period.End.CheckValid() != nil {
		return HistorySelection{}, ErrInvalidHistoryContract
	}
	out.start, out.end = period.Start.AsTime().UTC(), period.End.AsTime().UTC()
	maximum := historyquery.MaxPoints
	span := 168 * time.Hour
	if out.kind == historyquery.Aggregates {
		span = 500 * time.Hour
		if !out.start.Equal(out.start.Truncate(time.Hour)) || !out.end.Equal(out.end.Truncate(time.Hour)) {
			return HistorySelection{}, ErrInvalidHistoryContract
		}
	}
	if out.kind == historyquery.ExecutionTraces {
		maximum = historyquery.MaxTracePoints
	}
	if out.start.IsZero() || !out.start.Before(out.end) || out.end.Sub(out.start) > span {
		return HistorySelection{}, ErrInvalidHistoryContract
	}
	out.size = int(size)
	if out.size == 0 || out.size > maximum {
		out.size = maximum
	}
	var err error
	if out.kind == historyquery.ExecutionTraces {
		out.home, err = apitypes.ParseHomeName(parent)
	} else {
		out.entity, err = apitypes.ParseEntityName(parent)
		out.home = out.entity.Home()
	}
	if err != nil {
		return HistorySelection{}, ErrInvalidHistoryContract
	}
	return out, nil
}
