// The structural guard bounds untrusted ICS before the pinned parser.
package calendars

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"
)

type GuardIssue uint8

const (
	GuardInvalid GuardIssue = iota
	BodyTooLarge
	LineTooLarge
	TooManyLines
	TooDeep
	TooManyComponents
	TooManyEvents
	TooManyZones
	InvalidText
	UnbalancedComponent
	UnclosedComponent
	InvalidEnvelope
)

type guardFailure struct{ issue GuardIssue }

func (e guardFailure) Error() string {
	switch e.issue {
	case BodyTooLarge:
		return "body too large"
	case LineTooLarge:
		return "line too large"
	case TooManyLines:
		return "too many lines"
	case TooDeep:
		return "too deep"
	case TooManyComponents:
		return "too many components"
	case TooManyEvents:
		return "too many events"
	case TooManyZones:
		return "too many zones"
	case InvalidText:
		return "invalid text"
	case UnbalancedComponent:
		return "unbalanced component"
	case UnclosedComponent:
		return "unclosed component"
	case InvalidEnvelope:
		return "invalid envelope"
	case GuardInvalid:
		return "invalid guard input"
	}
	return "invalid guard issue"
}

const maxBody = 1 << 20
const maxLogicalLine = 8192
const maxLogicalLines = 16384
const maxPhysicalLines = 32768

// These are cost counters only. They are not admitted business objects.
type guardStats struct {
	BodyBytes, PhysicalLines, LogicalLines int
	Components, Events, Zones, MaxDepth    int
}

// guard consumes an already byte-bounded decompressed body. Read limiting in
// CloudClient must precede this copy; this function is not that transport bound.
// The output is unfolded LF text. A folded UTF8 sequence is validated only after
// reassembly. Nothing is sent to the candidate parser until all guards pass.
func guard(ctx context.Context, raw []byte) ([]byte, guardStats, error) {
	stats := guardStats{BodyBytes: len(raw)}
	if ctx == nil {
		return nil, stats, guardFailure{GuardInvalid}
	}
	if err := ctx.Err(); err != nil {
		return nil, stats, err
	}
	if len(raw) > maxBody {
		return nil, stats, guardFailure{BodyTooLarge}
	}
	var stack [4]string
	depth := 0
	closed := false
	var output []byte
	var logical []byte
	pending := false
	fail := func(issue GuardIssue) error { return guardFailure{issue} }
	flush := func() error {
		if !pending {
			return nil
		}
		stats.LogicalLines++
		if stats.LogicalLines > maxLogicalLines {
			return fail(TooManyLines)
		}
		if !utf8.Valid(logical) || bytes.IndexByte(logical, 0) >= 0 {
			return fail(InvalidText)
		}
		if closed {
			return fail(InvalidEnvelope)
		}
		colon := bytes.IndexByte(logical, ':')
		name := ""
		value := ""
		if colon >= 0 {
			name = strings.ToUpper(string(logical[:colon]))
			value = strings.ToUpper(string(logical[colon+1:]))
		}
		switch name {
		case "BEGIN":
			if depth == len(stack) {
				return fail(TooDeep)
			}
			if depth == 0 && value != "VCALENDAR" || depth > 0 && value == "VCALENDAR" {
				return fail(InvalidEnvelope)
			}
			stats.Components++
			if stats.Components > 4096 {
				return fail(TooManyComponents)
			}
			if value == "VEVENT" {
				stats.Events++
			}
			if value == "VTIMEZONE" {
				stats.Zones++
			}
			if stats.Events > 2048 {
				return fail(TooManyEvents)
			}
			if stats.Zones > 8 {
				return fail(TooManyZones)
			}
			stack[depth] = value
			depth++
			if depth > stats.MaxDepth {
				stats.MaxDepth = depth
			}
		case "END":
			if depth == 0 || value != stack[depth-1] {
				return fail(UnbalancedComponent)
			}
			depth--
			if depth == 0 {
				closed = true
			}
		default:
			if depth == 0 {
				return fail(InvalidEnvelope)
			}
		}
		output = append(output, logical...)
		output = append(output, '\n')
		logical = logical[:0]
		pending = false
		return nil
	}
	for cursor := 0; cursor < len(raw); {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		end := bytes.IndexByte(raw[cursor:], '\n')
		next := len(raw)
		if end < 0 {
			end = len(raw)
		} else {
			end += cursor
			next = end + 1
		}
		physical := raw[cursor:end]
		cursor = next
		stats.PhysicalLines++
		if stats.PhysicalLines > maxPhysicalLines {
			return nil, stats, fail(TooManyLines)
		}
		if len(physical) > 0 && physical[len(physical)-1] == '\r' {
			physical = physical[:len(physical)-1]
		}
		folded := len(physical) > 0 && (physical[0] == ' ' || physical[0] == '\t')
		if folded {
			if !pending {
				return nil, stats, fail(InvalidEnvelope)
			}
			physical = physical[1:]
		} else {
			if err := flush(); err != nil {
				return nil, stats, err
			}
			if len(physical) == 0 {
				continue
			}
			pending = true
		}
		if len(logical)+len(physical) > maxLogicalLine {
			return nil, stats, fail(LineTooLarge)
		}
		logical = append(logical, physical...)
	}
	if err := flush(); err != nil {
		return nil, stats, err
	}
	if depth != 0 {
		return nil, stats, fail(UnclosedComponent)
	}
	if !closed {
		return nil, stats, fail(InvalidEnvelope)
	}
	return output, stats, nil
}
