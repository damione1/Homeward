// Package template evaluates the admitted ha-2026.10/core-1 template subset.
// It uses Gonja only to lex and parse; evaluation never calls Gonja's executor.
package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrInvalidInput = errors.New("invalid template input")
	ErrUnsupported  = errors.New("unsupported template construction")
	ErrBudget       = errors.New("template budget exceeded")
	ErrCancelled    = errors.New("template evaluation cancelled")
	ErrEvaluation   = errors.New("template evaluation failed")
)

const (
	maxSourceBytes = 4096
	maxValueBytes  = 8192
	maxStringBytes = 2048
	maxItems       = 128
	maxValueDepth  = 8
	maxValueNodes  = 256
	maxInputBytes  = 65536
	maxInputNodes  = 1024
)

type valueKind uint8

// ValueKind is the single typed discriminator exposed to callers. Only
// constructors and evaluation can populate a valid Value.
type ValueKind = valueKind

const (
	valueInvalid valueKind = iota
	valueUndefined
	valueNone
	valueBool
	valueInt
	valueFloat
	valueString
	valueList
	valueObject
	valueDate
	valueDateTime
	valueDuration
	valueStateRef
)

const (
	KindUndefined ValueKind = valueUndefined
	KindNone      ValueKind = valueNone
	KindBool      ValueKind = valueBool
	KindInt       ValueKind = valueInt
	KindFloat     ValueKind = valueFloat
	KindString    ValueKind = valueString
	KindList      ValueKind = valueList
	KindObject    ValueKind = valueObject
	KindDate      ValueKind = valueDate
	KindDateTime  ValueKind = valueDateTime
	KindDuration  ValueKind = valueDuration
	KindStateRef  ValueKind = valueStateRef
)

// Value is a closed, immutable template value. Its zero value is invalid.
// Undefined is produced only by an absent lookup, never by a caller.
type Value struct {
	kind valueKind
	b    bool
	i    int64
	f    float64
	s    string
	list []Value
	obj  map[string]Value
	time time.Time
}

func None() Value       { return Value{kind: valueNone} }
func Bool(v bool) Value { return Value{kind: valueBool, b: v} }
func Int(v int64) Value { return Value{kind: valueInt, i: v} }
func String(v string) (Value, error) {
	if !utf8.ValidString(v) || len(v) > maxStringBytes {
		return Value{}, ErrInvalidInput
	}
	return Value{kind: valueString, s: v}, nil
}
func Float(v float64) (Value, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Value{}, ErrInvalidInput
	}
	return Value{kind: valueFloat, f: v}, nil
}

// Binding names an open-set variable or an object attribute.
type Binding struct {
	Name  string
	Value Value
}

func List(items []Value) (Value, error) {
	if len(items) > maxItems {
		return Value{}, ErrInvalidInput
	}
	budget := valueBudget{nodes: 1, maxBytes: maxValueBytes, maxNodes: maxValueNodes}
	for _, item := range items {
		if err := inspectValue(item, 1, &budget); err != nil {
			return Value{}, err
		}
	}
	copyItems := make([]Value, len(items))
	for i, item := range items {
		copyItems[i] = cloneValue(item)
	}
	return Value{kind: valueList, list: copyItems}, nil
}

func Object(fields []Binding) (Value, error) {
	if len(fields) > maxItems {
		return Value{}, ErrInvalidInput
	}
	budget := valueBudget{nodes: 1, maxBytes: maxValueBytes, maxNodes: maxValueNodes}
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if err := validKey(field.Name); err != nil {
			return Value{}, err
		}
		if _, exists := seen[field.Name]; exists {
			return Value{}, ErrInvalidInput
		}
		seen[field.Name] = struct{}{}
		budget.bytes += len(field.Name)
		if err := inspectValue(field.Value, 1, &budget); err != nil {
			return Value{}, err
		}
	}
	object := make(map[string]Value, len(fields))
	for _, field := range fields {
		object[field.Name] = cloneValue(field.Value)
	}
	return Value{kind: valueObject, obj: object}, nil
}

func validKey(s string) error {
	if s == "" || len(s) > 128 || !utf8.ValidString(s) {
		return ErrInvalidInput
	}
	return nil
}

type valueBudget struct {
	bytes, nodes       int
	maxBytes, maxNodes int
}

func inspectValue(v Value, depth int, budget *valueBudget) error {
	if depth > maxValueDepth {
		return ErrInvalidInput
	}
	budget.nodes++
	if budget.nodes > budget.maxNodes || budget.bytes > budget.maxBytes {
		return ErrInvalidInput
	}
	switch v.kind {
	case valueNone, valueBool, valueInt:
		return nil
	case valueFloat:
		if !math.IsNaN(v.f) && !math.IsInf(v.f, 0) {
			return nil
		}
	case valueString:
		if len(v.s) <= maxStringBytes && utf8.ValidString(v.s) {
			budget.bytes += len(v.s)
			if budget.bytes <= budget.maxBytes {
				return nil
			}
		}
	case valueList:
		if len(v.list) > maxItems {
			break
		}
		for _, item := range v.list {
			if err := inspectValue(item, depth+1, budget); err != nil {
				return err
			}
		}
		return nil
	case valueObject:
		if len(v.obj) > maxItems {
			break
		}
		for key, item := range v.obj {
			if err := validKey(key); err != nil {
				return err
			}
			budget.bytes += len(key)
			if budget.bytes > budget.maxBytes {
				return ErrInvalidInput
			}
			if err := inspectValue(item, depth+1, budget); err != nil {
				return err
			}
		}
		return nil
	case valueInvalid, valueUndefined, valueDate, valueDateTime, valueDuration, valueStateRef:
	}
	return ErrInvalidInput
}

func cloneValue(v Value) Value {
	switch v.kind {
	case valueList:
		items := make([]Value, len(v.list))
		for i, item := range v.list {
			items[i] = cloneValue(item)
		}
		v.list = items
	case valueObject:
		object := make(map[string]Value, len(v.obj))
		for key, item := range v.obj {
			object[key] = cloneValue(item)
		}
		v.obj = object
	case valueInvalid, valueUndefined, valueNone, valueBool, valueInt, valueFloat, valueString, valueDate, valueDateTime, valueDuration, valueStateRef:
	}
	return v
}

// DecodeJSON admits bounded JSON without putting an untyped map in the domain.
func DecodeJSON(input []byte) (Value, error) {
	if len(input) == 0 || len(input) > maxValueBytes {
		return Value{}, ErrInvalidInput
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	value, err := decodeJSONValue(dec, 0)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Value{}, ErrInvalidInput
	}
	budget := valueBudget{maxBytes: maxValueBytes, maxNodes: maxValueNodes}
	if err := inspectValue(value, 0, &budget); err != nil {
		return Value{}, err
	}
	return value, nil
}

func decodeJSONValue(dec *json.Decoder, depth int) (Value, error) {
	if depth > maxValueDepth {
		return Value{}, ErrInvalidInput
	}
	token, err := dec.Token()
	if err != nil {
		return Value{}, ErrInvalidInput
	}
	switch v := token.(type) {
	case nil:
		return None(), nil
	case bool:
		return Bool(v), nil
	case string:
		return String(v)
	case json.Number:
		if !strings.ContainsAny(string(v), ".eE") {
			i, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil {
				return Value{}, ErrInvalidInput
			}
			return Int(i), nil
		}
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return Value{}, ErrInvalidInput
		}
		return Float(f)
	case json.Delim:
		switch v {
		case '[':
			items := make([]Value, 0)
			for dec.More() {
				if len(items) >= maxItems {
					return Value{}, ErrInvalidInput
				}
				item, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return Value{}, err
				}
				items = append(items, item)
			}
			if end, err := dec.Token(); err != nil || end != json.Delim(']') {
				return Value{}, ErrInvalidInput
			}
			return Value{kind: valueList, list: items}, nil
		case '{':
			object := make(map[string]Value)
			for dec.More() {
				if len(object) >= maxItems {
					return Value{}, ErrInvalidInput
				}
				keyToken, err := dec.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok || validKey(key) != nil {
					return Value{}, ErrInvalidInput
				}
				if _, duplicate := object[key]; duplicate {
					return Value{}, ErrInvalidInput
				}
				item, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return Value{}, err
				}
				object[key] = item
			}
			if end, err := dec.Token(); err != nil || end != json.Delim('}') {
				return Value{}, ErrInvalidInput
			}
			return Value{kind: valueObject, obj: object}, nil
		}
	}
	return Value{}, ErrInvalidInput
}

// State is a synthetic HA read view. A later adapter may build it from the
// sequencer snapshot; it is not a replacement state or registry authority.
type State struct {
	alias domain.HAAlias
	text  string
	attrs map[string]Value
}

func NewState(alias domain.HAAlias, text string, attrs []Binding) (State, error) {
	if _, err := alias.Value(); err != nil {
		return State{}, ErrInvalidInput
	}
	if len(text) > maxStringBytes || !utf8.ValidString(text) {
		return State{}, ErrInvalidInput
	}
	object, err := Object(attrs)
	if err != nil {
		return State{}, err
	}
	return State{alias: alias, text: text, attrs: object.obj}, nil
}

// Inputs owns copies of every supplied value. Time and zone are injected.
type Inputs struct {
	states map[string]State
	vars   map[string]Value
	now    time.Time
	zone   *time.Location
}

func NewInputs(states []State, variables []Binding, nowUTC time.Time, zone *time.Location) (Inputs, error) {
	if nowUTC.IsZero() || nowUTC.Year() < 1900 || nowUTC.Year() > 2100 || zone == nil || len(states) > maxItems || len(variables) > maxItems {
		return Inputs{}, ErrInvalidInput
	}
	budget := valueBudget{maxBytes: maxInputBytes, maxNodes: maxInputNodes}
	seenStates := make(map[string]struct{}, len(states))
	for _, state := range states {
		alias, err := state.alias.Value()
		if err != nil || len(state.text) > maxStringBytes || !utf8.ValidString(state.text) || len(state.attrs) > maxItems {
			return Inputs{}, ErrInvalidInput
		}
		if _, duplicate := seenStates[alias]; duplicate {
			return Inputs{}, ErrInvalidInput
		}
		seenStates[alias] = struct{}{}
		budget.bytes += len(alias) + len(state.text)
		budget.nodes++
		if budget.bytes > budget.maxBytes || budget.nodes > budget.maxNodes {
			return Inputs{}, ErrInvalidInput
		}
		for key, value := range state.attrs {
			if validKey(key) != nil {
				return Inputs{}, ErrInvalidInput
			}
			budget.bytes += len(key)
			if inspectValue(value, 1, &budget) != nil {
				return Inputs{}, ErrInvalidInput
			}
		}
	}
	seenVars := make(map[string]struct{}, len(variables))
	for _, binding := range variables {
		if validIdentifier(binding.Name) != nil || reservedName(binding.Name) {
			return Inputs{}, ErrInvalidInput
		}
		if _, duplicate := seenVars[binding.Name]; duplicate {
			return Inputs{}, ErrInvalidInput
		}
		seenVars[binding.Name] = struct{}{}
		budget.bytes += len(binding.Name)
		if inspectValue(binding.Value, 0, &budget) != nil {
			return Inputs{}, ErrInvalidInput
		}
	}
	in := Inputs{states: make(map[string]State, len(states)), vars: make(map[string]Value, len(variables)), now: nowUTC.UTC(), zone: zone}
	for _, state := range states {
		alias, _ := state.alias.Value()
		attrs := make(map[string]Value, len(state.attrs))
		for key, value := range state.attrs {
			attrs[key] = cloneValue(value)
		}
		state.attrs = attrs
		in.states[alias] = state
	}
	for _, binding := range variables {
		in.vars[binding.Name] = cloneValue(binding.Value)
	}
	return in, nil
}

func validIdentifier(name string) error {
	if name == "" || len(name) > 128 || !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z') || name[0] == '_') {
		return ErrInvalidInput
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return ErrInvalidInput
		}
	}
	return nil
}

func reservedName(name string) bool {
	if functionName(name) != builtinInvalid {
		return true
	}
	switch name {
	case "none", "None", "true", "True", "false", "False":
		return true
	default:
		return false
	}
}

func (v Value) Type() string {
	switch v.kind {
	case valueUndefined:
		return "undefined"
	case valueNone:
		return "none"
	case valueBool:
		return "bool"
	case valueInt:
		return "int"
	case valueFloat:
		return "float"
	case valueString:
		return "str"
	case valueList:
		return "list"
	case valueObject:
		return "object"
	case valueDate:
		return "date"
	case valueDateTime:
		return "datetime"
	case valueDuration:
		return "duration"
	case valueStateRef:
		return "state_ref"
	case valueInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

func (v Value) Kind() (ValueKind, error) {
	if v.kind == valueInvalid || v.kind > valueStateRef {
		return valueInvalid, ErrInvalidInput
	}
	return v.kind, nil
}

func (v Value) Bool() (bool, bool)          { return v.b, v.kind == valueBool }
func (v Value) Int() (int64, bool)          { return v.i, v.kind == valueInt }
func (v Value) Float() (float64, bool)      { return v.f, v.kind == valueFloat }
func (v Value) StringValue() (string, bool) { return v.s, v.kind == valueString }
func (v Value) Items() ([]Value, bool) {
	if v.kind != valueList {
		return nil, false
	}
	return cloneValue(v).list, true
}

func (v Value) Field(name string) (Value, bool) {
	if v.kind != valueObject {
		return Value{}, false
	}
	field, exists := v.obj[name]
	return cloneValue(field), exists
}

func (v Value) GoString() string { return fmt.Sprintf("Value(%s)", v.Type()) }
