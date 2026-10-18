package domain

import (
	"errors"
	"math"
	"sync"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidUnit        = errors.New("unit is not registered in the catalogue")
	ErrInvalidMeasurement = errors.New("invalid numeric measurement")
	ErrDimensionMismatch  = errors.New("measurement dimensions differ; integration is required")
	ErrInexactPrecision   = errors.New("unit conversion cannot preserve decimal precision")
)

// Dimension and numeric SemanticClass are closed domain sets. Units are an open,
// explicitly registered catalogue within these dimensions.
type Dimension uint8

const (
	DimensionUnspecified Dimension = iota
	DimensionPower
	DimensionEnergy
	DimensionTemperature
	DimensionRatio
	DimensionIlluminance
)

func (d Dimension) valid() bool {
	switch d {
	case DimensionPower, DimensionEnergy, DimensionTemperature, DimensionRatio, DimensionIlluminance:
		return true
	case DimensionUnspecified:
		return false
	default:
		return false
	}
}

type SemanticClass uint8

const (
	SemanticUnspecified SemanticClass = iota
	SemanticMeasurement
	SemanticTotal
	SemanticTotalIncreasing
)

func (s SemanticClass) valid() bool {
	switch s {
	case SemanticMeasurement, SemanticTotal, SemanticTotalIncreasing:
		return true
	case SemanticUnspecified:
		return false
	default:
		return false
	}
}

type AggregationKind uint8

const (
	AggregationUnspecified AggregationKind = iota
	AggregationCurrent
	AggregationSum
	AggregationMean
)

func (a AggregationKind) valid() bool {
	switch a {
	case AggregationCurrent, AggregationSum, AggregationMean:
		return true
	case AggregationUnspecified:
		return false
	default:
		return false
	}
}

// Unit retains the admitted source spelling as well as the catalogue's
// canonical spelling. Its scale and offset map a value to the dimension's base:
// base = value*scale + offset. The zero value cannot be used in a measurement.
type Unit struct {
	symbol    string
	canonical string
	dimension Dimension
	scale     float64
	offset    float64
}

func (u Unit) valid() bool {
	return u.symbol != "" && u.canonical != "" && u.dimension.valid() &&
		u.scale > 0 && !math.IsInf(u.scale, 0) && !math.IsNaN(u.scale) &&
		!math.IsInf(u.offset, 0) && !math.IsNaN(u.offset)
}

func (u Unit) Symbol() (string, error) {
	if !u.valid() {
		return "", ErrInvalidUnit
	}
	return u.symbol, nil
}

func (u Unit) CanonicalSymbol() (string, error) {
	if !u.valid() {
		return "", ErrInvalidUnit
	}
	return u.canonical, nil
}

func (u Unit) Dimension() (Dimension, error) {
	if !u.valid() {
		return DimensionUnspecified, ErrInvalidUnit
	}
	return u.dimension, nil
}

// UnitCatalog owns admission. Registration is explicit and thread-safe; a
// parsed Unit is an immutable snapshot even if other entries are added later.
type UnitCatalog struct {
	mu      sync.RWMutex
	entries map[string]Unit
}

func NewUnitCatalog() *UnitCatalog {
	c := &UnitCatalog{entries: make(map[string]Unit)}
	for _, u := range []Unit{
		{symbol: "W", canonical: "W", dimension: DimensionPower, scale: 1},
		{symbol: "kW", canonical: "kW", dimension: DimensionPower, scale: 1000},
		{symbol: "Wh", canonical: "Wh", dimension: DimensionEnergy, scale: 3600},
		{symbol: "kW.h", canonical: "kW.h", dimension: DimensionEnergy, scale: 3600000},
		{symbol: "kWh", canonical: "kW.h", dimension: DimensionEnergy, scale: 3600000},
		{symbol: "Cel", canonical: "Cel", dimension: DimensionTemperature, scale: 1, offset: 273.15},
		{symbol: "%", canonical: "%", dimension: DimensionRatio, scale: 0.01},
		{symbol: "lx", canonical: "lx", dimension: DimensionIlluminance, scale: 1},
	} {
		c.entries[u.symbol] = u
	}
	return c
}

func validUnitSymbol(symbol string) bool {
	if symbol == "" || !utf8.ValidString(symbol) {
		return false
	}
	for _, r := range symbol {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Register admits a new affine unit in a known dimension. A provider must
// declare its scale and offset; an arbitrary wire string never becomes a unit.
func (c *UnitCatalog) Register(symbol string, dimension Dimension, scale, offset float64) error {
	if c == nil || !validUnitSymbol(symbol) || !dimension.valid() ||
		scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) ||
		math.IsNaN(offset) || math.IsInf(offset, 0) {
		return ErrInvalidUnit
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return ErrInvalidUnit
	}
	if _, exists := c.entries[symbol]; exists {
		return ErrInvalidUnit
	}
	c.entries[symbol] = Unit{symbol: symbol, canonical: symbol, dimension: dimension, scale: scale, offset: offset}
	return nil
}

func (c *UnitCatalog) Parse(symbol string) (Unit, error) {
	if c == nil || !validUnitSymbol(symbol) {
		return Unit{}, ErrInvalidUnit
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, found := c.entries[symbol]
	if !found || !u.valid() {
		return Unit{}, ErrInvalidUnit
	}
	return u, nil
}

// Measurement is a finite numeric value with explicit unit, dimension,
// decimal-place precision, semantic class and aggregation kind. No publisher
// should accept its zero value: Components checks every invariant.
type Measurement struct {
	value       float64
	unit        Unit
	precision   int
	semantic    SemanticClass
	aggregation AggregationKind
	valid       bool
}

// NewMeasurement accepts nonnegative decimal places. A conversion may return
// negative places, meaning resolution coarser than one target unit.
func NewMeasurement(value float64, unit Unit, precision int, semantic SemanticClass, aggregation AggregationKind) (Measurement, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || !unit.valid() ||
		precision < 0 || precision > 308 || !semantic.valid() || !aggregation.valid() {
		return Measurement{}, ErrInvalidMeasurement
	}
	return Measurement{value: value, unit: unit, precision: precision, semantic: semantic, aggregation: aggregation, valid: true}, nil
}

type MeasurementComponents struct {
	Value       float64
	Unit        Unit
	Dimension   Dimension
	Precision   int
	Semantic    SemanticClass
	Aggregation AggregationKind
}

func (m Measurement) Components() (MeasurementComponents, error) {
	if !m.valid || math.IsNaN(m.value) || math.IsInf(m.value, 0) || !m.unit.valid() ||
		m.precision < -308 || m.precision > 308 || !m.semantic.valid() || !m.aggregation.valid() {
		return MeasurementComponents{}, ErrInvalidMeasurement
	}
	return MeasurementComponents{m.value, m.unit, m.unit.dimension, m.precision, m.semantic, m.aggregation}, nil
}

// Convert changes units only within the same dimension. In particular, power
// cannot become energy without a separately modeled time integral. A decimal
// resolution is preserved only for a power-of-ten scale ratio.
func (m Measurement) Convert(target Unit) (Measurement, error) {
	if _, err := m.Components(); err != nil {
		return Measurement{}, err
	}
	if !target.valid() {
		return Measurement{}, ErrInvalidUnit
	}
	if m.unit.dimension != target.dimension {
		return Measurement{}, ErrDimensionMismatch
	}
	shift := math.Log10(target.scale / m.unit.scale)
	whole := math.Round(shift)
	if math.IsNaN(shift) || math.IsInf(shift, 0) || math.Abs(shift-whole) > 1e-12 ||
		whole < -308 || whole > 308 {
		return Measurement{}, ErrInexactPrecision
	}
	precision := m.precision + int(whole)
	if precision < -308 || precision > 308 {
		return Measurement{}, ErrInexactPrecision
	}
	value := m.value*(m.unit.scale/target.scale) + (m.unit.offset-target.offset)/target.scale
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Measurement{}, ErrInvalidMeasurement
	}
	return Measurement{value: value, unit: target, precision: precision, semantic: m.semantic, aggregation: m.aggregation, valid: true}, nil
}
