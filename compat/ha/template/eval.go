package template

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/damione1/Homeward/internal/domain"
)

const (
	defaultSteps  = 16384
	defaultOutput = 8192
)

// Limits are charged independently for each evaluation. The zero value is
// invalid; callers must choose defaults or an explicit bounded limit.
type Limits struct{ steps, output int }

func DefaultLimits() Limits { return Limits{steps: defaultSteps, output: defaultOutput} }

func NewLimits(steps, output int) (Limits, error) {
	if steps < 1 || steps > defaultSteps || output < 1 || output > defaultOutput {
		return Limits{}, ErrInvalidInput
	}
	return Limits{steps: steps, output: output}, nil
}

// Result keeps HA's rendered text and admitted native value distinct.
type Result struct {
	text   string
	native Value
}

func (r Result) Text() string  { return r.text }
func (r Result) Native() Value { return cloneValue(r.native) }

type evaluator struct {
	ctx    context.Context
	in     Inputs
	steps  int
	output int
}

func (e *evaluator) charge(cost int) error {
	if err := e.ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	if cost < 0 || cost > e.steps {
		return ErrBudget
	}
	e.steps -= cost
	return nil
}

// Evaluate runs only admitted IR, synchronously. Each primitive has a bounded
// operand; a budget failure stops the work before this call returns.
func Evaluate(ctx context.Context, program Program, input Inputs, limits Limits) (Result, error) {
	if nilInterface(ctx) || len(program.segments) == 0 || limits.steps < 1 || limits.steps > defaultSteps || limits.output < 1 || limits.output > defaultOutput || input.zone == nil || input.now.IsZero() {
		return Result{}, ErrInvalidInput
	}
	e := evaluator{ctx: ctx, in: input, steps: limits.steps, output: limits.output}
	var out strings.Builder
	var sole Value
	soleExpression := len(program.segments) == 1 && program.segments[0].expr != nil
	for _, segment := range program.segments {
		if err := e.charge(1); err != nil {
			return Result{}, err
		}
		piece := segment.data
		if segment.expr != nil {
			value, err := e.eval(*segment.expr)
			if err != nil {
				return Result{}, err
			}
			if soleExpression {
				sole = value
			}
			piece, err = e.render(value)
			if err != nil {
				return Result{}, err
			}
		}
		if len(piece) > e.output-out.Len() {
			return Result{}, ErrBudget
		}
		out.WriteString(piece)
	}
	text := strings.TrimSpace(out.String())
	if err := e.charge(len(text)); err != nil {
		return Result{}, err
	}
	native, err := nativeFromText(text, sole, soleExpression)
	if err != nil {
		return Result{}, err
	}
	return Result{text: text, native: native}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (e *evaluator) eval(x expression) (Value, error) {
	if err := e.charge(1); err != nil {
		return Value{}, err
	}
	switch x.op {
	case opLiteral:
		return x.literal, nil
	case opName:
		if value, ok := e.in.vars[x.name]; ok {
			return value, nil
		}
		return Value{kind: valueUndefined}, nil
	case opList:
		items := make([]Value, 0, len(x.children))
		for _, child := range x.children {
			if err := e.charge(1); err != nil {
				return Value{}, err
			}
			value, err := e.eval(child)
			if err != nil {
				return Value{}, err
			}
			items = append(items, value)
		}
		return Value{kind: valueList, list: items}, nil
	case opOr:
		left, err := e.eval(x.children[0])
		if err != nil {
			return Value{}, err
		}
		if truthy(left) {
			return left, nil
		}
		return e.eval(x.children[1])
	case opSubtract:
		left, err := e.eval(x.children[0])
		if err != nil {
			return Value{}, err
		}
		right, err := e.eval(x.children[1])
		if err != nil {
			return Value{}, err
		}
		if left.kind != valueDate || right.kind != valueDate {
			return Value{}, fmt.Errorf("%w: date subtraction", ErrEvaluation)
		}
		// Both are UTC calendar midnights, so DST cannot alter the day count.
		return Value{kind: valueDuration, i: int64(left.time.Sub(right.time) / (24 * time.Hour))}, nil
	case opNegate:
		value, err := e.eval(x.children[0])
		if err != nil {
			return Value{}, err
		}
		switch value.kind {
		case valueInt:
			if value.i == math.MinInt64 {
				return Value{}, ErrEvaluation
			}
			return Int(-value.i), nil
		case valueFloat:
			return Float(-value.f)
		default:
			return Value{}, fmt.Errorf("%w: unary minus", ErrEvaluation)
		}
	case opCall:
		return e.call(x)
	case opMethod:
		return e.method(x)
	case opAttribute, opItem:
		base, err := e.eval(x.children[0])
		if err != nil {
			return Value{}, err
		}
		switch base.kind {
		case valueObject:
			if value, exists := base.obj[x.name]; exists {
				return value, nil
			}
			return Value{kind: valueUndefined}, nil
		case valueDuration:
			if x.op == opAttribute && x.name == "days" {
				return Int(base.i), nil
			}
		case valueUndefined:
			return Value{kind: valueUndefined}, nil
		}
		return Value{}, fmt.Errorf("%w: attribute %s", ErrEvaluation, x.name)
	case opFilter:
		return e.filter(x)
	case opDefined:
		value, err := e.definedPath(x.children[0])
		if err != nil {
			return Value{}, err
		}
		return Bool(value.kind != valueUndefined), nil
	case opInvalid:
		return Value{}, ErrInvalidInput
	default:
		return Value{}, ErrInvalidInput
	}
}

// definedPath follows Jinja's one-step lookup rule only while evaluating an
// admitted "is defined" operand. A missing final lookup is Undefined; another
// lookup through that Undefined is an error. Ordinary access keeps its profile.
func (e *evaluator) definedPath(x expression) (Value, error) {
	if err := e.charge(1); err != nil {
		return Value{}, err
	}
	switch x.op {
	case opName:
		if value, ok := e.in.vars[x.name]; ok {
			return value, nil
		}
		return Value{kind: valueUndefined}, nil
	case opAttribute, opItem:
		base, err := e.definedPath(x.children[0])
		if err != nil {
			return Value{}, err
		}
		switch base.kind {
		case valueObject:
			// Jinja dot lookup sees dict methods before keys; item lookup sees
			// keys first. Refuse method names where that Python fallback matters.
			if x.op == opAttribute && pythonMappingMethod(x.name) {
				return Value{}, fmt.Errorf("%w: mapping method %s", ErrUnsupported, x.name)
			}
			if value, exists := base.obj[x.name]; exists {
				return value, nil
			}
			if x.op == opItem && pythonMappingMethod(x.name) {
				return Value{}, fmt.Errorf("%w: mapping method %s", ErrUnsupported, x.name)
			}
			return Value{kind: valueUndefined}, nil
		case valueUndefined:
			return Value{}, fmt.Errorf("%w: attribute %s on undefined", ErrEvaluation, x.name)
		case valueNone:
			return Value{kind: valueUndefined}, nil
		case valueDuration:
			if x.op == opAttribute && x.name == "days" {
				return Int(base.i), nil
			}
			return Value{}, fmt.Errorf("%w: duration attribute %s", ErrUnsupported, x.name)
		case valueBool, valueInt, valueFloat, valueString, valueList,
			valueDate, valueDateTime, valueStateRef:
			return Value{}, fmt.Errorf("%w: scalar attribute %s", ErrUnsupported, x.name)
		case valueInvalid:
			return Value{}, ErrInvalidInput
		}
	case opInvalid, opLiteral, opList, opOr, opSubtract, opNegate, opCall,
		opMethod, opFilter, opDefined:
		return Value{}, ErrInvalidInput
	}
	return Value{}, ErrInvalidInput
}

// Python mappings expose methods through dot syntax before a same-named key,
// and through item syntax only when the key is absent. This object profile has
// no callable values, so those cases cannot be represented by a Boolean result.
func pythonMappingMethod(name string) bool {
	switch name {
	case "clear", "copy", "fromkeys", "get", "items", "keys", "pop",
		"popitem", "setdefault", "update", "values":
		return true
	}
	return false
}

func truthy(v Value) bool {
	switch v.kind {
	case valueInvalid, valueUndefined, valueNone:
		return false
	case valueBool:
		return v.b
	case valueInt, valueDuration:
		return v.i != 0
	case valueFloat:
		return v.f != 0
	case valueString, valueStateRef:
		return v.s != ""
	case valueList:
		return len(v.list) != 0
	case valueObject:
		return len(v.obj) != 0
	case valueDate, valueDateTime:
		return true
	default:
		return false
	}
}

func (e *evaluator) args(children []expression) ([]Value, error) {
	args := make([]Value, 0, len(children))
	for _, child := range children {
		value, err := e.eval(child)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	return args, nil
}

func stringArg(v Value) (string, error) {
	if v.kind != valueString {
		return "", fmt.Errorf("%w: expected string", ErrEvaluation)
	}
	return v.s, nil
}

func aliasArg(v Value) (string, error) {
	s, err := stringArg(v)
	if err != nil {
		return "", err
	}
	alias, err := domain.ParseHAAlias(s)
	if err != nil {
		return "", fmt.Errorf("%w: entity alias", ErrEvaluation)
	}
	return alias.Value()
}

func (e *evaluator) call(x expression) (Value, error) {
	args, err := e.args(x.children)
	if err != nil {
		return Value{}, err
	}
	switch x.builtin {
	case builtinNow:
		if len(args) != 0 {
			return Value{}, ErrEvaluation
		}
		return Value{kind: valueDateTime, time: e.in.now.In(e.in.zone)}, nil
	case builtinAsDatetime:
		if len(args) != 1 {
			return Value{}, ErrEvaluation
		}
		text, err := stringArg(args[0])
		if err != nil {
			return Value{}, err
		}
		if len(text) > 64 {
			return Value{}, ErrBudget
		}
		var parsed time.Time
		if parsed, err = time.Parse("2006-01-02", text); err != nil {
			parsed, err = time.Parse(time.RFC3339, text)
		}
		if err != nil || parsed.Year() < 1900 || parsed.Year() > 2100 {
			return Value{}, fmt.Errorf("%w: as_datetime input", ErrEvaluation)
		}
		return Value{kind: valueDateTime, time: parsed, b: len(text) == len("2006-01-02")}, nil
	case builtinStates, builtinStateAttr, builtinIsState, builtinExpand:
		if (x.builtin == builtinStates || x.builtin == builtinExpand) && len(args) != 1 || (x.builtin == builtinStateAttr || x.builtin == builtinIsState) && len(args) != 2 {
			return Value{}, ErrEvaluation
		}
		alias, err := aliasArg(args[0])
		if err != nil {
			return Value{}, err
		}
		state, exists := e.in.states[alias]
		switch x.builtin {
		case builtinStates:
			if !exists {
				return Value{kind: valueString, s: "unknown"}, nil
			}
			return Value{kind: valueString, s: state.text}, nil
		case builtinStateAttr:
			key, err := stringArg(args[1])
			if err != nil || validKey(key) != nil {
				return Value{}, ErrEvaluation
			}
			if !exists {
				return None(), nil
			}
			if value, ok := state.attrs[key]; ok {
				return value, nil
			}
			return None(), nil
		case builtinIsState:
			want, err := stringArg(args[1])
			if err != nil {
				return Value{}, err
			}
			return Bool(exists && state.text == want), nil
		case builtinExpand:
			ids := make(map[string]struct{})
			if err := e.expand(alias, ids, 0); err != nil {
				return Value{}, err
			}
			ordered := make([]string, 0, len(ids))
			for id := range ids {
				ordered = append(ordered, id)
			}
			sort.Strings(ordered)
			items := make([]Value, len(ordered))
			for i, id := range ordered {
				items[i] = Value{kind: valueStateRef, s: id}
			}
			return Value{kind: valueList, list: items}, nil
		}
	}
	return Value{}, ErrUnsupported
}

func (e *evaluator) expand(alias string, ids map[string]struct{}, depth int) error {
	if depth > maxValueDepth || len(ids) >= maxItems {
		return ErrBudget
	}
	if depth > 0 && strings.HasPrefix(alias, "group.") {
		return fmt.Errorf("%w: nested group expansion", ErrUnsupported)
	}
	if err := e.charge(1); err != nil {
		return err
	}
	state, exists := e.in.states[alias]
	if !exists {
		return nil
	}
	if _, seen := ids[alias]; seen {
		return nil
	}
	ids[alias] = struct{}{}
	if !strings.HasPrefix(alias, "group.") {
		return nil
	}
	members, ok := state.attrs[entityIDAttribute]
	if !ok || members.kind != valueList {
		return fmt.Errorf("%w: group membership", ErrEvaluation)
	}
	delete(ids, alias)
	for _, member := range members.list {
		if err := e.charge(1); err != nil {
			return err
		}
		id, err := aliasArg(member)
		if err != nil {
			return err
		}
		if err := e.expand(id, ids, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (e *evaluator) method(x expression) (Value, error) {
	args, err := e.args(x.children)
	if err != nil || len(args) == 0 {
		return Value{}, ErrEvaluation
	}
	receiver := args[0]
	args = args[1:]
	switch x.builtin {
	case builtinDate:
		if len(args) != 0 || receiver.kind != valueDateTime {
			return Value{}, ErrEvaluation
		}
		y, m, d := receiver.time.Date()
		return Value{kind: valueDate, time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}, nil
	case builtinLower:
		if len(args) != 0 || receiver.kind != valueString {
			return Value{}, ErrEvaluation
		}
		for _, r := range receiver.s {
			if r > 127 {
				return Value{}, fmt.Errorf("%w: non-ASCII lower", ErrUnsupported)
			}
		}
		if err := e.charge(len(receiver.s)); err != nil {
			return Value{}, err
		}
		return String(strings.ToLower(receiver.s))
	case builtinReplace:
		if receiver.kind != valueString || len(args) < 2 || len(args) > 3 {
			return Value{}, ErrEvaluation
		}
		old, err := stringArg(args[0])
		if err != nil || old == "" {
			return Value{}, ErrEvaluation
		}
		newText, err := stringArg(args[1])
		if err != nil {
			return Value{}, err
		}
		count := -1
		if len(args) == 3 {
			if args[2].kind != valueInt || args[2].i < -1 || args[2].i > maxItems {
				return Value{}, ErrEvaluation
			}
			count = int(args[2].i)
		}
		if err := e.charge(len(receiver.s) + len(old) + len(newText)); err != nil {
			return Value{}, err
		}
		replacements := strings.Count(receiver.s, old)
		if count >= 0 && replacements > count {
			replacements = count
		}
		newSize := int64(len(receiver.s)) + int64(replacements)*int64(len(newText)-len(old))
		if newSize > maxStringBytes || newSize > int64(e.output) {
			return Value{}, ErrBudget
		}
		return String(strings.Replace(receiver.s, old, newText, count))
	}
	return Value{}, ErrUnsupported
}

func (e *evaluator) filter(x expression) (Value, error) {
	base, err := e.eval(x.children[0])
	if err != nil {
		return Value{}, err
	}
	switch x.builtin {
	case builtinDefault:
		if base.kind != valueUndefined {
			return base, nil
		}
		if len(x.children) == 1 {
			return Value{kind: valueString}, nil
		}
		return e.eval(x.children[1])
	case builtinFloat:
		if base.kind == valueFloat {
			return base, nil
		}
		if base.kind == valueBool {
			if base.b {
				return Float(1)
			}
			return Float(0)
		}
		if base.kind == valueInt {
			return Float(float64(base.i))
		}
		if base.kind == valueString {
			if err := e.charge(len(base.s)); err != nil {
				return Value{}, err
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(base.s), 64); err == nil {
				return Float(f)
			}
		}
		if len(x.children) == 2 {
			return e.eval(x.children[1])
		}
		return Value{}, fmt.Errorf("%w: float conversion", ErrEvaluation)
	case builtinMap:
		if base.kind != valueList || x.argName != entityIDAttribute {
			return Value{}, ErrEvaluation
		}
		items := make([]Value, 0, len(base.list))
		for _, item := range base.list {
			if err := e.charge(1); err != nil {
				return Value{}, err
			}
			if item.kind != valueStateRef {
				return Value{}, ErrEvaluation
			}
			items = append(items, Value{kind: valueString, s: item.s})
		}
		return Value{kind: valueList, list: items}, nil
	case builtinList:
		if base.kind != valueList {
			return Value{}, ErrEvaluation
		}
		return base, nil
	}
	return Value{}, ErrUnsupported
}

func (e *evaluator) render(value Value) (string, error) {
	if err := e.charge(1); err != nil {
		return "", err
	}
	switch value.kind {
	case valueUndefined:
		return "", nil
	case valueNone:
		return "None", nil
	case valueBool:
		if value.b {
			return "True", nil
		}
		return "False", nil
	case valueInt:
		return strconv.FormatInt(value.i, 10), nil
	case valueFloat:
		magnitude := math.Abs(value.f)
		format := byte('g')
		if magnitude == 0 || magnitude >= 1e-4 && magnitude < 1e16 {
			format = 'f'
		}
		text := strconv.FormatFloat(value.f, format, -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return text, nil
	case valueString:
		if err := e.charge(len(value.s)); err != nil {
			return "", err
		}
		return value.s, nil
	case valueDate:
		return value.time.Format("2006-01-02"), nil
	case valueDateTime:
		if value.time.Nanosecond()%1000 != 0 {
			return "", fmt.Errorf("%w: submicrosecond datetime", ErrUnsupported)
		}
		fraction := ""
		if micros := value.time.Nanosecond() / 1000; micros != 0 {
			fraction = fmt.Sprintf(".%06d", micros)
		}
		if value.b {
			return value.time.Format("2006-01-02 15:04:05") + fraction, nil
		}
		return value.time.Format("2006-01-02 15:04:05") + fraction + value.time.Format("-07:00"), nil
	case valueDuration:
		if value.i == 0 {
			return "0:00:00", nil
		}
		unit := " days, 0:00:00"
		if value.i == -1 || value.i == 1 {
			unit = " day, 0:00:00"
		}
		return strconv.FormatInt(value.i, 10) + unit, nil
	case valueList:
		var out strings.Builder
		out.WriteByte('[')
		for i, item := range value.list {
			if err := e.charge(1); err != nil {
				return "", err
			}
			switch item.kind {
			case valueUndefined, valueDate, valueDateTime, valueDuration:
				return "", fmt.Errorf("%w: list element representation", ErrUnsupported)
			}
			piece := ""
			if i > 0 {
				piece = ", "
			}
			var text string
			var err error
			if item.kind == valueString {
				if err := e.charge(len(item.s)); err != nil {
					return "", err
				}
				text, err = pythonQuote(item.s, e.output-out.Len()-len(piece)-1)
			} else {
				text, err = e.render(item)
			}
			if err != nil {
				return "", err
			}
			if out.Len()+len(piece)+len(text)+1 > e.output {
				return "", ErrBudget
			}
			out.WriteString(piece)
			out.WriteString(text)
		}
		out.WriteByte(']')
		return out.String(), nil
	case valueInvalid, valueObject, valueStateRef:
		return "", fmt.Errorf("%w: unrenderable value", ErrEvaluation)
	default:
		return "", ErrInvalidInput
	}
}

func pythonQuote(s string, limit int) (string, error) {
	if limit < 2 {
		return "", ErrBudget
	}
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = '"'
	}
	var out strings.Builder
	out.Grow(min(len(s)+2, limit))
	out.WriteByte(quote)
	for _, c := range s {
		piece := ""
		switch c {
		case '\\':
			piece = "\\\\"
		case '\n':
			piece = "\\n"
		case '\r':
			piece = "\\r"
		case '\t':
			piece = "\\t"
		default:
			if c == rune(quote) {
				piece = "\\" + string(c)
			} else if c != ' ' && !unicode.IsPrint(c) {
				switch {
				case c <= 0xff:
					piece = fmt.Sprintf("\\x%02x", c)
				case c <= 0xffff:
					piece = fmt.Sprintf("\\u%04x", c)
				default:
					piece = fmt.Sprintf("\\U%08x", c)
				}
			} else {
				piece = string(c)
			}
		}
		if out.Len()+len(piece)+1 > limit {
			return "", ErrBudget
		}
		out.WriteString(piece)
	}
	out.WriteByte(quote)
	return out.String(), nil
}

func IsBudget(err error) bool { return errors.Is(err, ErrBudget) }
