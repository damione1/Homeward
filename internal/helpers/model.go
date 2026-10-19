// Package helpers persists admitted helper values independently of configuration
// generations and sequencer observations. Services and activation belong to its callers.
package helpers

import (
	"math"
	"time"

	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
)

// Kind is the single operational vocabulary for the supported value variants.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindBoolean
	KindNumber
	KindCounter
)

func (k Kind) valid() bool {
	switch k {
	case KindBoolean, KindNumber, KindCounter:
		return true
	default:
		return false
	}
}

// Value is a private tagged sum. Its Go zero value is invalid, whereas explicit
// Boolean(false), Number(0), and Counter(0) are present values.
type Value struct {
	kind    Kind
	boolean bool
	number  float64
	counter int64
}

func Boolean(value bool) Value  { return Value{kind: KindBoolean, boolean: value} }
func Counter(value int64) Value { return Value{kind: KindCounter, counter: value} }
func Number(value float64) (Value, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Value{}, failure(InvalidValue)
	}
	return Value{kind: KindNumber, number: value}, nil
}
func (v Value) Kind() Kind { return v.kind }
func (v Value) Valid() bool {
	switch v.kind {
	case KindBoolean, KindCounter:
		return true
	case KindNumber:
		return !math.IsNaN(v.number) && !math.IsInf(v.number, 0)
	default:
		return false
	}
}
func (v Value) Boolean() (bool, bool)   { return v.boolean, v.kind == KindBoolean }
func (v Value) Number() (float64, bool) { return v.number, v.kind == KindNumber && v.Valid() }
func (v Value) Counter() (int64, bool)  { return v.counter, v.kind == KindCounter }

// Definition contains only configuration already admitted by bundle. It cannot
// be constructed from raw YAML or arbitrary bounds at an operational call site.
type Definition struct {
	kind    Kind
	boolean bundle.BoolHelper
	number  bundle.NumberHelper
	counter bundle.CounterHelper
}

func BooleanDefinition(value bundle.BoolHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: KindBoolean, boolean: value}, nil
}
func NumberDefinition(value bundle.NumberHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: KindNumber, number: value}, nil
}
func CounterDefinition(value bundle.CounterHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: KindCounter, counter: value}, nil
}
func (d Definition) Valid() bool {
	switch d.kind {
	case KindBoolean:
		return d.boolean.Valid()
	case KindNumber:
		return d.number.Valid()
	case KindCounter:
		return d.counter.Valid()
	default:
		return false
	}
}
func (d Definition) Kind() Kind { return d.kind }
func (d Definition) alias() domain.HAAlias {
	switch d.kind {
	case KindBoolean:
		return d.boolean.ID()
	case KindNumber:
		return d.number.ID()
	case KindCounter:
		return d.counter.ID()
	default:
		return domain.HAAlias{}
	}
}
func (d Definition) initial() (Value, bool) {
	switch d.kind {
	case KindBoolean:
		value, present := d.boolean.Initial()
		return Boolean(value), present
	case KindNumber:
		value, present := d.number.Initial()
		number, valid := value.Float64()
		return Value{kind: KindNumber, number: number}, present && valid
	case KindCounter:
		value, present := d.counter.Initial()
		return Counter(value), present
	default:
		return Value{}, false
	}
}
func (d Definition) accepts(value Value) bool {
	if !d.Valid() || !value.Valid() || value.kind != d.kind {
		return false
	}
	switch d.kind {
	case KindBoolean:
		return true
	case KindNumber:
		minimum, _ := d.number.Min().Float64()
		maximum, _ := d.number.Max().Float64()
		return minimum <= value.number && value.number <= maximum
	case KindCounter:
		minimum, hasMin := d.counter.Minimum()
		maximum, hasMax := d.counter.Maximum()
		return (!hasMin || minimum <= value.counter) && (!hasMax || value.counter <= maximum)
	default:
		return false
	}
}

// Handle binds a stable registry identity to its original declaration. A rename
// of its current display alias neither changes this handle nor retargets its data.
// Every repository operation revalidates the binding in the authoritative DB.
type Handle struct {
	entity domain.EntityID
	alias  domain.HAAlias
}

func (h Handle) valid() bool {
	_, entityErr := h.entity.Value()
	_, aliasErr := h.alias.Value()
	return entityErr == nil && aliasErr == nil
}
func (h Handle) Entity() (domain.EntityID, error) {
	if !h.valid() {
		return domain.EntityID{}, failure(InvalidInput)
	}
	return h.entity, nil
}

type revisionState uint8

const (
	revisionInvalid revisionState = iota
	revisionAbsent
	revisionPresent
)

// Revision is a business-data CAS token bound to one stable helper identity.
// Absence is an admitted private variant obtained from Restore, never a Go zero.
type Revision struct {
	entity domain.EntityID
	state  revisionState
	number int64
}

func (r Revision) validFor(entity domain.EntityID) bool {
	if _, err := entity.Value(); err != nil || r.entity != entity {
		return false
	}
	switch r.state {
	case revisionAbsent:
		return r.number == 0
	case revisionPresent:
		return r.number > 0
	default:
		return false
	}
}
func (r Revision) Number() (uint64, bool) {
	if !r.validFor(r.entity) {
		return 0, false
	}
	return uint64(r.number), true
}

// Snapshot is immutable and only exposed after its transaction committed.
// An absent snapshot has a valid absent revision and no value or update time.
type Snapshot struct {
	handle    Handle
	kind      Kind
	value     Value
	revision  Revision
	updatedAt time.Time
}

func (s Snapshot) Valid() bool {
	if !s.handle.valid() || !s.kind.valid() || !s.revision.validFor(s.handle.entity) {
		return false
	}
	switch s.revision.state {
	case revisionAbsent:
		return s.value.kind == KindInvalid && s.updatedAt.IsZero()
	case revisionPresent:
		return s.value.Valid() && s.value.kind == s.kind && !s.updatedAt.IsZero()
	default:
		return false
	}
}
func (s Snapshot) Entity() (domain.EntityID, error) {
	if !s.Valid() {
		return domain.EntityID{}, failure(InvalidInput)
	}
	return s.handle.entity, nil
}
func (s Snapshot) Kind() Kind { return s.kind }
func (s Snapshot) Value() (Value, bool) {
	return s.value, s.Valid() && s.revision.state == revisionPresent
}
func (s Snapshot) Revision() Revision { return s.revision }
func (s Snapshot) UpdatedAt() (time.Time, bool) {
	return s.updatedAt, s.Valid() && s.revision.state == revisionPresent
}
