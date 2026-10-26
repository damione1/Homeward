package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

// No raw protobuf is read by the analytical path. All historical numeric
// interpretation was captured atomically by the original Recorder writer.
// Oversized historical metadata is classified in SQL before driver allocation.
// Its exact digest is verified against the stored identity without copying the
// large chains into the bounded owner. The raw payload is never selected.
const recorderNumericColumns = `h.epoch,h.sequence::text,e.run_id,CASE WHEN octet_length(h.entity_id)<=1024 THEN h.entity_id ELSE '' END,h.entity_digest,h.admitted_unix_seconds,h.admitted_nanos,h.availability,h.freshness,h.provenance,h.value_kind,h.measurement_value,CASE WHEN octet_length(h.measurement_unit)<=256 THEN h.measurement_unit ELSE NULL END,CASE WHEN octet_length(h.measurement_canonical_unit)<=256 THEN h.measurement_canonical_unit ELSE NULL END,h.measurement_scale,h.measurement_offset,h.measurement_dimension,h.measurement_precision,h.measurement_semantic,h.measurement_aggregation,(octet_length(h.entity_id)>1024 OR COALESCE(octet_length(h.measurement_unit)>256,false) OR COALESCE(octet_length(h.measurement_canonical_unit)>256,false)),h.entity_digest=sha256(convert_to(h.entity_id,'UTF8'))`

type numericRow struct {
	epoch    RecorderEpoch
	sequence uint64
	runID    int64
	entity   string
	digest   []byte
	point    recordermaintenance.Point
	refused  bool
}

// Bound both the identity bytes PostgreSQL must hash and the bounded metadata
// returned to Go. octet_length reads a TOAST size without materializing text.
// This selection runs before the descriptor/digest projection, also for purge.
func numericSourceCharge(entityBytes, unitBytes, canonicalBytes int64) (int, error) {
	if entityBytes <= 0 || unitBytes < 0 || canonicalBytes < 0 {
		return 0, recordermaintenance.ErrIntegrity
	}
	charge := int64(1024) + entityBytes + min(entityBytes, 1024) + min(unitBytes, 256) + min(canonicalBytes, 256)
	if charge > recordermaintenance.MaxInputBytes {
		return 0, recordermaintenance.ErrBudget
	}
	return int(charge), nil
}

func scanNumericRow(row pgx.Row) (numericRow, error) {
	var out numericRow
	var raw []byte
	var seq string
	var seconds int64
	var nanos int32
	var availability, freshness, provenance int16
	var kind, dimension, precision, semantic, aggregation *int16
	var value, scale, offset *float64
	var unit, canonical *string
	var digestMatches bool
	if err := row.Scan(&raw, &seq, &out.runID, &out.entity, &out.digest, &seconds, &nanos, &availability, &freshness, &provenance, &kind, &value, &unit, &canonical, &scale, &offset, &dimension, &precision, &semantic, &aggregation, &out.refused, &digestMatches); err != nil {
		return out, err
	}
	var err error
	if out.epoch, err = coverageEpoch(raw); err != nil {
		return out, err
	}
	if out.sequence, err = coverageUint(seq); err != nil || out.sequence == 0 {
		return out, recordermaintenance.ErrIntegrity
	}
	if out.runID < 1 || !out.refused && (len(out.entity) == 0 || !bytes.Equal(out.digest, hashEntity(out.entity))) || !digestMatches || len(out.digest) != 32 || nanos < 0 || nanos > 999999999 {
		return out, recordermaintenance.ErrIntegrity
	}
	epoch, _ := out.epoch.Hex()
	in := recordermaintenance.PointInput{Epoch: epoch, Sequence: out.sequence, At: time.Unix(seconds, int64(nanos)).UTC()}
	switch availability {
	case 1:
		in.Availability = domain.AvailabilityAvailable
	case 2:
		in.Availability = domain.AvailabilityUnknown
	case 3:
		in.Availability = domain.AvailabilityUnavailable
	default:
		return out, recordermaintenance.ErrIntegrity
	}
	switch freshness {
	case 1:
		in.Freshness = domain.FreshnessFresh
	case 2:
		in.Freshness = domain.FreshnessStale
	default:
		return out, recordermaintenance.ErrIntegrity
	}
	switch provenance {
	case 1:
		in.Provenance = domain.ProvenanceObserved
	case 2:
		in.Provenance = domain.ProvenanceRestored
	case 3:
		in.Provenance = domain.ProvenanceEstimated
	default:
		return out, recordermaintenance.ErrIntegrity
	}
	if kind != nil {
		switch *kind {
		case 1:
			in.Kind = domain.ValueSwitch
		case 2:
			in.Kind = domain.ValueMeasurement
		case 3:
			in.Kind = domain.ValueLight
		case 4:
			in.Kind = domain.ValueFan
		case 5:
			in.Kind = domain.ValueClimate
		case 6:
			in.Kind = domain.ValueHelper
		case 7:
			in.Kind = domain.ValueSolar
		case 8:
			in.Kind = domain.ValueSolarEvent
		case 9:
			in.Kind = domain.ValueCover
		case 10:
			in.Kind = domain.ValueLock
		case 11:
			in.Kind = domain.ValueAlarm
		default:
			return out, recordermaintenance.ErrIntegrity
		}
	}
	if out.refused {
		out.entity = ""
		in.Kind = domain.ValueUnspecified
		out.point, err = recordermaintenance.NewPoint(in)
		if err != nil {
			return out, recordermaintenance.ErrIntegrity
		}
		return out, nil
	}
	if in.Kind == domain.ValueMeasurement {
		if value == nil || unit == nil || canonical == nil || scale == nil || offset == nil || dimension == nil || precision == nil || semantic == nil || aggregation == nil {
			return out, recordermaintenance.ErrIntegrity
		}
		n := recordermaintenance.NumericInput{Value: *value, Unit: *unit, CanonicalUnit: *canonical, Scale: *scale, Offset: *offset, Precision: int(*precision)}
		switch *dimension {
		case 1:
			n.Dimension = domain.DimensionPower
		case 2:
			n.Dimension = domain.DimensionEnergy
		case 3:
			n.Dimension = domain.DimensionTemperature
		case 4:
			n.Dimension = domain.DimensionRatio
		case 5:
			n.Dimension = domain.DimensionIlluminance
		case 6:
			n.Dimension = domain.DimensionCount
		default:
			return out, recordermaintenance.ErrIntegrity
		}
		switch *semantic {
		case 1:
			n.Semantic = domain.SemanticMeasurement
		case 2:
			n.Semantic = domain.SemanticTotal
		case 3:
			n.Semantic = domain.SemanticTotalIncreasing
		default:
			return out, recordermaintenance.ErrIntegrity
		}
		switch *aggregation {
		case 1:
			n.Aggregation = domain.AggregationCurrent
		case 2:
			n.Aggregation = domain.AggregationSum
		case 3:
			n.Aggregation = domain.AggregationMean
		default:
			return out, recordermaintenance.ErrIntegrity
		}
		in.Numeric = &n
		out.refused = out.refused || len(n.Unit) > 256 || len(n.CanonicalUnit) > 256
		if !out.refused {
			descriptor := n
			descriptor.Value = 0
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				return out, recordermaintenance.ErrIntegrity
			}
			out.refused = len(encoded) > 2048
		}
	} else if value != nil || unit != nil || canonical != nil || scale != nil || offset != nil || dimension != nil || precision != nil || semantic != nil || aggregation != nil {
		return out, recordermaintenance.ErrIntegrity
	}
	if out.refused {
		// Retain a bounded logical/temporal continuation, never invent a short
		// identity or descriptor. The original payload stays in Recorder until
		// its ordinary purge commits the logical retention proof.
		out.entity = ""
		in.Kind, in.Numeric = domain.ValueUnspecified, nil
	}
	out.point, err = recordermaintenance.NewPoint(in)
	if err != nil {
		return out, recordermaintenance.ErrIntegrity
	}
	return out, nil
}
func hashEntity(entity string) []byte { h := sha256.Sum256([]byte(entity)); return h[:] }
func decodePoint(data []byte) (recordermaintenance.Point, error) {
	if len(data) == 0 || len(data) > 4096 {
		return recordermaintenance.Point{}, recordermaintenance.ErrIntegrity
	}
	var in recordermaintenance.PointInput
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return recordermaintenance.Point{}, recordermaintenance.ErrIntegrity
	}
	p, err := recordermaintenance.NewPoint(in)
	if err != nil {
		return recordermaintenance.Point{}, recordermaintenance.ErrIntegrity
	}
	return p, nil
}

type numericContinuation struct {
	point                     recordermaintenance.Point
	pending, refused          bool
	highWater, coveredThrough time.Time
}

func loadContinuation(ctx context.Context, tx pgx.Tx, row numericRow) (*numericContinuation, error) {
	var entity *string
	var data []byte
	var out numericContinuation
	var highSeconds int64
	var highNanos int32
	err := tx.QueryRow(ctx, `SELECT entity_id,point,baseline_pending,high_water_seconds,high_water_nanos,covered_through,analytical_refusal FROM public.recorder_numeric_continuations WHERE entity_digest=$1`, row.digest).Scan(&entity, &data, &out.pending, &highSeconds, &highNanos, &out.coveredThrough, &out.refused)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.coveredThrough = out.coveredThrough.UTC()
	if out.refused && (entity != nil || out.pending) || !out.refused && (entity == nil || !row.refused && *entity != row.entity) {
		return nil, recordermaintenance.ErrIntegrity
	}
	out.highWater = time.Unix(highSeconds, int64(highNanos)).UTC()
	if highNanos < 0 || highNanos > 999999999 {
		return nil, recordermaintenance.ErrIntegrity
	}
	out.point, err = decodePoint(data)
	if err == nil && (out.highWater.Before(out.point.Input().At) || out.refused && (out.point.Input().Kind != domain.ValueUnspecified || out.point.Input().Numeric != nil)) {
		err = recordermaintenance.ErrIntegrity
	}
	return &out, err
}
func saveContinuation(ctx context.Context, tx pgx.Tx, row numericRow, pending bool, highWater, coveredThrough time.Time) error {
	in := row.point.Input()
	data, err := json.Marshal(in)
	if err != nil || len(data) > 4096 {
		return recordermaintenance.ErrBudget
	}
	if highWater.Before(in.At) {
		highWater = in.At
	}
	if coveredThrough.Before(in.At) {
		coveredThrough = in.At
	}
	var entity any = row.entity
	if row.refused {
		entity = nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO public.recorder_numeric_continuations(entity_digest,entity_id,point,baseline_pending,last_observation_at,high_water_seconds,high_water_nanos,covered_through,analytical_refusal) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(entity_digest) DO UPDATE SET entity_id=EXCLUDED.entity_id,point=EXCLUDED.point,baseline_pending=EXCLUDED.baseline_pending,last_observation_at=GREATEST(recorder_numeric_continuations.last_observation_at,EXCLUDED.last_observation_at),high_water_seconds=EXCLUDED.high_water_seconds,high_water_nanos=EXCLUDED.high_water_nanos,covered_through=EXCLUDED.covered_through,analytical_refusal=EXCLUDED.analytical_refusal WHERE recorder_numeric_continuations.entity_id=EXCLUDED.entity_id OR recorder_numeric_continuations.analytical_refusal OR EXCLUDED.analytical_refusal`, row.digest, entity, data, pending, in.At, highWater.Unix(), highWater.Nanosecond(), coveredThrough, row.refused)
	if err == nil && tag.RowsAffected() != 1 {
		return recordermaintenance.ErrIntegrity
	}
	return err
}
