// Package recordermaintenance computes historical contributions and runs bounded
// housekeeping. It owns no observation writer, command ledger or device access.
package recordermaintenance

import (
	"encoding/hex"
	"errors"
	"math"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrInvalid       = errors.New("recorder maintenance: invalid admitted value")
	ErrBudget        = errors.New("recorder maintenance: budget exceeded")
	ErrUnavailable   = errors.New("recorder maintenance: unavailable")
	ErrIntegrity     = errors.New("recorder maintenance: integrity failure")
	ErrCommitUnknown = errors.New("recorder maintenance: commit outcome unknown")
)

const MaxPrefixRows = 500
const MaxInputBytes = 4 << 20
const MaxOwnerBytes = 8 << 20
const AttemptTimeout = 2 * time.Second

// Reason is a set of explicit causes. A zero set means no known issue, never
// proof of complete coverage by itself; duration and logical scope remain separate.
type Reason uint16

const (
	Gap Reason = 1 << iota
	EpochBoundary
	RetentionPruned
	QualityBreak
	UnitBreak
	ClockRegression
	CounterDip
	UnknownBaseline
	BudgetLimit
	UnprovenTail
	NonNumeric
	UnsupportedNumeric // Admitted source semantics or arithmetic exceed this analytical representation.
)
const allReasons = Gap | EpochBoundary | RetentionPruned | QualityBreak | UnitBreak | ClockRegression | CounterDip | UnknownBaseline | BudgetLimit | UnprovenTail | NonNumeric | UnsupportedNumeric

func (r Reason) Valid() bool { return r & ^allReasons == 0 }

type Reset uint8

const (
	ResetUnspecified Reset = iota
	ResetNotApplicable
	ResetUnknown
	ResetNotDetected
	ResetInferred
)

type Window struct{ start, end time.Time }

func NewWindow(start time.Time) (Window, error) {
	if !validUTC(start) || !start.Equal(start.Truncate(time.Hour)) {
		return Window{}, ErrInvalid
	}
	end := start.Add(time.Hour)
	if !validUTC(end) {
		return Window{}, ErrInvalid
	}
	return Window{start, end}, nil
}
func (w Window) Bounds() (time.Time, time.Time) { return w.start, w.end }
func (w Window) Valid() bool {
	return validUTC(w.start) && w.end.Equal(w.start.Add(time.Hour)) && w.start.Equal(w.start.Truncate(time.Hour))
}

// NumericInput is a historical descriptor, not a lookup in today's unit catalogue.
// Admission keeps negative historical precision and source spelling intact.
type NumericInput struct {
	Value               float64
	Unit, CanonicalUnit string
	Scale, Offset       float64
	Dimension           domain.Dimension
	Precision           int
	Semantic            domain.SemanticClass
	Aggregation         domain.AggregationKind
}
type PointInput struct {
	Epoch        string
	Sequence     uint64
	At           time.Time
	Availability domain.Availability
	Freshness    domain.Freshness
	Provenance   domain.Provenance
	Kind         domain.ValueKind
	Numeric      *NumericInput
}
type Point struct {
	input PointInput
	valid bool
}

func NewPoint(in PointInput) (Point, error) {
	b, err := hex.DecodeString(in.Epoch)
	if err != nil || len(b) != 16 || hex.EncodeToString(b) != in.Epoch || in.Epoch == "00000000000000000000000000000000" || in.Sequence == 0 || !validUTC(in.At) {
		return Point{}, ErrInvalid
	}
	if _, err = domain.NewQuality(in.Availability, in.Freshness, in.Provenance); err != nil {
		return Point{}, ErrInvalid
	}
	switch in.Kind {
	case domain.ValueUnspecified:
		if in.Numeric != nil {
			return Point{}, ErrInvalid
		}
	case domain.ValueMeasurement:
		if in.Numeric == nil || !validNumeric(*in.Numeric) {
			return Point{}, ErrInvalid
		}
		n := *in.Numeric
		in.Numeric = &n
	case domain.ValueSwitch, domain.ValueLight, domain.ValueFan, domain.ValueClimate, domain.ValueHelper, domain.ValueSolar, domain.ValueSolarEvent, domain.ValueCover, domain.ValueLock, domain.ValueAlarm,
		domain.ValueWeather, domain.ValuePresence, domain.ValuePresenceDetails:
		if in.Numeric != nil {
			return Point{}, ErrInvalid
		}
	default:
		return Point{}, ErrInvalid
	}
	return Point{input: in, valid: true}, nil
}
func (p Point) Input() PointInput {
	in := p.input
	if in.Numeric != nil {
		n := *in.Numeric
		in.Numeric = &n
	}
	return in
}
func (p Point) Valid() bool {
	if !p.valid {
		return false
	}
	_, err := NewPoint(p.input)
	return err == nil
}
func validNumeric(n NumericInput) bool {
	if !finite(n.Value) || !finite(n.Scale) || !finite(n.Offset) || n.Scale <= 0 || n.Unit == "" || n.CanonicalUnit == "" || len(n.Unit) > 256 || len(n.CanonicalUnit) > 256 || n.Precision < -308 || n.Precision > 308 {
		return false
	}
	for _, symbol := range []string{n.Unit, n.CanonicalUnit} {
		if !utf8.ValidString(symbol) {
			return false
		}
		for _, r := range symbol {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return false
			}
		}
	}
	switch n.Dimension {
	case domain.DimensionPower, domain.DimensionEnergy, domain.DimensionTemperature, domain.DimensionRatio, domain.DimensionIlluminance, domain.DimensionCount:
	default:
		return false
	}
	switch n.Semantic {
	case domain.SemanticMeasurement, domain.SemanticTotal, domain.SemanticTotalIncreasing:
	case domain.SemanticUnspecified:
		return false
	default:
		return false
	}
	switch n.Aggregation {
	case domain.AggregationCurrent, domain.AggregationSum, domain.AggregationMean:
		return true
	case domain.AggregationUnspecified:
		return false
	default:
		return false
	}
}
func operationalNumeric(n NumericInput) bool {
	switch n.Semantic {
	case domain.SemanticMeasurement:
		return n.Aggregation == domain.AggregationCurrent || n.Aggregation == domain.AggregationMean
	case domain.SemanticTotal:
		return n.Aggregation == domain.AggregationSum && n.Offset == 0
	case domain.SemanticTotalIncreasing:
		return n.Aggregation == domain.AggregationSum && n.Offset == 0 && n.Value >= 0
	case domain.SemanticUnspecified:
		return false
	default:
		return false
	}
}
func validUTC(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Year() >= 1 && t.Year() <= 9999
}
func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

// CivilCutoff clamps the day instead of Go's AddDate normalization through the
// following month. Every retention comparison is independent of the local TZ.
func CivilCutoff(asOf time.Time) (time.Time, error) {
	if !validUTC(asOf) {
		return time.Time{}, ErrInvalid
	}
	y, m, d := asOf.Date()
	m -= 13
	for m < time.January {
		y--
		m += 12
	}
	if y < 1 {
		return time.Time{}, ErrInvalid
	}
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if d > last {
		d = last
	}
	return time.Date(y, m, d, asOf.Hour(), asOf.Minute(), asOf.Second(), asOf.Nanosecond(), time.UTC), nil
}
