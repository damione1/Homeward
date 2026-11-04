package server

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"time"
	"unicode"
	"unicode/utf8"
)

func publicHistoryPeriod(start, end time.Time) *api.HistoryPeriod {
	return &api.HistoryPeriod{Start: timestamppb.New(start), End: timestamppb.New(end)}
}
func publicHistoryScope(q historyquery.Query, cutoff, asOf time.Time) *api.HistoryReadScope {
	start, end := q.Bounds()
	resolution := api.HistoryResolution_HISTORY_RESOLUTION_RAW
	maxPeriod := int32(168)
	maxPoints := int32(historyquery.MaxPoints)
	retainedStart := cutoff
	if q.Kind() == historyquery.Aggregates {
		resolution = api.HistoryResolution_HISTORY_RESOLUTION_HOUR
		maxPeriod = 500
		retainedStart = cutoff.Truncate(time.Hour)
	}
	if q.Kind() == historyquery.ExecutionTraces {
		maxPoints = historyquery.MaxTracePoints
	}
	scope := &api.HistoryReadScope{Requested: publicHistoryPeriod(start, end), RetentionCutoff: timestamppb.New(cutoff), AsOf: timestamppb.New(asOf), Resolution: resolution, Limits: &api.HistoryLimits{MaxPoints: maxPoints, MaxResponseBytes: historyquery.MaxResponseBytes, MaxTokenBytes: historyquery.MaxTokenBytes, QueryTimeoutMillis: 2000, MaxReaders: 2, MaxPeriodHours: maxPeriod}}
	if start.Before(retainedStart) {
		until := end
		if retainedStart.Before(until) {
			until = retainedStart
		}
		scope.NotRetainedPrefix = publicHistoryPeriod(start, until)
		start = retainedStart
	}
	if start.Before(end) {
		scope.Retained = publicHistoryPeriod(start, end)
	}
	return scope
}
func publicHistoryDescriptor(n recordermaintenance.NumericInput) (*api.HistoryNumericDescriptor, error) {
	if n.Precision < -308 || n.Precision > 308 || math.IsNaN(n.Scale) || math.IsInf(n.Scale, 0) || n.Scale <= 0 || math.IsNaN(n.Offset) || math.IsInf(n.Offset, 0) {
		return nil, historyquery.ErrIntegrity
	}
	for _, unit := range []string{n.Unit, n.CanonicalUnit} {
		if len(unit) == 0 || len(unit) > 256 || !utf8.ValidString(unit) {
			return nil, historyquery.ErrIntegrity
		}
		for _, r := range unit {
			if unicode.IsControl(r) || unicode.IsSpace(r) {
				return nil, historyquery.ErrIntegrity
			}
		}
	}

	out := &api.HistoryNumericDescriptor{Unit: n.Unit, CanonicalUnit: n.CanonicalUnit, Scale: n.Scale, Offset: n.Offset, Precision: int32(n.Precision)}
	switch n.Dimension {
	case domain.DimensionPower:
		out.Dimension = api.HistoryDimension_HISTORY_DIMENSION_POWER
	case domain.DimensionEnergy:
		out.Dimension = api.HistoryDimension_HISTORY_DIMENSION_ENERGY
	case domain.DimensionTemperature:
		out.Dimension = api.HistoryDimension_HISTORY_DIMENSION_TEMPERATURE
	case domain.DimensionRatio:
		out.Dimension = api.HistoryDimension_HISTORY_DIMENSION_RATIO
	case domain.DimensionIlluminance:
		out.Dimension = api.HistoryDimension_HISTORY_DIMENSION_ILLUMINANCE
	case domain.DimensionCount:
		out.Dimension = api.HistoryDimension_HISTORY_DIMENSION_COUNT
	default:
		return nil, historyquery.ErrIntegrity
	}
	switch n.Semantic {
	case domain.SemanticMeasurement:
		out.Semantic = api.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT
	case domain.SemanticTotal:
		out.Semantic = api.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL
	case domain.SemanticTotalIncreasing:
		out.Semantic = api.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING
	default:
		return nil, historyquery.ErrIntegrity
	}
	switch n.Aggregation {
	case domain.AggregationCurrent:
		out.Aggregation = api.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT
	case domain.AggregationSum:
		out.Aggregation = api.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM
	case domain.AggregationMean:
		out.Aggregation = api.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN
	default:
		return nil, historyquery.ErrIntegrity
	}
	return out, nil
}
func publicHistoryCoverage(started *time.Time) *api.HistoryCoverage {
	result := &api.HistoryCoverage{Verdict: api.HistoryCoverageVerdict_HISTORY_COVERAGE_VERDICT_NOT_PROVEN}
	if started != nil {
		result.ProvenanceStartedAt = timestamppb.New(started.UTC())
	}
	return result
}

func publicHistoryReset(value recordermaintenance.Reset) (api.HistoryReset, error) {
	switch value {
	case recordermaintenance.ResetNotApplicable:
		return api.HistoryReset_HISTORY_RESET_NOT_APPLICABLE, nil
	case recordermaintenance.ResetUnknown:
		return api.HistoryReset_HISTORY_RESET_UNKNOWN, nil
	case recordermaintenance.ResetNotDetected:
		return api.HistoryReset_HISTORY_RESET_NOT_DETECTED, nil
	case recordermaintenance.ResetInferred:
		return api.HistoryReset_HISTORY_RESET_INFERRED, nil
	default:
		return 0, historyquery.ErrIntegrity
	}
}
func publicHistoryReasons(value recordermaintenance.Reason) ([]api.HistoryAggregateReason, error) {
	if !value.Valid() {
		return nil, historyquery.ErrIntegrity
	}
	var out []api.HistoryAggregateReason
	if value&recordermaintenance.Gap != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_GAP)
	}
	if value&recordermaintenance.EpochBoundary != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_EPOCH_BOUNDARY)
	}
	if value&recordermaintenance.RetentionPruned != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_RETENTION_PRUNED)
	}
	if value&recordermaintenance.QualityBreak != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_QUALITY_BREAK)
	}
	if value&recordermaintenance.UnitBreak != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_UNIT_BREAK)
	}
	if value&recordermaintenance.ClockRegression != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_CLOCK_REGRESSION)
	}
	if value&recordermaintenance.CounterDip != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_COUNTER_DIP)
	}
	if value&recordermaintenance.UnknownBaseline != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_UNKNOWN_BASELINE)
	}
	if value&recordermaintenance.BudgetLimit != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_BUDGET_LIMIT)
	}
	if value&recordermaintenance.UnprovenTail != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_UNPROVEN_TAIL)
	}
	if value&recordermaintenance.NonNumeric != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_NON_NUMERIC)
	}
	if value&recordermaintenance.UnsupportedNumeric != 0 {
		out = append(out, api.HistoryAggregateReason_HISTORY_AGGREGATE_REASON_UNSUPPORTED_NUMERIC)
	}
	return out, nil
}
func publicHistoryQuality(a domain.Availability, f domain.Freshness, p domain.Provenance) (*api.ObservationQuality, error) {
	out := new(api.ObservationQuality)
	switch a {
	case domain.AvailabilityAvailable:
		out.Availability = api.Availability_AVAILABILITY_AVAILABLE
	case domain.AvailabilityUnknown:
		out.Availability = api.Availability_AVAILABILITY_UNKNOWN
	case domain.AvailabilityUnavailable:
		out.Availability = api.Availability_AVAILABILITY_UNAVAILABLE
	default:
		return nil, historyquery.ErrIntegrity
	}
	switch f {
	case domain.FreshnessFresh:
		out.Freshness = api.Freshness_FRESHNESS_CURRENT
	case domain.FreshnessStale:
		out.Freshness = api.Freshness_FRESHNESS_STALE
	default:
		return nil, historyquery.ErrIntegrity
	}
	switch p {
	case domain.ProvenanceObserved:
		out.Provenance = api.Provenance_PROVENANCE_OBSERVED
	case domain.ProvenanceRestored:
		out.Provenance = api.Provenance_PROVENANCE_RESTORED
	case domain.ProvenanceEstimated:
		out.Provenance = api.Provenance_PROVENANCE_ESTIMATED
	default:
		return nil, historyquery.ErrIntegrity
	}
	return out, nil
}
