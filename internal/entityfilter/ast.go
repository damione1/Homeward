package entityfilter

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"
	"strings"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
)

type expression interface {
	matches(Candidate) bool
	digest() [32]byte
}
type allExpression struct{}

func (allExpression) matches(Candidate) bool { return true }
func (allExpression) digest() [32]byte       { return nodeHash(1, nil) }

type logicalKind uint8

const (
	conjunction logicalKind = iota + 1
	disjunction
)

type logicalExpression struct {
	kind     logicalKind
	children []expression
}

func (n logicalExpression) matches(c Candidate) bool {
	switch n.kind {
	case conjunction:
		for _, child := range n.children {
			if !child.matches(c) {
				return false
			}
		}
		return true
	case disjunction:
		for _, child := range n.children {
			if child.matches(c) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
func (n logicalExpression) digest() [32]byte {
	digests := make([][32]byte, len(n.children))
	for i, child := range n.children {
		digests[i] = child.digest()
	}
	sort.Slice(digests, func(i, j int) bool { return bytes.Compare(digests[i][:], digests[j][:]) < 0 })
	return nodeHash(byte(n.kind)+1, func(h hash.Hash) {
		for _, digest := range digests {
			h.Write(digest[:])
		}
	})
}
func combine(kind logicalKind, a, b expression) expression {
	children := make([]expression, 0, 2)
	for _, value := range []expression{a, b} {
		if n, ok := value.(logicalExpression); ok && n.kind == kind {
			children = append(children, n.children...)
		} else {
			children = append(children, value)
		}
	}
	return logicalExpression{kind, children}
}

type negatedExpression struct{ inner expression }

func (n negatedExpression) matches(c Candidate) bool { return !n.inner.matches(c) }
func (n negatedExpression) digest() [32]byte {
	return nodeHash(4, func(h hash.Hash) { value := n.inner.digest(); h.Write(value[:]) })
}

type stringField uint8

const (
	nameField stringField = iota + 1
	displayNameField
)

func (f stringField) value(c Candidate) string {
	switch f {
	case nameField:
		return c.name.String()
	case displayNameField:
		return c.label.String()
	default:
		return ""
	}
}

type comparison uint8

const (
	eq comparison = iota + 1
	ne
	lt
	le
	gt
	ge
	contains
	present
)

type patternKind uint8

const (
	exactPattern patternKind = iota + 1
	prefixPattern
	suffixPattern
	substringPattern
	anyPattern
)

type stringPattern struct {
	kind    patternKind
	literal string
}

func newPattern(value string) (stringPattern, bool) {
	prefix, suffix := strings.HasPrefix(value, "*"), strings.HasSuffix(value, "*")
	if prefix {
		value = value[1:]
	}
	if suffix && value != "" {
		value = value[:len(value)-1]
	}
	if strings.ContainsRune(value, '*') {
		return stringPattern{}, false
	}
	kind := exactPattern
	switch {
	case prefix && suffix:
		if value == "" {
			kind = anyPattern
		} else {
			kind = substringPattern
		}
	case prefix:
		kind = suffixPattern
	case suffix:
		kind = prefixPattern
	}
	return stringPattern{kind, strings.Clone(value)}, true
}
func (p stringPattern) matches(value string) bool {
	switch p.kind {
	case exactPattern:
		return value == p.literal
	case prefixPattern:
		return strings.HasPrefix(value, p.literal)
	case suffixPattern:
		return strings.HasSuffix(value, p.literal)
	case substringPattern:
		return strings.Contains(value, p.literal)
	case anyPattern:
		return true
	default:
		return false
	}
}

type stringExpression struct {
	field    stringField
	operator comparison
	pattern  stringPattern
}

func (n stringExpression) matches(c Candidate) bool {
	value := n.field.value(c)
	switch n.operator {
	case eq:
		return n.pattern.matches(value)
	case ne:
		return !n.pattern.matches(value)
	case lt:
		return value < n.pattern.literal
	case le:
		return value <= n.pattern.literal
	case gt:
		return value > n.pattern.literal
	case ge:
		return value >= n.pattern.literal
	case contains:
		if n.pattern.kind == exactPattern {
			return strings.Contains(value, n.pattern.literal)
		}
		return n.pattern.matches(value)
	case present:
		return value != ""
	default:
		return false
	}
}
func (n stringExpression) digest() [32]byte {
	return nodeHash(5, func(h hash.Hash) {
		h.Write([]byte{byte(n.field), byte(n.operator), byte(n.pattern.kind)})
		writeString(h, n.pattern.literal)
	})
}

type literalExpression struct{ pattern stringPattern }

func (n literalExpression) matches(c Candidate) bool {
	if n.pattern.kind == exactPattern {
		return strings.Contains(c.name.String(), n.pattern.literal) || strings.Contains(c.label.String(), n.pattern.literal)
	}
	return n.pattern.matches(c.name.String()) || n.pattern.matches(c.label.String())
}
func (n literalExpression) digest() [32]byte {
	return nodeHash(6, func(h hash.Hash) { h.Write([]byte{byte(n.pattern.kind)}); writeString(h, n.pattern.literal) })
}

type deviceExpression struct{ device apitypes.DeviceName }

func (n deviceExpression) matches(c Candidate) bool { return c.device.Valid() && c.device == n.device }
func (n deviceExpression) digest() [32]byte {
	return nodeHash(7, func(h hash.Hash) { writeString(h, n.device.String()) })
}

type traitExpression struct{ trait traitField }

func (n traitExpression) matches(c Candidate) bool {
	for _, value := range c.traits {
		if value == n.trait {
			return true
		}
	}
	return false
}
func (n traitExpression) digest() [32]byte {
	return nodeHash(8, func(h hash.Hash) {
		var number [4]byte
		binary.BigEndian.PutUint32(number[:], uint32(n.trait.number))
		h.Write(number[:])
	})
}

type availabilityExpression struct{ value api.Availability }

func (n availabilityExpression) matches(c Candidate) bool {
	bit, ok := availabilityBit(n.value)
	return ok && c.availabilities&bit != 0
}
func (n availabilityExpression) digest() [32]byte {
	return nodeHash(9, func(h hash.Hash) {
		var number [4]byte
		binary.BigEndian.PutUint32(number[:], uint32(n.value))
		h.Write(number[:])
	})
}
func nodeHash(kind byte, write func(hash.Hash)) [32]byte {
	h := sha256.New()
	h.Write([]byte("homeward.entity-filter/v2\x00"))
	h.Write([]byte{kind})
	if write != nil {
		write(h)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
func writeString(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])
	h.Write([]byte(value))
}
