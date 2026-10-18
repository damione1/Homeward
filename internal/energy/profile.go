// Package energy integrates admitted power observations into durable energy
// counters. It never reads devices or dispatches physical commands.
package energy

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

var ErrInvalidProfile = errors.New("energy: invalid profile")

// Method is the closed set of integration rules supported by this engine.
type Method uint8

const (
	MethodUnspecified Method = iota
	MethodTrapezoidal
	MethodLeft
)

func (m Method) valid() bool { return m == MethodTrapezoidal || m == MethodLeft }

// TimeBasis never changes implicitly within a counter generation.
type TimeBasis uint8

const (
	TimeBasisUnspecified TimeBasis = iota
	TimeBasisCaptureRequired
	TimeBasisReceipt
)

func (b TimeBasis) valid() bool { return b == TimeBasisCaptureRequired || b == TimeBasisReceipt }

// Profile has private, admitted fields. Source and output are registry IDs, not
// aliases or an enum of the three MVP entities.
type Profile struct {
	name         string
	version      uint16
	source       domain.EntityID
	output       domain.EntityID
	method       Method
	basis        TimeBasis
	unit         domain.Unit
	precision    int
	semantic     domain.SemanticClass
	maxGap       time.Duration
	tickInterval time.Duration
}

func NewProfile(name string, version uint16, source, output domain.EntityID, method Method, basis TimeBasis, unit domain.Unit, precision int, semantic domain.SemanticClass, maxGap, tickInterval time.Duration) (Profile, error) {
	if name == "" || len(name) > 80 || !utf8.ValidString(name) || strings.TrimSpace(name) != name || version == 0 ||
		!method.valid() || !basis.valid() || precision < 0 || precision > 9 ||
		maxGap <= 0 || maxGap > 24*time.Hour || tickInterval < 0 || tickInterval > maxGap ||
		(method == MethodTrapezoidal && tickInterval != 0) ||
		(semantic != domain.SemanticTotal && semantic != domain.SemanticTotalIncreasing) {
		return Profile{}, ErrInvalidProfile
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return Profile{}, ErrInvalidProfile
		}
	}
	if _, err := source.Value(); err != nil {
		return Profile{}, ErrInvalidProfile
	}
	if _, err := output.Value(); err != nil || output == source {
		return Profile{}, ErrInvalidProfile
	}
	if dimension, err := unit.Dimension(); err != nil || dimension != domain.DimensionEnergy {
		return Profile{}, ErrInvalidProfile
	}
	symbol, err := unit.Symbol()
	if err != nil || (symbol != "Wh" && symbol != "kWh" && symbol != "kW.h") {
		return Profile{}, ErrInvalidProfile
	}
	return Profile{name, version, source, output, method, basis, unit, precision, semantic, maxGap, tickInterval}, nil
}

// HiloLeftV1 is only a reusable profile template. NHA-090 owns its bindings
// and supplies the source lease; 120 seconds is not a freshness or gap limit.
func HiloLeftV1(source, output domain.EntityID, maxGap time.Duration) (Profile, error) {
	unit, err := domain.NewUnitCatalog().Parse("kWh")
	if err != nil {
		return Profile{}, err
	}
	return NewProfile("hilo-left-v1", 1, source, output, MethodLeft, TimeBasisReceipt, unit, 2, domain.SemanticTotalIncreasing, maxGap, 120*time.Second)
}

func (p Profile) Valid() bool {
	_, err := NewProfile(p.name, p.version, p.source, p.output, p.method, p.basis, p.unit, p.precision, p.semantic, p.maxGap, p.tickInterval)
	return err == nil
}

func (p Profile) Name() string                   { return p.name }
func (p Profile) Version() uint16                { return p.version }
func (p Profile) Source() domain.EntityID        { return p.source }
func (p Profile) Output() domain.EntityID        { return p.output }
func (p Profile) Method() Method                 { return p.method }
func (p Profile) TimeBasis() TimeBasis           { return p.basis }
func (p Profile) Unit() domain.Unit              { return p.unit }
func (p Profile) Precision() int                 { return p.precision }
func (p Profile) Semantic() domain.SemanticClass { return p.semantic }
func (p Profile) MaxGap() time.Duration          { return p.maxGap }
func (p Profile) TickInterval() time.Duration    { return p.tickInterval }
