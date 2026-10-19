package helpers

import (
	"math"
	"time"

	"github.com/damione1/Homeward/contracts"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/scope"
)

// Operation is a closed intrinsic mutation. Configuration edits and external
// automation effects are outside the helper value service.
type Operation uint8

const (
	OperationInvalid Operation = iota
	OperationSet
	OperationToggle
	OperationIncrement
	OperationDecrement
	OperationReset
	OperationPress
	OperationNext
	OperationPrevious
)

func Compute(definition Definition, current Snapshot, operation Operation, target Value, now time.Time) (Value, error) {
	if !definition.Valid() || !current.Valid() || current.kind != definition.kind || current.handle.alias != definition.alias() {
		return Value{}, failure(InvalidInput)
	}
	present, hasValue := current.Value()
	if operation == OperationSet {
		if !definition.accepts(target) {
			return Value{}, failure(InvalidValue)
		}
		if definition.kind == KindCounter {
			counter, _ := target.Counter()
			if counter%definition.counter.Step() != 0 {
				return Value{}, failure(InvalidValue)
			}
		}
		if definition.kind == KindButtonPress {
			return Value{}, failure(InvalidValue)
		}
		return target, nil
	}
	if target.Valid() {
		return Value{}, failure(InvalidValue)
	}
	switch definition.kind {
	case KindBoolean:
		if operation == OperationToggle && hasValue {
			boolean, _ := present.Boolean()
			return Boolean(!boolean), nil
		}
	case KindCounter:
		if operation == OperationReset {
			initial, ok := definition.initial()
			if !ok {
				return Value{}, failure(InitialAbsent)
			}
			return initial, nil
		}
		if (operation == OperationIncrement || operation == OperationDecrement) && hasValue {
			value, _ := present.Counter()
			step := definition.counter.Step()
			minimum, hasMinimum := definition.counter.Minimum()
			maximum, hasMaximum := definition.counter.Maximum()
			if operation == OperationIncrement {
				if value > math.MaxInt64-step {
					value = math.MaxInt64
				} else {
					value += step
				}
				if hasMaximum && value > maximum {
					value = maximum
				}
			} else {
				if value < math.MinInt64+step {
					value = math.MinInt64
				} else {
					value -= step
				}
				if hasMinimum && value < minimum {
					value = minimum
				}
			}
			return Counter(value), nil
		}
	case KindSelect:
		if (operation == OperationNext || operation == OperationPrevious) && hasValue {
			selected, _ := present.Select()
			options := definition.selectHelper.Options()
			for index, option := range options {
				if option == selected {
					if operation == OperationNext {
						index = (index + 1) % len(options)
					} else {
						index = (index + len(options) - 1) % len(options)
					}
					return domain.NewHelperSelect(options[index])
				}
			}
		}
	case KindButtonPress:
		if operation == OperationPress {
			return domain.NewHelperButtonPress(now)
		}
	}
	return Value{}, failure(InvalidValue)
}

// Description is derived only from an admitted definition and an explicit
// button policy. It never offers Cast or another external integration.
func Description(definition Definition, buttons scope.HelperButtonPolicy) (*registrywire.CapabilityDescription, error) {
	if !definition.Valid() {
		return nil, failure(InvalidDefinition)
	}
	alias, _ := definition.alias().Value()
	c := &helperwire.HelperConstraints{}
	switch definition.kind {
	case KindBoolean:
		c.Kind = helperwire.HelperKind_HELPER_KIND_BOOLEAN
		c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET, helperwire.HelperOperation_HELPER_OPERATION_TOGGLE}
	case KindNumber:
		c.Kind = helperwire.HelperKind_HELPER_KIND_NUMBER
		minimum, _ := definition.number.Min().Float64()
		maximum, _ := definition.number.Max().Float64()
		step, _ := definition.number.Step().Float64()
		c.NumberMinimum, c.NumberMaximum, c.NumberStep = &minimum, &maximum, &step
		c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}
	case KindCounter:
		c.Kind = helperwire.HelperKind_HELPER_KIND_COUNTER
		step := definition.counter.Step()
		c.CounterStep = &step
		minimum, hasMinimum := definition.counter.Minimum()
		if hasMinimum {
			c.CounterMinimum = &minimum
		}
		maximum, hasMaximum := definition.counter.Maximum()
		if hasMaximum {
			c.CounterMaximum = &maximum
		}
		c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET, helperwire.HelperOperation_HELPER_OPERATION_INCREMENT, helperwire.HelperOperation_HELPER_OPERATION_DECREMENT}
		if _, hasInitial := definition.initial(); hasInitial {
			c.Operations = append(c.Operations, helperwire.HelperOperation_HELPER_OPERATION_RESET)
		}
	case KindButtonPress:
		c.Kind = helperwire.HelperKind_HELPER_KIND_BUTTON_PRESS
		switch buttons.Decision(definition.alias()) {
		case scope.HelperButtonLocalPress:
			c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_PRESS}
		case scope.HelperButtonNoAuthorizedTarget:
			c.UnavailableReason = helperwire.HelperReason_HELPER_REASON_NO_AUTHORIZED_TARGET
		}
	case KindDate, KindTime, KindDateTime:
		switch definition.kind {
		case KindDate:
			c.Kind = helperwire.HelperKind_HELPER_KIND_DATE
		case KindTime:
			c.Kind = helperwire.HelperKind_HELPER_KIND_TIME
		case KindDateTime:
			c.Kind = helperwire.HelperKind_HELPER_KIND_DATETIME
		}
		c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}
	case KindSelect:
		c.Kind = helperwire.HelperKind_HELPER_KIND_SELECT
		c.SelectOptions = definition.selectHelper.Options()
		c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET, helperwire.HelperOperation_HELPER_OPERATION_NEXT, helperwire.HelperOperation_HELPER_OPERATION_PREVIOUS}
	case KindText:
		c.Kind = helperwire.HelperKind_HELPER_KIND_TEXT
		minimum, maximum := definition.text.Bounds()
		lower, upper := uint32(minimum), uint32(maximum)
		c.TextMinimumLength, c.TextMaximumLength = &lower, &upper
		c.Operations = []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}
	default:
		return nil, failure(InvalidDefinition)
	}
	return &registrywire.CapabilityDescription{SchemaId: contracts.HelperSchema, Major: 1, DisplayName: alias, Endpoint: "value", Constraints: &registrywire.CapabilityDescription_Helper{Helper: c}}, nil
}
