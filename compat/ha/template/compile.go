package template

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/parser"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

const (
	maxTokens       = 256
	maxParseDrivers = 32
	maxIRNodes      = 128
	maxIRDepth      = 32
	maxSegments     = 32
)

type operation uint8

const (
	opInvalid operation = iota
	opLiteral
	opName
	opList
	opOr
	opSubtract
	opNegate
	opCall
	opMethod
	opAttribute
	opItem
	opFilter
	opDefined
)

type builtin uint8

const (
	builtinInvalid builtin = iota
	builtinStates
	builtinStateAttr
	builtinIsState
	builtinExpand
	builtinNow
	builtinAsDatetime
	builtinDefault
	builtinFloat
	builtinMap
	builtinList
	builtinDate
	builtinLower
	builtinReplace
)

const entityIDAttribute = "entity_id"

func functionName(name string) builtin {
	switch name {
	case "states":
		return builtinStates
	case "state_attr":
		return builtinStateAttr
	case "is_state":
		return builtinIsState
	case "expand":
		return builtinExpand
	case "now":
		return builtinNow
	case "as_datetime":
		return builtinAsDatetime
	default:
		return builtinInvalid
	}
}

func filterName(name string) builtin {
	switch name {
	case "default":
		return builtinDefault
	case "float":
		return builtinFloat
	case "map":
		return builtinMap
	case "list":
		return builtinList
	default:
		return builtinInvalid
	}
}

func methodName(name string) builtin {
	switch name {
	case "date":
		return builtinDate
	case "lower":
		return builtinLower
	case "replace":
		return builtinReplace
	default:
		return builtinInvalid
	}
}

type expression struct {
	op       operation
	builtin  builtin
	literal  Value
	name     string
	children []expression
	argName  string
}

type segment struct {
	data string
	expr *expression
}

// Program is an immutable admitted program. Its zero value cannot evaluate.
type Program struct{ segments []segment }

func (p Program) Valid() bool { return len(p.segments) != 0 }

// Compile admits only bounded output expressions. No block, include, import,
// macro, loop, general test, reflection, or dynamic function invocation reaches eval.
func Compile(source string) (Program, error) {
	if source == "" || len(source) > maxSourceBytes || !utf8.ValidString(source) {
		return Program{}, ErrInvalidInput
	}
	if strings.Contains(source, "{%") || strings.Contains(source, "{#") {
		return Program{}, fmt.Errorf("%w: blocks and comments", ErrUnsupported)
	}
	cfg := config.New()
	stream := tokens.LexAll(source, cfg) // Synchronous: no lexer goroutine survives rejection.
	collected := make([]*tokens.Token, 0, 32)
	drivers, nesting := 0, 0
	for !stream.End() {
		tok := stream.Current()
		if tok == nil || len(collected) >= maxTokens {
			return Program{}, ErrBudget
		}
		switch tok.Type {
		case tokens.BlockBegin, tokens.BlockEnd, tokens.CommentBegin, tokens.CommentEnd,
			tokens.RawBegin, tokens.RawEnd, tokens.LinecontrolStructureBegin, tokens.LinecontrolStructureEnd,
			tokens.LinecommentBegin, tokens.LinecommentEnd:
			return Program{}, fmt.Errorf("%w: template directive", ErrUnsupported)
		case tokens.LeftParenthesis, tokens.LeftBracket, tokens.LeftBrace:
			nesting++
			drivers++
		case tokens.RightParenthesis, tokens.RightBracket, tokens.RightBrace:
			nesting--
			if nesting < 0 {
				return Program{}, ErrInvalidInput
			}
		case tokens.Dot, tokens.Pipe, tokens.Or, tokens.And, tokens.Not, tokens.Is,
			tokens.Subtraction, tokens.Addition:
			drivers++
		}
		if drivers > maxParseDrivers || nesting > maxParseDrivers {
			return Program{}, ErrBudget
		}
		collected = append(collected, tok)
		stream.Next()
	}
	if stream.IsError() || nesting != 0 {
		return Program{}, ErrInvalidInput
	}
	collected = append(collected, stream.Current())
	parsed, err := parser.NewParser("admitted", tokens.NewStream(collected), cfg, nil, nil).Parse()
	if err != nil {
		return Program{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(parsed.Nodes) == 0 || len(parsed.Nodes) > maxSegments {
		return Program{}, ErrBudget
	}
	// Gonja also constructs TestExpression for comparisons and "in". Its AST
	// discards the operator, so admission must retain the exact lexical "is".
	definedTokens := make(map[*tokens.Token]struct{})
	for i := 1; i < len(collected); i++ {
		if collected[i-1].Type == tokens.Is && collected[i].Type == tokens.Name && collected[i].Val == "defined" {
			definedTokens[collected[i]] = struct{}{}
		}
	}
	compiler := compiler{definedTokens: definedTokens}
	program := Program{segments: make([]segment, 0, len(parsed.Nodes))}
	for _, node := range parsed.Nodes {
		switch n := node.(type) {
		case *nodes.Data:
			program.segments = append(program.segments, segment{data: n.Data.Val})
		case *nodes.Output:
			if n.Condition != nil || n.Alternative != nil {
				return Program{}, fmt.Errorf("%w: conditional output", ErrUnsupported)
			}
			e, err := compiler.convert(n.Expression, 0)
			if err != nil {
				return Program{}, err
			}
			program.segments = append(program.segments, segment{expr: &e})
		default:
			return Program{}, fmt.Errorf("%w: %T", ErrUnsupported, n)
		}
	}
	return program, nil
}

type compiler struct {
	count         int
	definedTokens map[*tokens.Token]struct{}
}

func definedPath(x expression) bool {
	switch x.op {
	case opName:
		return true
	case opAttribute, opItem:
		return len(x.children) == 1 && definedPath(x.children[0])
	case opInvalid, opLiteral, opList, opOr, opSubtract, opNegate, opCall, opMethod, opFilter, opDefined:
		return false
	}
	return false
}

func (c *compiler) convert(node nodes.Node, depth int) (expression, error) {
	c.count++
	if c.count > maxIRNodes || depth > maxIRDepth {
		return expression{}, ErrBudget
	}
	child := func(n nodes.Node) (expression, error) { return c.convert(n, depth+1) }
	switch n := node.(type) {
	case *nodes.String:
		v, err := String(n.Val)
		return expression{op: opLiteral, literal: v}, err
	case *nodes.Integer:
		return expression{op: opLiteral, literal: Int(int64(n.Val))}, nil
	case *nodes.Float:
		v, err := Float(n.Val)
		return expression{op: opLiteral, literal: v}, err
	case *nodes.Bool:
		return expression{op: opLiteral, literal: Bool(n.Val)}, nil
	case *nodes.None:
		return expression{op: opLiteral, literal: None()}, nil
	case *nodes.Name:
		name := n.Name.Val
		switch name {
		case "none", "None":
			return expression{op: opLiteral, literal: None()}, nil
		case "true", "True":
			return expression{op: opLiteral, literal: Bool(true)}, nil
		case "false", "False":
			return expression{op: opLiteral, literal: Bool(false)}, nil
		default:
			if err := validIdentifier(name); err != nil {
				return expression{}, err
			}
			return expression{op: opName, name: name}, nil
		}
	case *nodes.TestExpression:
		if n.Test == nil || n.Test.Name != "defined" || len(n.Test.Args) != 0 || len(n.Test.Kwargs) != 0 {
			return expression{}, fmt.Errorf("%w: test", ErrUnsupported)
		}
		if _, ok := c.definedTokens[n.Test.Token]; !ok {
			return expression{}, fmt.Errorf("%w: test operator", ErrUnsupported)
		}
		operand, err := child(n.Expression)
		if err != nil {
			return expression{}, err
		}
		if !definedPath(operand) {
			return expression{}, fmt.Errorf("%w: defined operand", ErrUnsupported)
		}
		return expression{op: opDefined, children: []expression{operand}}, nil
	case *nodes.List:
		if len(n.Val) > maxItems {
			return expression{}, ErrBudget
		}
		e := expression{op: opList, children: make([]expression, 0, len(n.Val))}
		for _, item := range n.Val {
			converted, err := child(item)
			if err != nil {
				return expression{}, err
			}
			e.children = append(e.children, converted)
		}
		return e, nil
	case *nodes.BinaryExpression:
		var op operation
		switch n.Operator.Token.Type {
		case tokens.Or:
			op = opOr
		case tokens.Subtraction:
			op = opSubtract
		default:
			return expression{}, fmt.Errorf("%w: binary operator %s", ErrUnsupported, n.Operator.Token.Val)
		}
		left, err := child(n.Left)
		if err != nil {
			return expression{}, err
		}
		right, err := child(n.Right)
		return expression{op: op, children: []expression{left, right}}, err
	case *nodes.UnaryExpression:
		if !n.Negative {
			return expression{}, fmt.Errorf("%w: unary operator", ErrUnsupported)
		}
		term, err := child(n.Term)
		return expression{op: opNegate, children: []expression{term}}, err
	case *nodes.FilteredExpression:
		base, err := child(n.Expression)
		if err != nil {
			return expression{}, err
		}
		for _, filter := range n.Filters {
			// Each filter adds an IR wrapper beyond the parsed base expression.
			c.count++
			if c.count > maxIRNodes {
				return expression{}, ErrBudget
			}
			e := expression{op: opFilter, builtin: filterName(filter.Name), children: []expression{base}}
			switch e.builtin {
			case builtinDefault, builtinFloat:
				if len(filter.Kwargs) != 0 || len(filter.Args) > 1 {
					return expression{}, fmt.Errorf("%w: filter arguments", ErrUnsupported)
				}
			case builtinMap:
				if len(filter.Args) != 0 || len(filter.Kwargs) != 1 {
					return expression{}, fmt.Errorf("%w: map arguments", ErrUnsupported)
				}
				attribute, ok := filter.Kwargs["attribute"].(*nodes.String)
				if !ok || attribute.Val != entityIDAttribute {
					return expression{}, fmt.Errorf("%w: map attribute", ErrUnsupported)
				}
				e.argName = attribute.Val
			case builtinList:
				if len(filter.Args) != 0 || len(filter.Kwargs) != 0 {
					return expression{}, fmt.Errorf("%w: list arguments", ErrUnsupported)
				}
			default:
				return expression{}, fmt.Errorf("%w: filter %s", ErrUnsupported, filter.Name)
			}
			for _, arg := range filter.Args {
				converted, err := child(arg)
				if err != nil {
					return expression{}, err
				}
				e.children = append(e.children, converted)
			}
			base = e
		}
		return base, nil
	case *nodes.Call:
		if len(n.Kwargs) != 0 || len(n.Args) > 3 {
			return expression{}, fmt.Errorf("%w: call arguments", ErrUnsupported)
		}
		e := expression{children: make([]expression, 0, len(n.Args)+1)}
		switch fn := n.Func.(type) {
		case *nodes.Name:
			e.op, e.builtin = opCall, functionName(fn.Name.Val)
			if e.builtin == builtinInvalid {
				return expression{}, fmt.Errorf("%w: function %s", ErrUnsupported, fn.Name.Val)
			}
		case *nodes.GetAttribute:
			e.op, e.builtin = opMethod, methodName(fn.Attribute)
			if e.builtin == builtinInvalid {
				return expression{}, fmt.Errorf("%w: method %s", ErrUnsupported, fn.Attribute)
			}
			receiver, err := child(fn.Node)
			if err != nil {
				return expression{}, err
			}
			e.children = append(e.children, receiver)
		default:
			return expression{}, fmt.Errorf("%w: dynamic call", ErrUnsupported)
		}
		for _, arg := range n.Args {
			converted, err := child(arg)
			if err != nil {
				return expression{}, err
			}
			e.children = append(e.children, converted)
		}
		return e, nil
	case *nodes.GetAttribute:
		if validKey(n.Attribute) != nil || strings.HasPrefix(n.Attribute, "_") {
			return expression{}, ErrInvalidInput
		}
		base, err := child(n.Node)
		return expression{op: opAttribute, name: n.Attribute, children: []expression{base}}, err
	case *nodes.GetItem:
		key, ok := n.Arg.(*nodes.String)
		if !ok || validKey(key.Val) != nil || strings.HasPrefix(key.Val, "_") {
			return expression{}, fmt.Errorf("%w: item key", ErrUnsupported)
		}
		base, err := child(n.Node)
		return expression{op: opItem, name: key.Val, children: []expression{base}}, err
	default:
		return expression{}, fmt.Errorf("%w: node %T", ErrUnsupported, n)
	}
}
