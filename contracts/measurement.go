package contracts

import (
	"fmt"

	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
)

// AdmitWireMeasurement is the admission boundary for the current double-valued
// wire shape. Capability metadata supplies precision and semantics, which are
// not present in state.v1.Measurement. The original unit spelling is retained
// in the parsed Unit for compatibility output.
//
// decimal_value is deliberately refused here: coercing its exact decimal text
// to a float would silently lose precision. An exact-decimal domain path belongs
// with the later cost/recorder implementation.
func AdmitWireMeasurement(wire *state.Measurement, catalog *domain.UnitCatalog, precision int, semantic domain.SemanticClass, aggregation domain.AggregationKind) (domain.Measurement, error) {
	if wire == nil {
		return domain.Measurement{}, domain.ErrInvalidMeasurement
	}
	if wire.DecimalValue != nil {
		return domain.Measurement{}, fmt.Errorf("%w: decimal_value needs an exact-decimal model", domain.ErrInvalidMeasurement)
	}
	unit, err := catalog.Parse(wire.Unit)
	if err != nil {
		return domain.Measurement{}, err
	}
	return domain.NewMeasurement(wire.Value, unit, precision, semantic, aggregation)
}
