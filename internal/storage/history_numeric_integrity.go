package storage

import (
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"math"
	"unicode"
	"unicode/utf8"
)

// Historical interpretation is validated from the persisted descriptor only.
// In particular it does not inherit today's analytical unit-length limit or
// consult a current capability/catalogue. These bounds match retained records.
func historicalNumericDescriptorValid(v recordermaintenance.NumericInput) bool {
	if v.Scale <= 0 || math.IsNaN(v.Scale) || math.IsInf(v.Scale, 0) || math.IsNaN(v.Offset) || math.IsInf(v.Offset, 0) || v.Precision < -308 || v.Precision > 308 {
		return false
	}
	switch v.Dimension {
	case domain.DimensionPower, domain.DimensionEnergy, domain.DimensionTemperature, domain.DimensionRatio, domain.DimensionIlluminance, domain.DimensionCount:
	default:
		return false
	}
	switch v.Semantic {
	case domain.SemanticMeasurement, domain.SemanticTotal, domain.SemanticTotalIncreasing:
	default:
		return false
	}
	switch v.Aggregation {
	case domain.AggregationCurrent, domain.AggregationSum, domain.AggregationMean:
	default:
		return false
	}
	for _, symbol := range []string{v.Unit, v.CanonicalUnit} {
		if symbol == "" || len(symbol) > historyquery.MaxPointBytes || !utf8.ValidString(symbol) {
			return false
		}
		for _, r := range symbol {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}
