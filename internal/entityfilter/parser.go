package entityfilter

import "github.com/damione1/Homeward/internal/apitypes"

// Filter is immutable admitted syntax. Its zero value matches nothing.
// Digest canonicalizes whitespace, quoting, implicit AND, associative grouping
// and commutative operand order; it is not a general boolean-equivalence proof.
type Filter struct {
	root   expression
	digest [32]byte
	valid  bool
	source string
}

func (f Filter) Source() string { return f.source }
func (f Filter) Valid() bool    { return f.valid && f.root != nil }
func (f Filter) Match(candidate Candidate) bool {
	return f.Valid() && candidate.valid && f.root.matches(candidate)
}
func (f Filter) Digest() ([32]byte, error) {
	if !f.Valid() {
		return [32]byte{}, reject(Syntax, 0)
	}
	return f.digest, nil
}
func Parse(input string) (Filter, error) {
	tokens, err := lex(input)
	if err != nil {
		return Filter{}, err
	}
	parser := parser{tokens: tokens}
	var root expression
	if parser.peek().kind == end {
		root = allExpression{}
	} else {
		root, err = parser.expression(0)
		if err != nil {
			return Filter{}, err
		}
	}
	if parser.peek().kind != end {
		return Filter{}, reject(Syntax, parser.peek().position)
	}
	return Filter{root: root, digest: root.digest(), valid: true, source: input}, nil
}

type parser struct {
	tokens       []token
	index, nodes int
}

func (p *parser) peek() token { return p.tokens[p.index] }
func (p *parser) take() token {
	value := p.peek()
	if value.kind != end {
		p.index++
	}
	return value
}
func (p *parser) node(value expression, position int) (expression, error) {
	p.nodes++
	if p.nodes > MaxNodes {
		return nil, reject(NodeLimit, position)
	}
	return value, nil
}
func (p *parser) expression(depth int) (expression, error) {
	first, err := p.factor(depth)
	if err != nil {
		return nil, err
	}
	for {
		next := p.peek()
		if next.kind == and {
			p.take()
			if !next.space || !p.peek().space {
				return nil, reject(Syntax, next.position)
			}
		} else if startsTerm(next.kind) {
			if !next.space {
				return nil, reject(Syntax, next.position)
			}
		} else {
			break
		}
		second, err := p.factor(depth)
		if err != nil {
			return nil, err
		}
		first, err = p.node(combine(conjunction, first, second), next.position)
		if err != nil {
			return nil, err
		}
	}
	return first, nil
}
func (p *parser) factor(depth int) (expression, error) {
	first, err := p.term(depth)
	if err != nil {
		return nil, err
	}
	for p.peek().kind == or {
		operator := p.take()
		if !operator.space || !p.peek().space {
			return nil, reject(Syntax, operator.position)
		}
		second, err := p.term(depth)
		if err != nil {
			return nil, err
		}
		first, err = p.node(combine(disjunction, first, second), operator.position)
		if err != nil {
			return nil, err
		}
	}
	return first, nil
}
func startsTerm(kind tokenKind) bool {
	switch kind {
	case atom, quoted, leftParen, minus, not:
		return true
	default:
		return false
	}
}
func (p *parser) term(depth int) (expression, error) {
	operator := p.peek()
	if operator.kind != not && operator.kind != minus {
		return p.simple(depth)
	}
	p.take()
	if operator.kind == not && !p.peek().space {
		return nil, reject(Syntax, operator.position)
	}
	if depth == MaxDepth {
		return nil, reject(DepthLimit, operator.position)
	}
	inner, err := p.simple(depth + 1)
	if err != nil {
		return nil, err
	}
	return p.node(negatedExpression{inner}, operator.position)
}
func (p *parser) simple(depth int) (expression, error) {
	if p.peek().kind == leftParen {
		start := p.take()
		if depth == MaxDepth {
			return nil, reject(DepthLimit, start.position)
		}
		inner, err := p.expression(depth + 1)
		if err != nil {
			return nil, err
		}
		if p.peek().kind != rightParen {
			return nil, reject(Syntax, p.peek().position)
		}
		p.take()
		return inner, nil
	}
	first := p.take()
	if first.kind != atom && first.kind != quoted {
		return nil, reject(Syntax, first.position)
	}
	if p.peek().kind == leftParen && first.kind == atom {
		value, err := p.function(first)
		if err != nil {
			return nil, err
		}
		if isComparator(p.peek().kind) {
			return nil, reject(UnsupportedOperator, p.peek().position)
		}
		return p.node(value, first.position)
	}
	if p.peek().kind == dot {
		return nil, reject(UnknownField, first.position)
	}
	if !isComparator(p.peek().kind) {
		pattern, ok := newPattern(first.value)
		if !ok {
			return nil, reject(InvalidValue, first.position)
		}
		return p.node(literalExpression{pattern}, first.position)
	}
	if first.kind != atom {
		return nil, reject(UnknownField, first.position)
	}
	var field stringField
	switch first.value {
	case "name":
		field = nameField
	case "display_name":
		field = displayNameField
	default:
		return nil, reject(UnknownField, first.position)
	}
	operator := p.take()
	literal := p.take()
	if literal.kind != atom && literal.kind != quoted {
		return nil, reject(InvalidValue, literal.position)
	}
	comparison := comparisonFor(operator.kind)
	if comparison == contains && literal.value == "*" {
		return p.node(stringExpression{field: field, operator: present}, first.position)
	}
	pattern, ok := newPattern(literal.value)
	if !ok {
		return nil, reject(InvalidValue, literal.position)
	}
	if comparison == contains && pattern.kind == anyPattern {
		return p.node(stringExpression{field: field, operator: present}, first.position)
	}
	if comparison >= lt && comparison <= ge && pattern.kind != exactPattern {
		return nil, reject(UnsupportedOperator, literal.position)
	}
	return p.node(stringExpression{field, comparison, pattern}, first.position)
}
func (p *parser) function(name token) (expression, error) {
	switch name.value {
	case "device", "trait", "availability":
	default:
		return nil, reject(UnknownFunction, name.position)
	}
	p.take() // Opening parenthesis.
	argument := p.take()
	if argument.kind != atom && argument.kind != quoted {
		return nil, reject(InvalidValue, argument.position)
	}
	if p.peek().kind != rightParen {
		return nil, reject(Syntax, p.peek().position)
	}
	p.take()
	switch name.value {
	case "device":
		device, err := apitypes.ParseDeviceName(argument.value)
		if err != nil {
			return nil, reject(InvalidValue, argument.position)
		}
		return deviceExpression{device}, nil
	case "trait":
		trait, err := parseTrait(argument.value)
		if err != nil {
			return nil, reject(UnknownTrait, argument.position)
		}
		return traitExpression{trait}, nil
	case "availability":
		value, ok := parseAvailability(argument.value)
		if !ok {
			return nil, reject(InvalidValue, argument.position)
		}
		return availabilityExpression{value}, nil
	default:
		return nil, reject(UnknownFunction, name.position)
	}
}
func isComparator(kind tokenKind) bool {
	switch kind {
	case equal, unequal, less, lessEqual, greater, greaterEqual, has:
		return true
	default:
		return false
	}
}
func comparisonFor(kind tokenKind) comparison {
	switch kind {
	case equal:
		return eq
	case unequal:
		return ne
	case less:
		return lt
	case lessEqual:
		return le
	case greater:
		return gt
	case greaterEqual:
		return ge
	case has:
		return contains
	default:
		return 0
	}
}
