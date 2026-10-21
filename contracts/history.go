package contracts

import (
	"bytes"
	"errors"
	"math"
	"time"
	"unicode/utf8"

	history "github.com/damione1/Homeward/gen/home/history/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"unicode"
)

var ErrInvalidHistoryContract = errors.New("history: invalid contract")

func historyPeriod(p *history.Period) (time.Time, time.Time, error) {
	if p == nil || p.Start == nil || p.End == nil || p.Start.CheckValid() != nil || p.End.CheckValid() != nil || p.Start.AsTime().IsZero() || !p.Start.AsTime().Before(p.End.AsTime()) {
		return time.Time{}, time.Time{}, ErrInvalidHistoryContract
	}
	return p.Start.AsTime().UTC(), p.End.AsTime().UTC(), nil
}
func AdmitHistoryRequest(m proto.Message) (historyquery.Query, string, error) {
	if observationNil(m) || !historyTree(m.ProtoReflect(), 0) {
		return historyquery.Query{}, "", ErrInvalidHistoryContract
	}
	var kind historyquery.Kind
	var entity, epoch, token string
	var period *history.Period
	var size *uint32
	switch v := m.(type) {
	case *history.QueryObservationsRequest:
		if v.Resolution != history.Resolution_RESOLUTION_RAW {
			return historyquery.Query{}, "", ErrInvalidHistoryContract
		}
		kind = historyquery.Observations
		entity = v.EntityId
		period = v.Period
		size = v.PageSize
		token = v.PageToken
	case *history.QueryAggregatesRequest:
		if v.Resolution != history.Resolution_RESOLUTION_HOUR {
			return historyquery.Query{}, "", ErrInvalidHistoryContract
		}
		kind = historyquery.Aggregates
		entity = v.EntityId
		period = v.Period
		size = v.PageSize
		token = v.PageToken
	case *history.QueryExecutionTracesRequest:
		kind = historyquery.ExecutionTraces
		period = v.Period
		size = v.PageSize
		token = v.PageToken
		if v.Epoch != nil {
			if *v.Epoch == "" {
				return historyquery.Query{}, "", ErrInvalidHistoryContract
			}
			epoch = *v.Epoch
		}
	default:
		return historyquery.Query{}, "", ErrInvalidHistoryContract
	}
	start, end, err := historyPeriod(period)
	if err != nil {
		return historyquery.Query{}, "", err
	}
	limit := 0
	if size != nil {
		if *size == 0 || *size > historyquery.MaxPoints {
			return historyquery.Query{}, "", ErrInvalidHistoryContract
		}
		limit = int(*size)
	}
	if len(token) > historyquery.MaxTokenBytes {
		return historyquery.Query{}, "", ErrInvalidHistoryContract
	}
	q, err := historyquery.NewQuery(kind, entity, start, end, limit, epoch)
	if err != nil {
		return historyquery.Query{}, "", ErrInvalidHistoryContract
	}
	return q, token, nil
}

// historyTree bounds nesting/cardinality, rejects typed nils, unknown fields and
// enum variants. Operational presence/relationships are checked separately.
func historyTree(m protoreflect.Message, depth int) bool {
	if !m.IsValid() || depth > 16 || len(m.GetUnknown()) != 0 || observationNilOneof(m.Interface()) {
		return false
	}
	valid := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.IsMap() {
			valid = false
			return false
		}
		check := func(v protoreflect.Value) bool {
			switch f.Kind() {
			case protoreflect.MessageKind:
				return historyTree(v.Message(), depth+1)
			case protoreflect.EnumKind:
				return v.Enum() > 0 && f.Enum().Values().ByNumber(v.Enum()) != nil
			case protoreflect.StringKind:
				return len(v.String()) <= historyquery.MaxPointBytes && utf8.ValidString(v.String())
			case protoreflect.BytesKind:
				return len(v.Bytes()) <= historyquery.MaxPointBytes
			case protoreflect.DoubleKind:
				return !math.IsNaN(v.Float()) && !math.IsInf(v.Float(), 0)
			case protoreflect.FloatKind:
				return !math.IsNaN(v.Float()) && !math.IsInf(v.Float(), 0)
			default:
				return true
			}
		}
		if f.IsList() {
			list := v.List()
			maximum := 100
			switch f.Name() {
			case "epochs":
				maximum = 64
			case "gaps", "retention":
				maximum = 512
			case "reasons":
				maximum = 12
			case "losses":
				maximum = 4
			}
			if list.Len() > maximum {
				valid = false
				return false
			}
			for i := 0; i < list.Len(); i++ {
				if !check(list.Get(i)) {
					valid = false
					return false
				}
			}
			return true
		}
		valid = check(v)
		return valid
	})
	if !valid {
		return false
	}
	if t, ok := m.Interface().(*timestamppb.Timestamp); ok {
		return t.CheckValid() == nil && !t.AsTime().IsZero()
	}
	return true
}

// AdmitHistoricalState checks the persisted value without applying today's
// catalogue constraints. Historical measurement interpretation is separate.
func AdmitHistoricalState(v *state.ObservedState) error {
	if v == nil || !historyTree(v.ProtoReflect(), 0) || v.Entity == nil || v.Entity.GetEntityId() == "" || v.Revision == 0 {
		return ErrInvalidHistoryContract
	}
	if _, err := domain.ParseEntityID(v.Entity.GetEntityId()); err != nil {
		return ErrInvalidHistoryContract
	}
	if _, err := admitQuality(v.Quality); err != nil {
		return ErrInvalidHistoryContract
	}
	if (v.Quality.Availability == state.Availability_AVAILABILITY_AVAILABLE) != (v.Value != nil) {
		return ErrInvalidHistoryContract
	}
	for _, t := range []*timestamppb.Timestamp{v.ReceivedAt, v.LastChanged, v.LastUpdated, v.LastReported} {
		if t == nil || t.CheckValid() != nil || t.AsTime().IsZero() {
			return ErrInvalidHistoryContract
		}
	}
	if _, err := domain.NewSourceStamp(v.SourceSession, v.SourceSeq); err != nil {
		return ErrInvalidHistoryContract
	}
	switch x := v.Value.(type) {
	case nil:
	case *state.ObservedState_SwitchOn:
		if x == nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Measurement:
		if x == nil || x.Measurement == nil || x.Measurement.Unit == "" {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Light:
		if x == nil || x.Light == nil || x.Light.On == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := domain.NewLightValue(*x.Light.On, x.Light.BrightnessPercent, x.Light.ColorTempMireds); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Fan:
		if x == nil || x.Fan == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireFanSpeed(x.Fan.Speed); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Climate:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireClimateValue(x.Climate); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Helper:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, _, err := admitHelperWire(x.Helper, true); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_Solar:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireSolarValue(x.Solar); err != nil {
			return ErrInvalidHistoryContract
		}
	case *state.ObservedState_SolarEvent:
		if x == nil {
			return ErrInvalidHistoryContract
		}
		if _, err := AdmitWireSolarEventValue(x.SolarEvent); err != nil {
			return ErrInvalidHistoryContract
		}
	default:
		return ErrInvalidHistoryContract
	}
	return nil
}
func AdmitHistoryResponse(m proto.Message) error {
	if observationNil(m) || !historyTree(m.ProtoReflect(), 0) || proto.Size(m) > historyquery.MaxResponseBytes {
		return ErrInvalidHistoryContract
	}
	var scope *history.ReadScope
	var continuation *history.Continuation
	var coverage *history.Coverage
	needCoverage := false
	needContinuation := false
	switch v := m.(type) {
	case *history.QueryObservationsResponse:
		switch r := v.Result.(type) {
		case *history.QueryObservationsResponse_Page:
			if r == nil || r.Page == nil {
				return ErrInvalidHistoryContract
			}
			needCoverage = true
			needContinuation = true
			scope = r.Page.Scope
			continuation = r.Page.Continuation
			coverage = r.Page.Coverage
			for _, point := range r.Page.Points {
				if point == nil || point.Position == nil || point.Position.Sequence == 0 || len(point.Position.Epoch) != 16 || AdmitHistoricalState(point.State) != nil {
					return ErrInvalidHistoryContract
				}
				if (point.State.GetMeasurement() != nil) != (point.Descriptor_ != nil) || (point.Descriptor_ != nil && !historyDescriptorValid(point.Descriptor_)) {
					return ErrInvalidHistoryContract
				}
			}
		case *history.QueryObservationsResponse_NotRetained:
			scope = r.NotRetained
		case *history.QueryObservationsResponse_StaleCursor:
			if r == nil || r.StaleCursor == nil {
				return ErrInvalidHistoryContract
			}
			return nil
		case *history.QueryObservationsResponse_Budget:
			if r == nil || r.Budget == nil || r.Budget.Reason == 0 {
				return ErrInvalidHistoryContract
			}
			return nil
		default:
			return ErrInvalidHistoryContract
		}
	case *history.QueryAggregatesResponse:
		switch r := v.Result.(type) {
		case *history.QueryAggregatesResponse_Page:
			if r == nil || r.Page == nil {
				return ErrInvalidHistoryContract
			}
			needCoverage = true
			needContinuation = true
			scope = r.Page.Scope
			continuation = r.Page.Continuation
			coverage = r.Page.Coverage
			for _, point := range r.Page.Points {
				if point == nil || !historyDescriptorValid(point.Descriptor_) || point.Quality == nil || point.Reset_ == 0 || point.Bucket == nil || point.DurationNanos > uint64(time.Hour) || ((point.Minimum == nil) != (point.Maximum == nil)) || ((point.Mean == nil) != (point.DurationNanos == 0)) {
					return ErrInvalidHistoryContract
				}
			}
		case *history.QueryAggregatesResponse_NotRetained:
			scope = r.NotRetained
		case *history.QueryAggregatesResponse_StaleCursor:
			if r == nil || r.StaleCursor == nil {
				return ErrInvalidHistoryContract
			}
			return nil
		case *history.QueryAggregatesResponse_Budget:
			if r == nil || r.Budget == nil || r.Budget.Reason == 0 {
				return ErrInvalidHistoryContract
			}
			return nil
		default:
			return ErrInvalidHistoryContract
		}
	case *history.QueryExecutionTracesResponse:
		switch r := v.Result.(type) {
		case *history.QueryExecutionTracesResponse_Page:
			if r == nil || r.Page == nil || r.Page.Epoch == "" || len(r.Page.Points) > historyquery.MaxTracePoints {
				return ErrInvalidHistoryContract
			}
			needContinuation = true
			scope = r.Page.Scope
			continuation = r.Page.Continuation
			for _, point := range r.Page.Points {
				if point == nil || point.Ordinal == 0 || point.EmissionTime == nil || point.Outcome == nil {
					return ErrInvalidHistoryContract
				}
			}
		case *history.QueryExecutionTracesResponse_NotRetained:
			scope = r.NotRetained
		case *history.QueryExecutionTracesResponse_StaleCursor:
			if r == nil || r.StaleCursor == nil {
				return ErrInvalidHistoryContract
			}
			return nil
		case *history.QueryExecutionTracesResponse_Budget:
			if r == nil || r.Budget == nil || r.Budget.Reason == 0 {
				return ErrInvalidHistoryContract
			}
			return nil
		default:
			return ErrInvalidHistoryContract
		}
	default:
		return ErrInvalidHistoryContract
	}
	if scope == nil || scope.Requested == nil || scope.RetentionCutoff == nil || scope.AsOf == nil || scope.Limits == nil || scope.Resolution == 0 {
		return ErrInvalidHistoryContract
	}
	if scope.Limits.MaxResponseBytes != historyquery.MaxResponseBytes || scope.Limits.MaxTokenBytes != historyquery.MaxTokenBytes || scope.Limits.QueryTimeoutMillis != 2000 || scope.Limits.MaxReaders != 2 || (scope.Limits.MaxPoints != 100 && scope.Limits.MaxPoints != 31) || (scope.Limits.MaxPeriodHours != 168 && scope.Limits.MaxPeriodHours != 500) {
		return ErrInvalidHistoryContract
	}
	if _, _, err := historyPeriod(scope.Requested); err != nil {
		return err
	}
	if scope.Retained != nil {
		if _, _, err := historyPeriod(scope.Retained); err != nil {
			return err
		}
	}
	if scope.NotRetainedPrefix != nil {
		if _, _, err := historyPeriod(scope.NotRetainedPrefix); err != nil {
			return err
		}
	}
	if needContinuation && continuation == nil {
		return ErrInvalidHistoryContract
	}
	if needCoverage && coverage == nil {
		return ErrInvalidHistoryContract
	}
	if continuation != nil {
		switch c := continuation.Result.(type) {
		case *history.Continuation_End:
			if c == nil || c.End == nil {
				return ErrInvalidHistoryContract
			}
		case *history.Continuation_NextPageToken:
			if c == nil || len(c.NextPageToken) == 0 || len(c.NextPageToken) > historyquery.MaxTokenBytes {
				return ErrInvalidHistoryContract
			}
		default:
			return ErrInvalidHistoryContract
		}
	}
	if coverage != nil && coverage.RecorderTail != nil {
		tail := coverage.RecorderTail
		if tail.From == nil || tail.Through == nil || tail.UnknownUtc == nil || !observationID(tail.From.Epoch, 16) || coverage.Logical == nil || coverage.Logical.Through == nil || !proto.Equal(coverage.Logical.Through, tail.From) || !bytes.Equal(tail.From.Epoch, tail.Through.Epoch) || tail.Through.Sequence <= tail.From.Sequence || coverage.Verdict != history.CoverageVerdict_COVERAGE_VERDICT_INCOMPLETE {
			return ErrInvalidHistoryContract
		}
	}
	if coverage != nil && (coverage.Logical == nil || coverage.Verdict == 0 || (coverage.AnalyticalTailUnproven && coverage.Verdict == history.CoverageVerdict_COVERAGE_VERDICT_COMPLETE_UNDER_POLICY) || AdmitObservabilityResponse(&obs.ReadRecorderCoverageResponse{Result: &obs.ReadRecorderCoverageResponse_Complete{Complete: coverage.Logical}}) != nil) {
		return ErrInvalidHistoryContract
	}
	return nil
}

// AdmitHistoryNumericDescriptor admits a persisted interpretation, independent
// of a current unit catalogue or the narrower analytical representation of032.
func AdmitHistoryNumericDescriptor(d *history.NumericDescriptor) error {
	if !historyDescriptorValid(d) {
		return ErrInvalidHistoryContract
	}
	return nil
}
func historyDescriptorValid(d *history.NumericDescriptor) bool {
	if d == nil || !historyTree(d.ProtoReflect(), 0) || d.Scale <= 0 || d.Precision < -308 || d.Precision > 308 {
		return false
	}
	for _, symbol := range []string{d.Unit, d.CanonicalUnit} {
		if symbol == "" || len(symbol) > historyquery.MaxPointBytes {
			return false
		}
		for _, r := range symbol {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return false
			}
		}
	}
	switch d.Dimension {
	case history.Dimension_DIMENSION_POWER, history.Dimension_DIMENSION_ENERGY, history.Dimension_DIMENSION_TEMPERATURE, history.Dimension_DIMENSION_RATIO, history.Dimension_DIMENSION_ILLUMINANCE, history.Dimension_DIMENSION_COUNT:
	default:
		return false
	}
	switch d.Semantic {
	case registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING:
	default:
		return false
	}
	switch d.Aggregation {
	case registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT, registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM, registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN:
	default:
		return false
	}
	return true
}
