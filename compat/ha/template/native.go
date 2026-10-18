package template

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// nativeFromText is the admitted scalar/list part of HA parse_result=True.
// Conversion depends on the complete rendered text, including literal data.
// Unsupported Python literal forms remain strings; numeric overflow is refused.
func nativeFromText(text string, _ Value, _ bool) (Value, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "None" {
		return None(), nil
	}
	if trimmed == "True" {
		return Bool(true), nil
	}
	if trimmed == "False" {
		return Bool(false), nil
	}
	if trimmed == "[]" {
		return Value{kind: valueList, list: []Value{}}, nil
	}
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") ||
		strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		return Value{}, fmt.Errorf("%w: dict, set or tuple native value", ErrUnsupported)
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		p := nativeParser{source: trimmed}
		value, err := p.parseList(0)
		if err == nil && p.pos == len(trimmed) {
			return value, nil
		}
		if errors.Is(err, ErrBudget) || errors.Is(err, ErrUnsupported) {
			return Value{}, err
		}
	}
	if canonicalInteger(trimmed) {
		integer, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("%w: integer outside int64", ErrUnsupported)
		}
		return Int(integer), nil
	}
	if canonicalFloat(trimmed) {
		number, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return Value{}, fmt.Errorf("%w: float outside finite range", ErrUnsupported)
		}
		return Float(number)
	}
	value, err := String(text)
	if err != nil {
		return Value{}, ErrBudget
	}
	return value, nil
}

func canonicalInteger(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func canonicalFloat(s string) bool {
	if !strings.Contains(s, ".") || len(s) > 128 {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	digits := 0
	dot := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
			continue
		}
		if r == '.' {
			dot++
			continue
		}
		return false
	}
	if digits == 0 || dot != 1 {
		return false
	}
	beforeDot := strings.SplitN(s, ".", 2)[0]
	if len(beforeDot) > 1 && beforeDot[0] == '0' {
		return false
	}
	return true
}

// Python literal_eval accepts scientific notation inside a list even though
// HA's scalar numeric conversion deliberately leaves it as text.
func canonicalScientific(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	index := strings.IndexAny(s, "eE")
	if index < 1 || index == len(s)-1 || strings.ContainsAny(s[index+1:], "eE") {
		return false
	}
	mantissa, exponent := s[:index], s[index+1:]
	if !canonicalInteger(mantissa) && !canonicalFloat(mantissa) {
		return false
	}
	if exponent[0] == '-' || exponent[0] == '+' {
		exponent = exponent[1:]
	}
	if exponent == "" {
		return false
	}
	for i := range exponent {
		if exponent[i] < '0' || exponent[i] > '9' {
			return false
		}
	}
	return true
}

type nativeParser struct {
	source string
	pos    int
	items  int
}

func (p *nativeParser) skipSpaces() {
	for p.pos < len(p.source) && p.source[p.pos] == ' ' {
		p.pos++
	}
}

func (p *nativeParser) parseList(depth int) (Value, error) {
	if depth > maxValueDepth || p.pos >= len(p.source) || p.source[p.pos] != '[' {
		return Value{}, ErrBudget
	}
	p.pos++
	items := make([]Value, 0)
	p.skipSpaces()
	if p.pos < len(p.source) && p.source[p.pos] == ']' {
		p.pos++
		return Value{kind: valueList, list: items}, nil
	}
	for {
		if p.items >= maxItems {
			return Value{}, ErrBudget
		}
		p.items++
		p.skipSpaces()
		var value Value
		var err error
		if p.pos >= len(p.source) {
			return Value{}, ErrInvalidInput
		}
		switch p.source[p.pos] {
		case '[':
			value, err = p.parseList(depth + 1)
		case '\'', '"':
			value, err = p.parseQuoted()
		default:
			start := p.pos
			for p.pos < len(p.source) && p.source[p.pos] != ',' && p.source[p.pos] != ']' {
				p.pos++
			}
			token := strings.TrimSpace(p.source[start:p.pos])
			if canonicalInteger(token) {
				var integer int64
				integer, err = strconv.ParseInt(token, 10, 64)
				if err != nil {
					return Value{}, fmt.Errorf("%w: list integer outside int64", ErrUnsupported)
				}
				value = Int(integer)
			} else if canonicalFloat(token) || canonicalScientific(token) {
				var number float64
				number, err = strconv.ParseFloat(token, 64)
				if err != nil {
					return Value{}, fmt.Errorf("%w: list float outside finite range", ErrUnsupported)
				}
				value, err = Float(number)
				if err != nil {
					return Value{}, fmt.Errorf("%w: list float outside finite range", ErrUnsupported)
				}
			} else {
				switch token {
				case "None":
					value = None()
				case "True":
					value = Bool(true)
				case "False":
					value = Bool(false)
				default:
					return Value{}, ErrInvalidInput
				}
			}
		}
		if err != nil {
			return Value{}, ErrInvalidInput
		}
		items = append(items, value)
		p.skipSpaces()
		if p.pos >= len(p.source) {
			return Value{}, ErrInvalidInput
		}
		if p.source[p.pos] == ']' {
			p.pos++
			return Value{kind: valueList, list: items}, nil
		}
		if p.source[p.pos] != ',' {
			return Value{}, ErrInvalidInput
		}
		p.pos++
	}
}

func (p *nativeParser) parseQuoted() (Value, error) {
	if p.pos >= len(p.source) {
		return Value{}, ErrInvalidInput
	}
	quote := p.source[p.pos]
	p.pos++
	var out strings.Builder
	for p.pos < len(p.source) {
		c := p.source[p.pos]
		p.pos++
		if c == quote {
			return String(out.String())
		}
		if c == '\\' {
			if p.pos >= len(p.source) {
				return Value{}, ErrInvalidInput
			}
			c = p.source[p.pos]
			p.pos++
			switch c {
			case '\\', '\'', '"':
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'x', 'u', 'U':
				digits := 2
				if c == 'u' {
					digits = 4
				} else if c == 'U' {
					digits = 8
				}
				if p.pos+digits > len(p.source) {
					return Value{}, ErrInvalidInput
				}
				codepoint, err := strconv.ParseUint(p.source[p.pos:p.pos+digits], 16, 32)
				if err != nil || !utf8.ValidRune(rune(codepoint)) {
					return Value{}, ErrInvalidInput
				}
				p.pos += digits
				if out.Len()+utf8.RuneLen(rune(codepoint)) > maxStringBytes {
					return Value{}, ErrBudget
				}
				out.WriteRune(rune(codepoint))
				continue
			default:
				return Value{}, ErrInvalidInput
			}
		}
		if out.Len()+1 > maxStringBytes {
			return Value{}, ErrBudget
		}
		out.WriteByte(c)
	}
	return Value{}, ErrInvalidInput
}
