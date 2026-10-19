// Package helpers persists admitted helper values independently of configuration
// generations and sequencer observations. Services and activation belong to its callers.
package helpers

import (
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
)

// Existing NHA-033 names delegate to the single closed operational sum in domain.
type Kind = domain.HelperKind

const (
	KindInvalid     = domain.HelperInvalid
	KindBoolean     = domain.HelperBoolean
	KindNumber      = domain.HelperNumber
	KindCounter     = domain.HelperCounter
	KindButtonPress = domain.HelperButtonPress
	KindDate        = domain.HelperDate
	KindTime        = domain.HelperTime
	KindDateTime    = domain.HelperDateTime
	KindSelect      = domain.HelperSelect
	KindText        = domain.HelperText
)

type Value = domain.HelperValue

func Boolean(value bool) Value  { return domain.NewHelperBoolean(value) }
func Counter(value int64) Value { return domain.NewHelperCounter(value) }
func Number(value float64) (Value, error) {
	v, err := domain.NewHelperNumber(value)
	if err != nil {
		return Value{}, failure(InvalidValue)
	}
	return v, nil
}

// Definition contains only configuration already admitted by bundle. It cannot
// be constructed from raw YAML or arbitrary bounds at an operational call site.
type Definition struct {
	kind         Kind
	boolean      bundle.BoolHelper
	number       bundle.NumberHelper
	counter      bundle.CounterHelper
	button       bundle.ButtonHelper
	datetime     bundle.DatetimeHelper
	selectHelper bundle.SelectHelper
	text         bundle.TextHelper
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
func ButtonDefinition(value bundle.ButtonHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: KindButtonPress, button: value}, nil
}
func DatetimeDefinition(value bundle.DatetimeHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: value.Mode(), datetime: value}, nil
}
func SelectDefinition(value bundle.SelectHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: KindSelect, selectHelper: value}, nil
}
func TextDefinition(value bundle.TextHelper) (Definition, error) {
	if !value.Valid() {
		return Definition{}, failure(InvalidDefinition)
	}
	return Definition{kind: KindText, text: value}, nil
}
func (d Definition) Valid() bool {
	switch d.kind {
	case KindBoolean:
		return d.boolean.Valid()
	case KindNumber:
		return d.number.Valid()
	case KindCounter:
		return d.counter.Valid()
	case KindButtonPress:
		return d.button.Valid()
	case KindDate, KindTime, KindDateTime:
		return d.datetime.Valid() && d.datetime.Mode() == d.kind
	case KindSelect:
		return d.selectHelper.Valid()
	case KindText:
		return d.text.Valid()
	default:
		return false
	}
}
func (d Definition) Kind() Kind { return d.kind }
func (d Definition) Alias() (domain.HAAlias, error) {
	if !d.Valid() {
		return domain.HAAlias{}, failure(InvalidDefinition)
	}
	return d.alias(), nil
}
func (d Definition) alias() domain.HAAlias {
	switch d.kind {
	case KindBoolean:
		return d.boolean.ID()
	case KindNumber:
		return d.number.ID()
	case KindCounter:
		return d.counter.ID()
	case KindButtonPress:
		return d.button.ID()
	case KindDate, KindTime, KindDateTime:
		return d.datetime.ID()
	case KindSelect:
		return d.selectHelper.ID()
	case KindText:
		return d.text.ID()
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
		if !present || !valid {
			return Value{}, false
		}
		v, err := Number(number)
		return v, err == nil
	case KindCounter:
		value, present := d.counter.Initial()
		return Counter(value), present
	case KindDate, KindTime, KindDateTime:
		return d.datetime.Initial()
	case KindSelect:
		value, present := d.selectHelper.Initial()
		if !present {
			return Value{}, false
		}
		v, err := domain.NewHelperSelect(value)
		return v, err == nil
	case KindText:
		value, present := d.text.Initial()
		if !present {
			return Value{}, false
		}
		v, err := domain.NewHelperText(value)
		return v, err == nil
	default:
		return Value{}, false
	}
}
func (d Definition) accepts(value Value) bool {
	kind, err := value.Kind()
	if !d.Valid() || err != nil || kind != d.kind {
		return false
	}
	switch d.kind {
	case KindBoolean:
		return true
	case KindNumber:
		minimum, _ := d.number.Min().Float64()
		maximum, _ := d.number.Max().Float64()
		number, _ := value.Number()
		return minimum <= number && number <= maximum
	case KindCounter:
		minimum, hasMin := d.counter.Minimum()
		maximum, hasMax := d.counter.Maximum()
		counter, _ := value.Counter()
		return (!hasMin || minimum <= counter) && (!hasMax || counter <= maximum)
	case KindButtonPress, KindDate, KindTime, KindDateTime:
		return true
	case KindSelect:
		selected, _ := value.Select()
		for _, option := range d.selectHelper.Options() {
			if selected == option {
				return true
			}
		}
		return false
	case KindText:
		valueText, _ := value.Text()
		minimum, maximum := d.text.Bounds()
		runes := utf8.RuneCountInString(valueText)
		return runes >= minimum && runes <= maximum
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
	if !s.handle.valid() || !s.kind.Valid() || !s.revision.validFor(s.handle.entity) {
		return false
	}
	switch s.revision.state {
	case revisionAbsent:
		return !s.value.Valid() && s.updatedAt.IsZero()
	case revisionPresent:
		kind, err := s.value.Kind()
		return err == nil && kind == s.kind && !s.updatedAt.IsZero()
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
