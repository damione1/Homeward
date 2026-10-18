package domain_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA013IdentityCategories(t *testing.T) {
	raw := "same opaque host value"
	entity, err := domain.ParseEntityID(raw)
	if err != nil {
		t.Fatal(err)
	}
	device, err := domain.ParseDeviceID(raw)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID(raw)
	if err != nil {
		t.Fatal(err)
	}
	action, err := domain.ParseActionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Go's assignability rule is the API boundary: a parsed EntityID cannot
	// be passed as a DeviceID. The identical raw string has no type provenance.
	types := []reflect.Type{reflect.TypeOf(entity), reflect.TypeOf(device), reflect.TypeOf(instance), reflect.TypeOf(action)}
	for i, left := range types {
		for j, right := range types {
			if i != j && (left.AssignableTo(right) || left.ConvertibleTo(right)) {
				t.Fatalf("%v can bypass %v parsing by assignment or explicit conversion", left, right)
			}
		}
	}
	for _, test := range []struct {
		name  string
		parse func(string) error
		zero  func() error
		want  error
	}{
		{"entity", func(s string) error { _, e := domain.ParseEntityID(s); return e }, func() error { _, e := (domain.EntityID{}).Value(); return e }, domain.ErrInvalidEntityID},
		{"device", func(s string) error { _, e := domain.ParseDeviceID(s); return e }, func() error { _, e := (domain.DeviceID{}).Value(); return e }, domain.ErrInvalidDeviceID},
		{"instance", func(s string) error { _, e := domain.ParseIntegrationInstanceID(s); return e }, func() error { _, e := (domain.IntegrationInstanceID{}).Value(); return e }, domain.ErrInvalidIntegrationInstanceID},
		{"action", func(s string) error { _, e := domain.ParseActionID(s); return e }, func() error { _, e := (domain.ActionID{}).Value(); return e }, domain.ErrInvalidActionID},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, raw := range []string{"", string([]byte{0xff})} {
				if err := test.parse(raw); !errors.Is(err, test.want) {
					t.Fatalf("parse(%q): %v", raw, err)
				}
			}
			if err := test.zero(); !errors.Is(err, test.want) {
				t.Fatalf("zero value: %v", err)
			}
		})
	}
	for _, read := range []func() (string, error){entity.Value, device.Value, instance.Value, action.Value} {
		got, err := read()
		if err != nil || got != raw {
			t.Fatalf("opaque value changed: %q, %v", got, err)
		}
	}
}

func TestNHA013UnitCatalogueAdmission(t *testing.T) {
	catalog := domain.NewUnitCatalog()
	for _, invalid := range []string{"", "not.registered", " kW", string([]byte{0xff})} {
		if _, err := catalog.Parse(invalid); !errors.Is(err, domain.ErrInvalidUnit) {
			t.Fatalf("unit %q admitted: %v", invalid, err)
		}
	}
	if _, err := (domain.Unit{}).Symbol(); !errors.Is(err, domain.ErrInvalidUnit) {
		t.Fatalf("zero unit published: %v", err)
	}
	if err := catalog.Register("vendor.test.power", domain.DimensionPower, 2, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Parse("vendor.test.power"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		dim    domain.Dimension
		scale  float64
		offset float64
	}{
		{"vendor.test.power", domain.DimensionPower, 1, 0}, // duplicate
		{"vendor.test.other", domain.DimensionUnspecified, 1, 0},
		{"vendor.test.nan", domain.DimensionPower, math.NaN(), 0},
		{"vendor.test.inf", domain.DimensionPower, 1, math.Inf(1)},
	} {
		if err := catalog.Register(tc.name, tc.dim, tc.scale, tc.offset); !errors.Is(err, domain.ErrInvalidUnit) {
			t.Fatalf("registered invalid %q: %v", tc.name, err)
		}
	}
	unit, err := catalog.Parse("kWh")
	if err != nil {
		t.Fatal(err)
	}
	source, _ := unit.Symbol()
	canonical, _ := unit.CanonicalSymbol()
	if source != "kWh" || canonical != "kW.h" {
		t.Fatalf("source spelling lost: %q, canonical %q", source, canonical)
	}
}

func TestNHA013MeasurementConstruction(t *testing.T) {
	catalog := domain.NewUnitCatalog()
	watt, _ := catalog.Parse("W")
	m, err := domain.NewMeasurement(123.25, watt, 2, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Components()
	if err != nil || got.Value != 123.25 || got.Dimension != domain.DimensionPower ||
		got.Precision != 2 || got.Semantic != domain.SemanticMeasurement || got.Aggregation != domain.AggregationCurrent {
		t.Fatalf("typed components: %+v, %v", got, err)
	}
	for _, tc := range []struct {
		value       float64
		unit        domain.Unit
		precision   int
		semantic    domain.SemanticClass
		aggregation domain.AggregationKind
	}{
		{math.NaN(), watt, 0, domain.SemanticMeasurement, domain.AggregationCurrent},
		{math.Inf(1), watt, 0, domain.SemanticMeasurement, domain.AggregationCurrent},
		{1, domain.Unit{}, 0, domain.SemanticMeasurement, domain.AggregationCurrent},
		{1, watt, -1, domain.SemanticMeasurement, domain.AggregationCurrent},
		{1, watt, 0, domain.SemanticUnspecified, domain.AggregationCurrent},
		{1, watt, 0, domain.SemanticMeasurement, domain.AggregationUnspecified},
		{1, watt, 0, domain.SemanticClass(255), domain.AggregationCurrent},
	} {
		candidate, err := domain.NewMeasurement(tc.value, tc.unit, tc.precision, tc.semantic, tc.aggregation)
		if !errors.Is(err, domain.ErrInvalidMeasurement) {
			t.Fatalf("invalid input admitted: %+v, %v", tc, err)
		}
		if _, err := candidate.Components(); !errors.Is(err, domain.ErrInvalidMeasurement) {
			t.Fatal("failed construction produced publishable measurement")
		}
	}
	if _, err := (domain.Measurement{}).Components(); !errors.Is(err, domain.ErrInvalidMeasurement) {
		t.Fatalf("zero measurement published: %v", err)
	}
}

func TestNHA013DimensionSafeConversion(t *testing.T) {
	catalog := domain.NewUnitCatalog()
	watt, _ := catalog.Parse("W")
	kilowatt, _ := catalog.Parse("kW")
	kilowattHour, _ := catalog.Parse("kW.h")
	m, err := domain.NewMeasurement(1200, watt, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Convert(kilowattHour); !errors.Is(err, domain.ErrDimensionMismatch) {
		t.Fatalf("W -> kWh without integral: %v", err)
	}
	converted, err := m.Convert(kilowatt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := converted.Components()
	if err != nil || got.Value != 1.2 || got.Dimension != domain.DimensionPower || got.Precision != 3 ||
		got.Semantic != domain.SemanticMeasurement || got.Aggregation != domain.AggregationCurrent {
		t.Fatalf("W -> kW: %+v, %v", got, err)
	}
	if _, err := m.Convert(domain.Unit{}); !errors.Is(err, domain.ErrInvalidUnit) {
		t.Fatalf("zero target unit admitted: %v", err)
	}
	if err := catalog.Register("double-W", domain.DimensionPower, 2, 0); err != nil {
		t.Fatal(err)
	}
	custom, _ := catalog.Parse("double-W")
	if _, err := m.Convert(custom); !errors.Is(err, domain.ErrInexactPrecision) {
		t.Fatalf("unrepresentable decimal resolution admitted: %v", err)
	}
	// Avoid overflowing an intermediate base-unit value for identity conversion.
	large, err := domain.NewMeasurement(math.MaxFloat64, kilowatt, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := large.Convert(kilowatt)
	if err != nil {
		t.Fatal(err)
	}
	largeFields, err := unchanged.Components()
	if err != nil || largeFields.Value != math.MaxFloat64 {
		t.Fatalf("same-unit finite value changed: %+v, %v", largeFields, err)
	}
	// Offsets remain explicit within the same dimension; zero is a real value.
	if err := catalog.Register("K", domain.DimensionTemperature, 1, 0); err != nil {
		t.Fatal(err)
	}
	celsius, _ := catalog.Parse("Cel")
	kelvin, _ := catalog.Parse("K")
	zeroC, err := domain.NewMeasurement(0, celsius, 2, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	zeroK, err := zeroC.Convert(kelvin)
	if err != nil {
		t.Fatal(err)
	}
	kelvinFields, err := zeroK.Components()
	if err != nil || math.Abs(kelvinFields.Value-273.15) > 1e-12 || kelvinFields.Precision != 2 ||
		kelvinFields.Dimension != domain.DimensionTemperature {
		t.Fatalf("Cel -> K: %+v, %v", kelvinFields, err)
	}
}
