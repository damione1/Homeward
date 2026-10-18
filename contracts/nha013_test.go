package contracts_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA013WireMeasurementAdmission(t *testing.T) {
	catalog := domain.NewUnitCatalog()
	const precision = 2
	valid := &state.Measurement{Value: 1.25, Unit: "kWh"}
	got, err := contracts.AdmitWireMeasurement(valid, catalog, precision, domain.SemanticTotalIncreasing, domain.AggregationSum)
	if err != nil {
		t.Fatal(err)
	}
	components, err := got.Components()
	if err != nil || components.Value != 1.25 || components.Dimension != domain.DimensionEnergy ||
		components.Precision != precision || components.Semantic != domain.SemanticTotalIncreasing || components.Aggregation != domain.AggregationSum {
		t.Fatalf("wire admission lost metadata: %+v, %v", components, err)
	}
	if symbol, _ := components.Unit.Symbol(); symbol != "kWh" {
		t.Fatalf("wire unit spelling lost: %q", symbol)
	}
	for _, candidate := range []*state.Measurement{
		nil,
		{Value: 1, Unit: "not.registered"},
		{Value: 1, Unit: ""},
		{Value: 1, Unit: "W", DecimalValue: ptr("0.123456789012345678")},
	} {
		m, err := contracts.AdmitWireMeasurement(candidate, catalog, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err == nil {
			t.Fatalf("invalid wire measurement admitted: %+v", candidate)
		}
		if _, err := m.Components(); !errors.Is(err, domain.ErrInvalidMeasurement) {
			t.Fatal("failed wire admission produced publishable measurement")
		}
	}
}

func ptr(s string) *string { return &s }
