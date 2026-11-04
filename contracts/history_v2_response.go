package contracts

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"strconv"
	"strings"
	"time"
	"unicode"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/proto"
)

func historyV2Period(p *api.HistoryPeriod) bool {
	return p != nil && p.Start != nil && p.End != nil && p.Start.CheckValid() == nil && p.End.CheckValid() == nil && !p.Start.AsTime().IsZero() && p.Start.AsTime().Before(p.End.AsTime())
}
func historyV2Quality(q *api.ObservationQuality) bool {
	return q != nil && q.Availability != 0 && q.Freshness != 0 && q.Provenance != 0 && historyTree(q.ProtoReflect(), 0)
}
func historyV2Descriptor(d *api.HistoryNumericDescriptor) bool {
	if d == nil || !historyTree(d.ProtoReflect(), 0) || d.Scale <= 0 || d.Precision < -308 || d.Precision > 308 || d.Dimension == 0 || d.Semantic == 0 || d.Aggregation == 0 {
		return false
	}
	for _, unit := range []string{d.Unit, d.CanonicalUnit} {
		if unit == "" || len(unit) > historyquery.MaxPointBytes {
			return false
		}
		for _, r := range unit {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}
func historyV2Position(p *api.AdmissionPosition) bool { return p != nil && observationID(p.Epoch, 16) }

// AdmitRecorderEvidence validates the same finite native proof in both History
// and Observability. Sequence widths never stand in for unknown lost counts.
func AdmitRecorderEvidence(v *api.RecorderCoverageSnapshot) error {
	if v == nil || !observationV2Tree(v.ProtoReflect(), 0) {
		return ErrInvalidHistoryContract
	}
	return nil
}

func historyV2Coverage(v *api.HistoryCoverage) bool {
	return v != nil && historyTree(v.ProtoReflect(), 0) && v.Verdict == api.HistoryCoverageVerdict_HISTORY_COVERAGE_VERDICT_NOT_PROVEN && (v.ProvenanceStartedAt == nil || v.ProvenanceStartedAt.CheckValid() == nil)
}
func historyObservationID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func historicalPublicState(v *api.HistoricalState) bool {
	if v == nil || !historyTree(v.ProtoReflect(), 0) || !historyV2Quality(v.Quality) || v.ReceiveTime == nil || v.LastChangeTime == nil || v.LastUpdateTime == nil || v.LastReportTime == nil || (v.Quality.Availability == api.Availability_AVAILABILITY_AVAILABLE) != (v.Value != nil) {
		return false
	}
	if m := v.GetMeasurement(); m != nil && m.Unit == "" {
		return false
	}
	if light := v.GetLight(); light != nil && (light.On == nil || light.ColorTempMireds != nil && (*light.ColorTempMireds <= 0 || *light.ColorTempMireds > 4294967295) || light.BrightnessPercent != nil && (*light.BrightnessPercent < 0 || *light.BrightnessPercent > 100)) {
		return false
	}
	return true
}
func AdmitHistoryV2Response(message proto.Message, home apitypes.HomeName) error {
	invalid := ErrInvalidHistoryContract
	if !home.Valid() || observationNil(message) || !historyTree(message.ProtoReflect(), 0) {
		return invalid
	}
	var scope *api.HistoryReadScope
	var coverage *api.HistoryCoverage
	var token string
	var count int
	resolution := api.HistoryResolution_HISTORY_RESOLUTION_RAW
	maxPoints, maxHours := int32(historyquery.MaxPoints), int32(168)
	traces := false
	switch response := message.(type) {
	case *api.QueryObservationsResponse:
		scope, coverage, token, count = response.Scope, response.Coverage, response.NextPageToken, len(response.Observations)
		for _, point := range response.Observations {
			if point == nil || !historyObservationID(point.ObservationId) || !historicalPublicState(point.State) || (point.State.GetMeasurement() != nil) != (point.Descriptor_ != nil) || point.Descriptor_ != nil && !historyV2Descriptor(point.Descriptor_) {
				return invalid
			}
		}
	case *api.QueryAggregatesResponse:
		scope, coverage, token, count = response.Scope, response.Coverage, response.NextPageToken, len(response.Aggregates)
		resolution, maxHours = api.HistoryResolution_HISTORY_RESOLUTION_HOUR, 500
		for _, point := range response.Aggregates {
			if point == nil || !historyObservationID(point.AggregateId) || !historyV2Period(point.Bucket) || !point.Bucket.Start.AsTime().Equal(point.Bucket.Start.AsTime().Truncate(time.Hour)) || point.Bucket.End.AsTime().Sub(point.Bucket.Start.AsTime()) != time.Hour || !historyV2Descriptor(point.Descriptor_) || !historyV2Quality(point.Quality) || point.Reset_ == 0 || point.DurationNanos < 0 || point.DurationNanos > int64(time.Hour) || (point.Minimum == nil) != (point.Maximum == nil) || (point.Mean == nil) != (point.DurationNanos == 0) {
				return invalid
			}
		}
	case *api.QueryExecutionTracesResponse:
		if _, err := apitypes.ParseDecimalUint64(response.Checkpoint); err != nil {
			return invalid
		}
		if _, err := apitypes.ParseDecimalUint64(response.CommitHigh); err != nil {
			return invalid
		}
		scope, token, count = response.Scope, response.NextPageToken, len(response.ExecutionTraces)
		traces, maxPoints = true, historyquery.MaxTracePoints
		if len(response.ExecutionEpoch) > 512 || len(response.PreviousExecutionEpoch) > 512 || (response.Scope != nil && response.Scope.Retained != nil && response.ExecutionEpoch == "") || len(response.Losses) > 4 {
			return invalid
		}
		for _, loss := range response.Losses {
			if loss == nil || loss.Reason == api.HistoryTraceLossReason_HISTORY_TRACE_LOSS_REASON_UNSPECIFIED || !historyPositiveDecimal(loss.KnownCount) || loss.From == nil || loss.Through == nil || loss.From.CheckValid() != nil || loss.Through.CheckValid() != nil || loss.From.AsTime().After(loss.Through.AsTime()) {
				return invalid
			}
		}
		for _, point := range response.ExecutionTraces {
			if point == nil || !historyPositiveDecimal(point.Ordinal) || point.EmissionTime == nil || point.Outcome == nil {
				return invalid
			}
			for _, reference := range []*string{point.Run, point.ParentRun} {
				if reference != nil {
					if _, err := apitypes.ParseHistoryRunName(*reference); err != nil || !strings.HasPrefix(*reference, home.String()+"/executionEpochs/") {
						return invalid
					}
				}
			}
			if point.Controller != nil {
				if _, err := apitypes.ParseHistoryControllerName(*point.Controller); err != nil || !strings.HasPrefix(*point.Controller, home.String()+"/executionEpochs/") {
					return invalid
				}
			}
			if (point.ConfigurationGeneration == nil) != (len(point.ManifestSha256) == 0) {
				return invalid
			}
			if point.ConfigurationGeneration != nil {
				suffix, ok := strings.CutPrefix(*point.ConfigurationGeneration, home.String()+"/configurationGenerations/g-")
				revision, err := strconv.ParseInt(suffix, 10, 64)
				if !ok || err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != suffix || !configurationHash(point.ManifestSha256) {
					return invalid
				}
			}
		}
	default:
		return invalid
	}
	if scope == nil || !historyV2Period(scope.Requested) || scope.RetentionCutoff == nil || scope.AsOf == nil || scope.Limits == nil || scope.Resolution != resolution || len(token) > historyquery.MaxTokenBytes || count > int(maxPoints) {
		return invalid
	}
	limits := scope.Limits
	if limits.MaxPoints != maxPoints || limits.MaxPeriodHours != maxHours || limits.MaxResponseBytes != historyquery.MaxResponseBytes || limits.MaxTokenBytes != historyquery.MaxTokenBytes || limits.QueryTimeoutMillis != 2000 || limits.MaxReaders != 2 {
		return invalid
	}
	if scope.NotRetainedPrefix != nil && !historyV2Period(scope.NotRetainedPrefix) {
		return invalid
	}
	if scope.Retained == nil {
		if count != 0 || token != "" || coverage != nil || scope.NotRetainedPrefix == nil {
			return invalid
		}
	} else if !historyV2Period(scope.Retained) || !traces && !historyV2Coverage(coverage) {
		return invalid
	}
	return nil
}

func historyPositiveDecimal(text string) bool {
	n, err := apitypes.ParseDecimalUint64(text)
	return err == nil && n.Value() > 0
}
