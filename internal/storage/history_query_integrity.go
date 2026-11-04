package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"github.com/damione1/Homeward/internal/recorderevidence"
	"time"

	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

// Payload timestamps retain original nanoseconds; SQL indexes retain admitted
// PostgreSQL microseconds. The record digest must use the former, including the
// separate admitted seconds/nanos, rather than inventing rounded source times.
func verifyHistoricalRecord(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, sequence uint64, wire *state.ObservedState, payload []byte) (*recordermaintenance.NumericInput, error) {
	var row RecorderRecord
	var o recorderObserved
	var storedDigest, evidence []byte
	var revision string
	var sourceSeq *string
	var source string
	var observed *time.Time
	var received, changed, updated, reported time.Time
	var seconds int64
	var nanos int32
	err := tx.QueryRow(ctx, `SELECT instance_id,admitted_unix_seconds,admitted_nanos,causation_id,correlation_id,record_digest,revision::text,source_session,source_sequence::text,observed_at,received_at,last_changed,last_updated,last_reported,availability,freshness,provenance,value_kind,measurement_value,measurement_unit,measurement_canonical_unit,measurement_scale,measurement_offset,measurement_dimension,measurement_precision,measurement_semantic,measurement_aggregation,admission_evidence FROM public.state_history WHERE epoch=$1 AND sequence=$2::numeric`, epoch.data(), decimalUint(sequence)).Scan(&row.instance, &seconds, &nanos, &row.causation, &row.correlation, &storedDigest, &revision, &source, &sourceSeq, &observed, &received, &changed, &updated, &reported, &o.availability, &o.freshness, &o.provenance, &o.valueKind, &o.measurementValue, &o.measurementUnit, &o.measurementCanonical, &o.measurementScale, &o.measurementOffset, &o.measurementDimension, &o.measurementPrecision, &o.measurementSemantic, &o.measurementAggregation, &evidence)
	if err != nil {
		return nil, historyFailure(historyquery.ErrUnavailable, err)
	}
	if nanos < 0 || nanos > 999999999 || len(storedDigest) != 32 {
		return nil, historyquery.ErrIntegrity
	}
	if evidence != nil {
		row.evidence, err = recorderevidence.Parse(evidence)
		if err != nil {
			return nil, historyquery.ErrIntegrity
		}
		entity, _ := row.evidence.Entity().Value()
		instance, _ := row.evidence.Instance().Value()
		if entity != wire.Entity.GetEntityId() || instance != row.instance {
			return nil, historyquery.ErrIntegrity
		}
	}
	row.sequence = sequence
	row.admittedAt = time.Unix(seconds, int64(nanos)).UTC()
	o.entity = wire.Entity.GetEntityId()
	o.sourceSession = wire.SourceSession
	o.revision = wire.Revision
	o.sourceSequence = wire.SourceSeq
	o.payload = payload
	o.payloadDigest = sha256.Sum256(payload)
	if int32(o.availability) != int32(wire.Quality.Availability) || int32(o.freshness) != int32(wire.Quality.Freshness) || int32(o.provenance) != int32(wire.Quality.Provenance) {
		return nil, historyquery.ErrIntegrity
	}
	kind := historicalRawKind(wire)
	if (kind == nil) != (o.valueKind == nil) || (kind != nil && *kind != *o.valueKind) {
		return nil, historyquery.ErrIntegrity
	}

	if wire.ObservedAt != nil {
		at := wire.ObservedAt.AsTime().UTC()
		o.observedAt = &at
	}
	o.receivedAt = wire.ReceivedAt.AsTime().UTC()
	o.lastChanged = wire.LastChanged.AsTime().UTC()
	o.lastUpdated = wire.LastUpdated.AsTime().UTC()
	o.lastReported = wire.LastReported.AsTime().UTC()
	if revision != decimalUint(o.revision) || source != o.sourceSession || (sourceSeq == nil) != (wire.SourceSeq == nil) || (sourceSeq != nil && *sourceSeq != decimalUint(*wire.SourceSeq)) || (observed == nil) != (o.observedAt == nil) {
		return nil, historyquery.ErrIntegrity
	}
	if observed != nil && !observed.Equal(o.observedAt.Truncate(time.Microsecond)) {
		return nil, historyquery.ErrIntegrity
	}
	for _, pair := range [][2]time.Time{{received, o.receivedAt}, {changed, o.lastChanged}, {updated, o.lastUpdated}, {reported, o.lastReported}} {
		if !pair[0].Equal(pair[1].Truncate(time.Microsecond)) {
			return nil, historyquery.ErrIntegrity
		}
	}
	var descriptor *recordermaintenance.NumericInput
	fieldsPresent := o.measurementValue != nil && o.measurementUnit != nil && o.measurementCanonical != nil && o.measurementScale != nil && o.measurementOffset != nil && o.measurementDimension != nil && o.measurementPrecision != nil && o.measurementSemantic != nil && o.measurementAggregation != nil
	fieldsAbsent := o.measurementValue == nil && o.measurementUnit == nil && o.measurementCanonical == nil && o.measurementScale == nil && o.measurementOffset == nil && o.measurementDimension == nil && o.measurementPrecision == nil && o.measurementSemantic == nil && o.measurementAggregation == nil
	if wire.GetMeasurement() != nil {
		if !fieldsPresent || *o.measurementDimension < 1 || *o.measurementDimension > 6 || *o.measurementSemantic < 1 || *o.measurementSemantic > 3 || *o.measurementAggregation < 1 || *o.measurementAggregation > 3 {
			return nil, historyquery.ErrIntegrity
		}
		descriptor = &recordermaintenance.NumericInput{Value: *o.measurementValue, Unit: *o.measurementUnit, CanonicalUnit: *o.measurementCanonical, Scale: *o.measurementScale, Offset: *o.measurementOffset, Dimension: domain.Dimension(*o.measurementDimension), Precision: int(*o.measurementPrecision), Semantic: domain.SemanticClass(*o.measurementSemantic), Aggregation: domain.AggregationKind(*o.measurementAggregation)}
		if !historicalNumericDescriptorValid(*descriptor) || wire.GetMeasurement().Unit != *o.measurementUnit || wire.GetMeasurement().Value != *o.measurementValue {
			return nil, historyquery.ErrIntegrity
		}
	} else if !fieldsAbsent {
		return nil, historyquery.ErrIntegrity
	}

	row.observed = o
	digest := digestRecorderRecord(row)
	if !bytes.Equal(storedDigest, digest[:]) {
		return nil, historyquery.ErrIntegrity
	}
	return descriptor, nil
}

func historicalRawKind(wire *state.ObservedState) *int16 {
	var kind int16
	switch wire.Value.(type) {
	case nil:
		return nil
	case *state.ObservedState_SwitchOn:
		kind = int16(domain.ValueSwitch)
	case *state.ObservedState_Measurement:
		kind = int16(domain.ValueMeasurement)
	case *state.ObservedState_Light:
		kind = int16(domain.ValueLight)
	case *state.ObservedState_Fan:
		kind = int16(domain.ValueFan)
	case *state.ObservedState_Climate:
		kind = int16(domain.ValueClimate)
	case *state.ObservedState_Helper:
		kind = int16(domain.ValueHelper)
	case *state.ObservedState_Solar:
		kind = int16(domain.ValueSolar)
	case *state.ObservedState_SolarEvent:
		kind = int16(domain.ValueSolarEvent)
	case *state.ObservedState_Cover:
		kind = int16(domain.ValueCover)
	case *state.ObservedState_Lock:
		kind = int16(domain.ValueLock)
	case *state.ObservedState_Alarm:
		kind = int16(domain.ValueAlarm)
	case *state.ObservedState_Weather:
		kind = int16(domain.ValueWeather)
	case *state.ObservedState_Presence:
		kind = int16(domain.ValuePresence)
	case *state.ObservedState_PresenceDetails:
		kind = int16(domain.ValuePresenceDetails)
	default:
		kind = -1
	}
	return &kind
}
