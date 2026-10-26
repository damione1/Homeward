package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strconv"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

var ErrRecorderInvalid = errors.New("recorder: invalid storage record")
var ErrRecorderIntegrity = errors.New("recorder: stored content conflicts with admission")
var ErrRecorderTooLarge = errors.New("recorder: encoded state exceeds its admitted budget")

type RecorderEpoch struct{ bytes [16]byte }

func ParseRecorderEpoch(value string) (RecorderEpoch, error) {
	if len(value) != 32 {
		return RecorderEpoch{}, ErrRecorderInvalid
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return RecorderEpoch{}, ErrRecorderInvalid
	}
	var epoch RecorderEpoch
	copy(epoch.bytes[:], decoded)
	if epoch.bytes == ([16]byte{}) {
		return RecorderEpoch{}, ErrRecorderInvalid
	}
	return epoch, nil
}

func (e RecorderEpoch) Hex() (string, error) {
	if e.bytes == ([16]byte{}) {
		return "", ErrRecorderInvalid
	}
	return hex.EncodeToString(e.bytes[:]), nil
}

func (e RecorderEpoch) valid() bool  { return e.bytes != ([16]byte{}) }
func (e RecorderEpoch) data() []byte { return e.bytes[:] }

// recorderObserved is the admitted, immutable projection prepared before a
// transaction. The protobuf is the versioned state payload; columns retain
// provenance and numeric interpretation independently of later catalogues.
type recorderObserved struct {
	entity, sourceSession                                                                   string
	entityDigest                                                                            [32]byte
	revision                                                                                uint64
	observedAt                                                                              *time.Time
	receivedAt, lastChanged, lastUpdated, lastReported                                      time.Time
	sourceSequence                                                                          *uint64
	availability, freshness, provenance                                                     int16
	valueKind                                                                               *int16
	measurementValue                                                                        *float64
	measurementUnit, measurementCanonical                                                   *string
	measurementScale, measurementOffset                                                     *float64
	measurementDimension, measurementPrecision, measurementSemantic, measurementAggregation *int16
	payload                                                                                 []byte
	payloadDigest                                                                           [32]byte
}

type RecorderRecord struct {
	sequence               uint64
	instance               string
	admittedAt             time.Time
	causation, correlation *string
	observed               recorderObserved
	digest                 [32]byte
}

type RecorderSnapshotRecord struct {
	observed recorderObserved
	digest   [32]byte
}

func NewRecorderRecord(sequence uint64, instance domain.IntegrationInstanceID, state domain.ObservedState, causality domain.Causality, admittedAt time.Time, maxPayload int) (RecorderRecord, error) {
	if sequence == 0 || admittedAt.IsZero() {
		return RecorderRecord{}, ErrRecorderInvalid
	}
	name, err := instance.Value()
	if err != nil {
		return RecorderRecord{}, ErrRecorderInvalid
	}
	cause, trace, err := causality.Components()
	if err != nil {
		return RecorderRecord{}, ErrRecorderInvalid
	}
	observed, err := encodeRecorderObserved(state, maxPayload)
	if err != nil {
		return RecorderRecord{}, err
	}
	record := RecorderRecord{sequence: sequence, instance: name, admittedAt: admittedAt.UTC(), observed: observed}
	if cause != nil {
		value, _ := cause.Value()
		record.causation = &value
	}
	if trace != nil {
		value, _ := trace.Value()
		record.correlation = &value
	}
	record.digest = digestRecorderRecord(record)
	return record, nil
}

func NewRecorderSnapshotRecord(state domain.ObservedState, maxPayload int) (RecorderSnapshotRecord, error) {
	observed, err := encodeRecorderObserved(state, maxPayload)
	if err != nil {
		return RecorderSnapshotRecord{}, err
	}
	record := RecorderSnapshotRecord{observed: observed}
	record.digest = digestRecorderSnapshot(record)
	return record, nil
}

func (r RecorderRecord) Sequence() uint64          { return r.sequence }
func (r RecorderRecord) EncodedBytes() int         { return len(r.observed.payload) }
func (r RecorderSnapshotRecord) EncodedBytes() int { return len(r.observed.payload) }

// RetainedBytes charges the encoded batch row after it leaves the sequencer
// mailbox. Entry.EstimatedBytes remains the only charge for an Entry itself.
func (r RecorderRecord) RetainedBytes() int {
	const overBudget = 4<<20 + 1
	size := int(unsafe.Sizeof(RecorderRecord{})) + 256
	for _, part := range []int{len(r.instance), len(r.observed.entity), len(r.observed.sourceSession), len(r.observed.payload)} {
		if part > overBudget-size {
			return overBudget
		}
		size += part
	}
	for _, part := range []*string{r.causation, r.correlation, r.observed.measurementUnit, r.observed.measurementCanonical} {
		if part == nil {
			continue
		}
		if len(*part) > overBudget-size {
			return overBudget
		}
		size += len(*part)
	}
	return size
}

func (r RecorderSnapshotRecord) RetainedBytes() int {
	const overBudget = 4<<20 + 1
	size := int(unsafe.Sizeof(RecorderSnapshotRecord{})) + 256
	for _, part := range []int{len(r.observed.entity), len(r.observed.sourceSession), len(r.observed.payload)} {
		if part > overBudget-size {
			return overBudget
		}
		size += part
	}
	for _, part := range []*string{r.observed.measurementUnit, r.observed.measurementCanonical} {
		if part == nil {
			continue
		}
		if len(*part) > overBudget-size {
			return overBudget
		}
		size += len(*part)
	}
	return size
}

func (o recorderObserved) validDigest() bool {
	return o.entity != "" && o.sourceSession != "" && o.revision != 0 &&
		len(o.payload) > 0 && len(o.payload) <= 4<<20 &&
		o.entityDigest == sha256.Sum256([]byte(o.entity)) &&
		o.payloadDigest == sha256.Sum256(o.payload)
}

func encodeRecorderObserved(state domain.ObservedState, maxPayload int) (recorderObserved, error) {
	if maxPayload < 1 || maxPayload > 4<<20 {
		return recorderObserved{}, ErrRecorderInvalid
	}
	entity, quality, times, source, revision, err := state.Components()
	if err != nil {
		return recorderObserved{}, err
	}
	name, err := entity.Value()
	if err != nil {
		return recorderObserved{}, err
	}
	availability, freshness, provenance, err := quality.Components()
	if err != nil {
		return recorderObserved{}, err
	}
	session, sourceSequence, err := source.Components()
	if err != nil {
		return recorderObserved{}, err
	}
	wire, err := contracts.EncodeObservedState(state)
	if err != nil {
		return recorderObserved{}, err
	}
	if proto.Size(wire) > maxPayload {
		return recorderObserved{}, ErrRecorderTooLarge
	}
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wire)
	if len(payload) > maxPayload {
		return recorderObserved{}, ErrRecorderTooLarge
	}
	if err != nil || len(payload) < 1 {
		return recorderObserved{}, ErrRecorderInvalid
	}
	result := recorderObserved{
		entity: name, entityDigest: sha256.Sum256([]byte(name)), sourceSession: session, revision: revision,
		observedAt: times.ObservedAt, receivedAt: times.ReceivedAt,
		lastChanged: times.LastChanged, lastUpdated: times.LastUpdated, lastReported: times.LastReported,
		sourceSequence: sourceSequence, availability: int16(availability),
		freshness: int16(freshness), provenance: int16(provenance),
		payload: payload, payloadDigest: sha256.Sum256(payload),
	}
	if value, present := state.CurrentValue(); present {
		kind, err := value.Kind()
		if err != nil {
			return recorderObserved{}, err
		}
		encodedKind := int16(kind)
		switch kind {
		case domain.ValueSwitch, domain.ValueMeasurement, domain.ValueLight, domain.ValueFan, domain.ValueClimate:
		case domain.ValueHelper:
			helper, revision, ok := value.Helper()
			if !ok || !helper.Valid() || revision == 0 || encodedKind != 6 {
				return recorderObserved{}, ErrRecorderInvalid
			}
		case domain.ValueSolar:
			if _, ok := value.Solar(); !ok || encodedKind != 7 {
				return recorderObserved{}, ErrRecorderInvalid
			}
		case domain.ValueSolarEvent:
			if _, ok := value.SolarEvent(); !ok || encodedKind != 8 {
				return recorderObserved{}, ErrRecorderInvalid
			}
		case domain.ValueCover:
			if _, ok := value.Cover(); !ok || encodedKind != 9 {
				return recorderObserved{}, ErrRecorderInvalid
			}
		case domain.ValueLock:
			if _, ok := value.Lock(); !ok || encodedKind != 10 {
				return recorderObserved{}, ErrRecorderInvalid
			}
		case domain.ValueAlarm:
			if _, ok := value.Alarm(); !ok || encodedKind != 11 {
				return recorderObserved{}, ErrRecorderInvalid
			}
		case domain.ValueUnspecified:
			return recorderObserved{}, ErrRecorderInvalid
		default:
			return recorderObserved{}, ErrRecorderInvalid
		}
		result.valueKind = &encodedKind
		if kind == domain.ValueMeasurement {
			measurement, _ := value.Measurement()
			parts, err := measurement.Components()
			if err != nil {
				return recorderObserved{}, err
			}
			unit, err := parts.Unit.Symbol()
			if err != nil {
				return recorderObserved{}, err
			}
			canonical, err := parts.Unit.CanonicalSymbol()
			if err != nil {
				return recorderObserved{}, err
			}
			scale, offset, err := parts.Unit.BaseTransform()
			if err != nil {
				return recorderObserved{}, err
			}
			valueCopy := parts.Value
			dimension, precision := int16(parts.Dimension), int16(parts.Precision)
			semantic, aggregation := int16(parts.Semantic), int16(parts.Aggregation)
			result.measurementValue, result.measurementUnit, result.measurementCanonical = &valueCopy, &unit, &canonical
			result.measurementScale, result.measurementOffset = &scale, &offset
			result.measurementDimension, result.measurementPrecision = &dimension, &precision
			result.measurementSemantic, result.measurementAggregation = &semantic, &aggregation
		}
	}
	return result, nil
}

func decimalUint(value uint64) string { return strconv.FormatUint(value, 10) }

func optionalDecimal(value *uint64) any {
	if value == nil {
		return nil
	}
	return decimalUint(*value)
}

func writeRecorderBytes(h hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(value)
}

func writeRecorderString(h hash.Hash, value string) { writeRecorderBytes(h, []byte(value)) }

func writeRecorderOptional(h hash.Hash, value *string) {
	if value == nil {
		writeRecorderBytes(h, nil)
		return
	}
	writeRecorderString(h, *value)
}

func digestRecorderObserved(h hash.Hash, o recorderObserved) {
	writeRecorderString(h, o.entity)
	writeRecorderString(h, decimalUint(o.revision))
	writeRecorderString(h, o.sourceSession)
	if o.sourceSequence == nil {
		writeRecorderString(h, "absent")
	} else {
		writeRecorderString(h, decimalUint(*o.sourceSequence))
	}
	if o.observedAt == nil {
		writeRecorderString(h, "absent")
	} else {
		writeRecorderString(h, o.observedAt.UTC().Format(time.RFC3339Nano))
	}
	for _, at := range []time.Time{o.receivedAt, o.lastChanged, o.lastUpdated, o.lastReported} {
		writeRecorderString(h, at.UTC().Format(time.RFC3339Nano))
	}
	writeRecorderString(h, strconv.Itoa(int(o.availability)))
	writeRecorderString(h, strconv.Itoa(int(o.freshness)))
	writeRecorderString(h, strconv.Itoa(int(o.provenance)))
	if o.valueKind == nil {
		writeRecorderString(h, "absent")
	} else {
		writeRecorderString(h, strconv.Itoa(int(*o.valueKind)))
	}
	// The deterministic wire payload covers current state data. These columns
	// retain the admitted numeric interpretation even if a catalogue changes.
	if o.measurementValue != nil {
		writeRecorderString(h, strconv.FormatFloat(*o.measurementValue, 'g', -1, 64))
		writeRecorderString(h, *o.measurementUnit)
		writeRecorderString(h, *o.measurementCanonical)
		writeRecorderString(h, strconv.FormatFloat(*o.measurementScale, 'g', -1, 64))
		writeRecorderString(h, strconv.FormatFloat(*o.measurementOffset, 'g', -1, 64))
		for _, n := range []*int16{o.measurementDimension, o.measurementPrecision, o.measurementSemantic, o.measurementAggregation} {
			writeRecorderString(h, strconv.Itoa(int(*n)))
		}
	}
	writeRecorderBytes(h, o.payload)
}

func digestRecorderRecord(r RecorderRecord) [32]byte {
	h := sha256.New()
	writeRecorderString(h, decimalUint(r.sequence))
	writeRecorderString(h, r.instance)
	writeRecorderString(h, r.admittedAt.UTC().Format(time.RFC3339Nano))
	writeRecorderOptional(h, r.causation)
	writeRecorderOptional(h, r.correlation)
	digestRecorderObserved(h, r.observed)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func digestRecorderSnapshot(r RecorderSnapshotRecord) [32]byte {
	h := sha256.New()
	digestRecorderObserved(h, r.observed)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
