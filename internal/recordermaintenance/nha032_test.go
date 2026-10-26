package recordermaintenance

import (
	"math"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func point(t *testing.T, seq uint64, at time.Time, value float64, semantic domain.SemanticClass) Point {
	t.Helper()
	agg := domain.AggregationCurrent
	if semantic != domain.SemanticMeasurement {
		agg = domain.AggregationSum
	}
	p, err := NewPoint(PointInput{Epoch: "0123456789abcdef0123456789abcdef", Sequence: seq, At: at, Availability: domain.AvailabilityAvailable, Freshness: domain.FreshnessFresh, Provenance: domain.ProvenanceObserved, Kind: domain.ValueMeasurement, Numeric: &NumericInput{Value: value, Unit: "W", CanonicalUnit: "W", Scale: 1, Dimension: domain.DimensionPower, Precision: 2, Semantic: semantic, Aggregation: agg}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func projected(t *testing.T, w Window, a, b Point, issues Reason) Values {
	t.Helper()
	e, err := NewEdge(a, b, issues)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Project(w, e)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestNHA032HourlyTimeWeightedMeasurement(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w, _ := NewWindow(at)
	a := point(t, 1, at, 10, domain.SemanticMeasurement)
	b := point(t, 2, at.Add(15*time.Minute), 30, domain.SemanticMeasurement)
	c := point(t, 3, at.Add(time.Hour), 70, domain.SemanticMeasurement)
	v, err := Merge(projected(t, w, a, b, 0), projected(t, w, b, c, 0))
	mean, ok := v.Mean()
	if err != nil || !ok || mean != 25 || v.Input().Duration != time.Hour || v.Input().Minimum != 10 || v.Input().Maximum != 30 {
		t.Fatalf("weighted=%+v mean=%v/%v err=%v", v, mean, ok, err)
	}
	partial := projected(t, w, a, point(t, 2, at.Add(30*time.Minute), 30, domain.SemanticMeasurement), 0)
	if mean, ok := partial.Mean(); !ok || mean != 10 || partial.Input().Duration != 30*time.Minute {
		t.Fatalf("partial: %+v", partial)
	}
}
func TestNHA032ZeroAbsentAndQuality(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w, _ := NewWindow(at)
	a := point(t, 1, at, 0, domain.SemanticMeasurement)
	b := point(t, 2, at.Add(time.Hour), 0, domain.SemanticMeasurement)
	if mean, ok := projected(t, w, a, b, 0).Mean(); !ok || mean != 0 {
		t.Fatalf("zero lost: %v %v", mean, ok)
	}
	for _, change := range []func(*PointInput){func(i *PointInput) {
		i.Availability = domain.AvailabilityUnavailable
		i.Kind = domain.ValueUnspecified
		i.Numeric = nil
	}, func(i *PointInput) { i.Freshness = domain.FreshnessStale }, func(i *PointInput) { i.Provenance = domain.ProvenanceRestored }, func(i *PointInput) { i.Kind = domain.ValueHelper; i.Numeric = nil }, func(i *PointInput) { i.Kind = domain.ValueSolar; i.Numeric = nil }, func(i *PointInput) { i.Kind = domain.ValueSolarEvent; i.Numeric = nil }, func(i *PointInput) { i.Kind = domain.ValueCover; i.Numeric = nil }, func(i *PointInput) { i.Kind = domain.ValueLock; i.Numeric = nil }, func(i *PointInput) { i.Kind = domain.ValueAlarm; i.Numeric = nil }} {
		in := b.Input()
		change(&in)
		p, err := NewPoint(in)
		if err != nil {
			t.Fatal(err)
		}
		v := projected(t, w, a, p, 0)
		if _, ok := v.Mean(); ok || v.Input().Reasons == 0 {
			t.Fatalf("invalid quality fabricated value: %+v", v)
		}
	}
	ai, bi := a.Input(), b.Input()
	ai.Provenance = domain.ProvenanceEstimated
	bi.Provenance = domain.ProvenanceEstimated
	a, _ = NewPoint(ai)
	b, _ = NewPoint(bi)
	if _, ok := projected(t, w, a, b, 0).Mean(); !ok {
		t.Fatal("estimated energy was dropped")
	}
}
func TestNHA032CounterBaselineResetDipAndGap(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w, _ := NewWindow(at)
	for _, tc := range []struct {
		semantic domain.SemanticClass
		right    float64
		want     float64
		present  bool
		reset    Reset
		issue    Reason
	}{{domain.SemanticTotal, 90, -10, true, ResetUnknown, 0}, {domain.SemanticTotalIncreasing, 100, 0, true, ResetNotDetected, 0}, {domain.SemanticTotalIncreasing, 5, 5, true, ResetInferred, 0}, {domain.SemanticTotalIncreasing, 90, 0, false, ResetNotDetected, CounterDip}} {
		a := point(t, 1, at, 100, tc.semantic)
		b := point(t, 2, at.Add(time.Minute), tc.right, tc.semantic)
		v := projected(t, w, a, b, 0)
		if v.Input().HasContribution != tc.present || v.Input().Contribution != tc.want || v.Input().Reset != tc.reset || v.Input().Reasons != tc.issue {
			t.Fatalf("case %+v got %+v", tc, v)
		}
	}
	a := point(t, 1, at, 100, domain.SemanticTotalIncreasing)
	b := point(t, 2, at.Add(time.Minute), 5, domain.SemanticTotalIncreasing)
	c := point(t, 3, at.Add(2*time.Minute), 8, domain.SemanticTotalIncreasing)
	v, err := Merge(projected(t, w, a, b, 0), projected(t, w, b, c, 0))
	if err != nil || v.Input().Contribution != 8 || v.Input().Reset != ResetInferred {
		t.Fatalf("reset: %+v %v", v, err)
	}
	for _, issue := range []Reason{Gap, RetentionPruned} {
		v := projected(t, w, a, c, issue)
		if v.Input().HasContribution || v.Input().Reasons&issue == 0 {
			t.Fatalf("crossed issue: %+v", v)
		}
	}
	in := c.Input()
	in.Epoch = "1123456789abcdef0123456789abcdef"
	c, _ = NewPoint(in)
	if v := projected(t, w, a, c, 0); v.Input().HasContribution || v.Input().Reasons&EpochBoundary == 0 {
		t.Fatalf("crossed epoch: %+v", v)
	}
}
func TestNHA032CanonicalUnitsAndAffine(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w, _ := NewWindow(at)
	a := point(t, 1, at, 1, domain.SemanticMeasurement)
	in := a.Input()
	in.Numeric.Unit = "kW"
	in.Numeric.CanonicalUnit = "kW"
	in.Numeric.Scale = 1000
	in.Numeric.Precision = -2
	a, _ = NewPoint(in)
	b := point(t, 2, at.Add(time.Hour), 1000, domain.SemanticMeasurement)
	v := projected(t, w, a, b, 0)
	v, err := ConvertValues(v, *a.Input().Numeric, *b.Input().Numeric)
	if mean, ok := v.Mean(); err != nil || !ok || mean != 1000 {
		t.Fatalf("canonical=%+v %v", v, err)
	}
	source := NumericInput{Value: 0, Unit: "Cel", CanonicalUnit: "Cel", Scale: 1, Offset: 273.15, Dimension: domain.DimensionTemperature, Semantic: domain.SemanticMeasurement, Aggregation: domain.AggregationMean}
	target := source
	target.Unit = "K"
	target.CanonicalUnit = "K"
	target.Offset = 0
	base, _ := NewValues(ValuesInput{Duration: time.Hour, HasMeasurement: true, Reset: ResetNotApplicable})
	v, err = ConvertValues(base, source, target)
	if mean, ok := v.Mean(); err != nil || !ok || math.Abs(mean-273.15) > 1e-9 {
		t.Fatalf("affine %+v %v", v, err)
	}
}
func TestNHA032RetentionCivilMonthsAndAdmission(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"2026-03-31T12:00:00Z", "2025-02-28T12:00:00Z"}, {"2025-03-31T12:00:00Z", "2024-02-29T12:00:00Z"}} {
		at, _ := time.Parse(time.RFC3339, tc.in)
		got, err := CivilCutoff(at)
		if err != nil || got.Format(time.RFC3339) != tc.want {
			t.Fatalf("clamp %v %v", got, err)
		}
	}
	if (Point{}).Valid() {
		t.Fatal("zero admitted")
	}
	if _, err := NewPoint(PointInput{}); err == nil {
		t.Fatal("empty point admitted")
	}
	if _, err := CivilCutoff(time.Now()); err == nil {
		t.Fatal("local clock admitted")
	}
}
func TestNHA032BarrierTailIsClippedFromFollowingSegment(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w, _ := NewWindow(at)
	a := point(t, 1, at, 10, domain.SemanticMeasurement)
	b := point(t, 2, at.Add(time.Hour), 30, domain.SemanticMeasurement)
	tail, err := ProjectTail(w, a, at, at.Add(30*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := NewEdge(a, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := ProjectFrom(w, edge, at.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	v, err := Merge(tail, closed)
	if mean, ok := v.Mean(); err != nil || !ok || mean != 10 || v.Input().Duration != time.Hour {
		t.Fatalf("double suffix %+v %v", v, err)
	}
	unknown, err := ProjectTail(w, a, at, at.Add(time.Hour), Gap)
	if err != nil || unknown.Input().HasMeasurement || unknown.Input().Reasons&Gap == 0 {
		t.Fatalf("unknown tail became value %+v %v", unknown, err)
	}
}

func TestNHA032FiniteExtremesRefuseLocallyWithoutNumericZero(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w, _ := NewWindow(at)
	a := point(t, 1, at, 1e306, domain.SemanticMeasurement)
	b := point(t, 2, at.Add(time.Hour), 1e306, domain.SemanticMeasurement)
	assertRefused := func(value Values, err error) {
		t.Helper()
		in := value.Input()
		if err != nil || !value.Valid() || in.Reasons&UnsupportedNumeric == 0 || in.HasMeasurement || in.HasContribution || in.Duration != 0 {
			t.Fatalf("extreme became global error or fabricated value: %+v %v", in, err)
		}
		if _, present := value.Mean(); present {
			t.Fatal("refused value exposed a numeric zero")
		}
	}
	edge, err := NewEdge(a, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertRefused(Project(w, edge))
	assertRefused(ProjectTail(w, a, at, at.Add(time.Hour), 0))
	left, err := NewValues(ValuesInput{Duration: 30 * time.Minute, Weighted: 9e307, Minimum: 5e304, Maximum: 5e304, HasMeasurement: true, Reset: ResetNotApplicable})
	if err != nil {
		t.Fatal(err)
	}
	assertRefused(Merge(left, left))
	source, target := *a.Input().Numeric, *a.Input().Numeric
	source.Scale = 1000
	assertRefused(ConvertValues(left, source, target))
	input := a.Input()
	input.Numeric.Scale = 1000
	canonical, err := NewPoint(input)
	if err != nil {
		t.Fatal(err)
	}
	assertRefused(Project(w, func() Edge {
		e, err := NewEdge(canonical, b, 0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}()))
	assertRefused(Project(w, func() Edge {
		e, err := NewEdge(point(t, 1, at, -1e308, domain.SemanticTotal), point(t, 2, at.Add(time.Minute), 1e308, domain.SemanticTotal), 0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}()))
	// FMA preserves a representable canonical result despite an overflowing
	// multiplication intermediate. The separate integral may still be refused.
	input.Numeric.Value, input.Numeric.Scale, input.Numeric.Offset = 1e308, 2, -1e308
	stable, err := NewPoint(input)
	if err != nil {
		t.Fatal(err)
	}
	if edge, err := NewEdge(stable, b, 0); err != nil || edge.reasons != 0 {
		t.Fatalf("representable canonical conversion refused: %+v %v", edge, err)
	}
	if _, err := NewValues(ValuesInput{Reset: ResetNotApplicable, Weighted: math.Inf(1)}); err == nil {
		t.Fatal("external invalid DTO admitted")
	}
}
