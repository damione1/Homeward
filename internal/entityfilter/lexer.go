package entityfilter

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type tokenKind uint8

const (
	end tokenKind = iota
	atom
	quoted
	leftParen
	rightParen
	comma
	dot
	minus
	and
	or
	not
	equal
	unequal
	less
	lessEqual
	greater
	greaterEqual
	has
)

type token struct {
	kind          tokenKind
	value         string
	position, end int
	space         bool
}

func whitespace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
func lex(input string) ([]token, error) {
	if len(input) > MaxTextBytes {
		return nil, reject(TextLimit, MaxTextBytes)
	}
	if !utf8.ValidString(input) {
		return nil, reject(InvalidValue, 0)
	}
	var result []token
	for i := 0; i < len(input); {
		spaced := false
		for i < len(input) && whitespace(input[i]) {
			spaced = true
			i++
		}
		if i == len(input) {
			break
		}
		if len(result) == MaxTokens {
			return nil, reject(TokenLimit, i)
		}
		start := i
		kind := atom
		value := ""
		switch b := input[i]; b {
		case '(':
			kind = leftParen
			i++
		case ')':
			kind = rightParen
			i++
		case ',':
			kind = comma
			i++
		case '.':
			kind = dot
			i++
		case '-':
			kind = minus
			i++
		case ':':
			kind = has
			i++
		case '=':
			kind = equal
			i++
		case '!':
			i++
			if i == len(input) || input[i] != '=' {
				return nil, reject(Syntax, start)
			}
			i++
			kind = unequal
		case '<', '>':
			i++
			if b == '<' {
				kind = less
			} else {
				kind = greater
			}
			if i < len(input) && input[i] == '=' {
				i++
				if b == '<' {
					kind = lessEqual
				} else {
					kind = greaterEqual
				}
			}
		case '\'', '"':
			parsed, next, err := quotedString(input, i)
			if err != nil {
				return nil, err
			}
			kind = quoted
			value = parsed
			i = next
		case '[', ']', '{', '}':
			return nil, reject(Syntax, start)
		default:
			for i < len(input) && !whitespace(input[i]) && !strings.ContainsRune("().,:=!<>\"'[]{}", rune(input[i])) {
				if input[i] < 0x20 || input[i] == 0x7f {
					return nil, reject(InvalidValue, i)
				}
				i++
			}
			if i == start {
				return nil, reject(Syntax, start)
			}
			value = input[start:i]
			switch value {
			case "AND":
				kind = and
			case "OR":
				kind = or
			case "NOT":
				kind = not
			}
		}
		result = append(result, token{kind, value, start, i, spaced})
	}
	return append(result, token{kind: end, position: len(input), end: len(input)}), nil
}
func quotedString(input string, start int) (string, int, error) {
	quote := input[start]
	var out strings.Builder
	for i := start + 1; i < len(input); {
		b := input[i]
		i++
		if b == quote {
			return out.String(), i, nil
		}
		if b < 0x20 {
			return "", 0, reject(InvalidValue, i-1)
		}
		if b != '\\' {
			out.WriteByte(b)
			continue
		}
		if i == len(input) {
			return "", 0, reject(Syntax, i-1)
		}
		escape := input[i]
		i++
		switch escape {
		case '\\', '\'', '"':
			out.WriteByte(escape)
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case 'u':
			first, next, err := unicodeEscape(input, i)
			if err != nil {
				return "", 0, err
			}
			i = next
			r := rune(first)
			if utf16.IsSurrogate(r) {
				if first < 0xd800 || first > 0xdbff || i+2 > len(input) || input[i:i+2] != "\\u" {
					return "", 0, reject(InvalidValue, i)
				}
				second, next, err := unicodeEscape(input, i+2)
				if err != nil {
					return "", 0, err
				}
				i = next
				if second < 0xdc00 || second > 0xdfff {
					return "", 0, reject(InvalidValue, i)
				}
				r = utf16.DecodeRune(r, rune(second))
			}
			out.WriteRune(r)
		default:
			return "", 0, reject(InvalidValue, i-1)
		}
	}
	return "", 0, reject(Syntax, start)
}
func unicodeEscape(input string, start int) (uint16, int, error) {
	if start+4 > len(input) {
		return 0, 0, reject(InvalidValue, start)
	}
	value, err := strconv.ParseUint(input[start:start+4], 16, 16)
	if err != nil {
		return 0, 0, reject(InvalidValue, start)
	}
	return uint16(value), start + 4, nil
}
