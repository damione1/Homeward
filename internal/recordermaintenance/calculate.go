package recordermaintenance

import (
	"math"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

// Values carries separate numeric presence, proven duration and failure causes.
// ResetUnknown is deliberately different from a proved zero contribution.
type ValuesInput struct {
	Duration                   time.Duration
	Weighted, Minimum, Maximum float64
	HasMeasurement             bool
	Contribution               float64
	HasContribution            bool
	Reset                      Reset
	Reasons                    Reason
}
type Values struct {
	input    ValuesInput
	admitted bool
}

func NewValues(in ValuesInput) (Values, error) {
	if in.Duration < 0 || in.Duration > time.Hour || !finite(in.Weighted) || !finite(in.Minimum) || !finite(in.Maximum) || !finite(in.Contribution) || !in.Reasons.Valid() {
		return Values{}, ErrInvalid
	}
	switch in.Reset {
	case ResetNotApplicable, ResetUnknown, ResetNotDetected, ResetInferred:
	default:
		return Values{}, ErrInvalid
	}
	if in.HasMeasurement {
		if in.Duration <= 0 || in.Minimum > in.Maximum || in.Reset != ResetNotApplicable || in.HasContribution || !finite(in.Weighted/in.Duration.Seconds()) {
			return Values{}, ErrInvalid
		}
	} else if in.Duration != 0 || in.Weighted != 0 || in.Minimum != 0 || in.Maximum != 0 {
		return Values{}, ErrInvalid
	}
	if !in.HasContribution && in.Contribution != 0 {
		return Values{}, ErrInvalid
	}
	if in.HasContribution && in.Reset == ResetNotApplicable {
		return Values{}, ErrInvalid
	}
	if in.Reset == ResetInferred && !in.HasContribution {
		return Values{}, ErrInvalid
	}
	return Values{input: in, admitted: true}, nil
}

// A historical point can be valid while its integral or conversion is not
// representable in float64. Keep an explicit absence with an incomplete cause,
// rather than poisoning the whole logical prefix or manufacturing a zero.
func numericalRefusal(in ValuesInput) (Values, error) {
	reset := in.Reset
	if reset == ResetInferred {
		reset = ResetUnknown
	}
	return NewValues(ValuesInput{Reset: reset, Reasons: in.Reasons | UnsupportedNumeric})
}

func arithmeticValues(in ValuesInput) (Values, error) {
	if !finite(in.Weighted) || !finite(in.Minimum) || !finite(in.Maximum) || !finite(in.Contribution) || in.HasMeasurement && !finite(in.Weighted/in.Duration.Seconds()) {
		return numericalRefusal(in)
	}
	return NewValues(in)
}
func (v Values) Input() ValuesInput { return v.input }
func (v Values) Valid() bool {
	if !v.admitted {
		return false
	}
	_, err := NewValues(v.input)
	return err == nil
}
func (v Values) Mean() (float64, bool) {
	if !v.Valid() || !v.input.HasMeasurement {
		return 0, false
	}
	return v.input.Weighted / v.input.Duration.Seconds(), true
}

type Edge struct {
	left, right Point
	reasons     Reason
	delta       float64
	hasDelta    bool
	reset       Reset
	valid       bool
}

func NewEdge(previous, current Point, logicalIssues Reason) (Edge, error) {
	if !previous.Valid() || !current.Valid() || !logicalIssues.Valid() {
		return Edge{}, ErrInvalid
	}
	a, b := previous.input, current.input
	if a.Epoch == b.Epoch && b.Sequence <= a.Sequence {
		return Edge{}, ErrIntegrity
	}
	e := Edge{left: previous, right: current, reasons: logicalIssues, reset: ResetUnknown, valid: true}
	if a.Numeric != nil && a.Numeric.Semantic == domain.SemanticMeasurement {
		e.reset = ResetNotApplicable
	}
	if a.Epoch != b.Epoch {
		e.reasons |= EpochBoundary
	}
	if b.At.Before(a.At) {
		e.reasons |= ClockRegression
	}
	if a.Availability != domain.AvailabilityAvailable || b.Availability != domain.AvailabilityAvailable || a.Freshness != domain.FreshnessFresh || b.Freshness != domain.FreshnessFresh || a.Provenance == domain.ProvenanceRestored || b.Provenance == domain.ProvenanceRestored || a.Provenance != b.Provenance {
		e.reasons |= QualityBreak
	}
	if a.Numeric == nil || b.Numeric == nil {
		e.reasons |= NonNumeric
		return e, nil
	}
	x, y := *a.Numeric, *b.Numeric
	if !operationalNumeric(x) || !operationalNumeric(y) {
		e.reasons |= UnsupportedNumeric
		return e, nil
	}
	if x.Dimension != y.Dimension || x.Semantic != y.Semantic || x.Aggregation != y.Aggregation {
		e.reasons |= UnitBreak
	}
	if e.reasons != 0 {
		return e, nil
	}
	left := math.FMA(x.Value, x.Scale, x.Offset)
	right := math.FMA(y.Value, y.Scale, y.Offset)
	if !finite(left) || !finite(right) {
		e.reasons |= UnsupportedNumeric
		return e, nil
	}
	switch x.Semantic {
	case domain.SemanticMeasurement:
		return e, nil
	case domain.SemanticTotal:
		e.delta = right - left
		e.hasDelta = true
	case domain.SemanticTotalIncreasing:
		e.reset = ResetNotDetected
		if right >= left {
			e.delta = right - left
			e.hasDelta = true
		} else if right < 0.9*left {
			e.delta = right
			e.hasDelta = true
			e.reset = ResetInferred
		} else {
			e.reasons |= CounterDip
		}
	case domain.SemanticUnspecified:
		return Edge{}, ErrInvalid
	default:
		return Edge{}, ErrInvalid
	}
	if !finite(e.delta) {
		e.delta, e.hasDelta, e.reset = 0, false, ResetUnknown
		e.reasons |= UnsupportedNumeric
	}
	return e, nil
}

// Project uses one left-constant segment. Counters belong to the observation on
// the right, never to a guessed distribution over intervening hours.
func Project(w Window, e Edge) (Values, error) {
	return ProjectFrom(w, e, time.Time{})
}

// ProjectFrom excludes a suffix already closed by a previous admitted barrier.
func ProjectFrom(w Window, e Edge, coveredAfter time.Time) (Values, error) {
	if !w.Valid() || !e.valid {
		return Values{}, ErrInvalid
	}
	v := ValuesInput{Reset: e.reset, Reasons: e.reasons}
	a, b := e.left.input, e.right.input
	if e.reasons != 0 {
		return NewValues(v)
	}
	if a.Numeric == nil {
		return Values{}, ErrInvalid
	}
	if a.Numeric.Semantic != domain.SemanticMeasurement {
		if !b.At.Before(w.start) && b.At.Before(w.end) {
			v.Contribution = e.delta
			v.HasContribution = e.hasDelta
		}
		return NewValues(v)
	}
	start, end := a.At, b.At
	if !coveredAfter.IsZero() && coveredAfter.After(start) {
		start = coveredAfter
	}
	if start.Before(w.start) {
		start = w.start
	}
	if end.After(w.end) {
		end = w.end
	}
	if !start.Before(end) {
		return NewValues(v)
	}
	// Preserve the first descriptor's unit; affine conversion precedes math.
	x := a.Numeric.Value
	v.Duration = end.Sub(start)
	v.Weighted = x * v.Duration.Seconds()
	v.Minimum = x
	v.Maximum = x
	v.HasMeasurement = true
	return arithmeticValues(v)
}

// ProjectTail requires the caller's SQL proof that the captured admission
// cursor has committed. It never turns a wall-clock read alone into coverage.
func ProjectTail(w Window, p Point, from, through time.Time, issues Reason) (Values, error) {
	if !w.Valid() || !p.Valid() || !validUTC(from) || !validUTC(through) || !issues.Valid() {
		return Values{}, ErrInvalid
	}
	in := p.Input()
	v := ValuesInput{Reset: ResetNotApplicable, Reasons: issues}
	if in.Numeric == nil || in.Numeric.Semantic != domain.SemanticMeasurement {
		v.Reasons |= NonNumeric
		return NewValues(v)
	}
	if !operationalNumeric(*in.Numeric) {
		v.Reasons |= UnsupportedNumeric
	}
	if in.Availability != domain.AvailabilityAvailable || in.Freshness != domain.FreshnessFresh || in.Provenance == domain.ProvenanceRestored {
		v.Reasons |= QualityBreak
	}
	if through.Before(from) {
		v.Reasons |= ClockRegression
	}
	if v.Reasons != 0 {
		return NewValues(v)
	}
	if from.Before(in.At) {
		return Values{}, ErrInvalid
	}
	if from.Before(w.start) {
		from = w.start
	}
	if through.After(w.end) {
		through = w.end
	}
	if !from.Before(through) {
		return NewValues(v)
	}
	v.Duration = through.Sub(from)
	v.Weighted = in.Numeric.Value * v.Duration.Seconds()
	v.Minimum = in.Numeric.Value
	v.Maximum = in.Numeric.Value
	v.HasMeasurement = true
	return arithmeticValues(v)
}

// ConvertValues converts only measurement levels and counter differences. An
// affine offset is applied once to levels, never to a counter delta.
func ConvertValues(value Values, source, target NumericInput) (Values, error) {
	if !value.Valid() || !validNumeric(source) || !validNumeric(target) || source.Dimension != target.Dimension {
		return Values{}, ErrInvalid
	}
	v := value.Input()
	ratio := source.Scale / target.Scale
	offset := (source.Offset - target.Offset) / target.Scale
	if v.HasMeasurement {
		v.Weighted = math.FMA(v.Weighted, ratio, offset*v.Duration.Seconds())
		v.Minimum = math.FMA(v.Minimum, ratio, offset)
		v.Maximum = math.FMA(v.Maximum, ratio, offset)
	}
	if v.HasContribution {
		v.Contribution /= target.Scale
	}
	return arithmeticValues(v)
}
func Merge(left, right Values) (Values, error) {
	if !left.Valid() || !right.Valid() {
		return Values{}, ErrInvalid
	}
	a, b := left.Input(), right.Input()
	if a.Duration > time.Hour-b.Duration || a.Reset == ResetNotApplicable && b.Reset != ResetNotApplicable || b.Reset == ResetNotApplicable && a.Reset != ResetNotApplicable {
		return Values{}, ErrInvalid
	}
	v := a
	v.Reasons |= b.Reasons
	v.Duration += b.Duration
	v.Weighted += b.Weighted
	v.Contribution += b.Contribution
	v.HasContribution = a.HasContribution || b.HasContribution
	if b.HasMeasurement {
		if !a.HasMeasurement {
			v.Minimum = b.Minimum
			v.Maximum = b.Maximum
		} else {
			v.Minimum = math.Min(a.Minimum, b.Minimum)
			v.Maximum = math.Max(a.Maximum, b.Maximum)
		}
		v.HasMeasurement = true
	}
	if a.Reset == ResetNotApplicable {
		v.Reset = ResetNotApplicable
	} else if a.Reset == ResetInferred || b.Reset == ResetInferred {
		v.Reset = ResetInferred
	} else if a.Reset == ResetUnknown || b.Reset == ResetUnknown {
		v.Reset = ResetUnknown
	} else {
		v.Reset = ResetNotDetected
	}
	return arithmeticValues(v)
}
