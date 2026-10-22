package mqtt

import (
	"crypto/rand"
	"encoding/binary"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

const (
	correlationVersion   = 2
	correlationCause     = 1
	correlationTrace     = 2
	maxCausalID          = 256
	correlationScopeSize = 16
	correlationHeader    = 2 + correlationScopeSize
	maxCorrelation       = correlationHeader + 2*(2+maxCausalID)
)

// correlationMetadata is an immutable, bounded MQTT5 CorrelationData value.
// Decoding proves its shape only; the Host's admitted action tracker remains
// the authority for command identity, ownership, freshness and compatibility.
// A zero value is absent. The wire form is version, presence flags, fixed
// scope, then each present cause/correlation as a big-endian uint16 length and
// UTF-8 bytes. The nonzero scope identifies one admitted MQTT profile
// lifetime. It is an echo fence, never peer authentication or action authority.
type correlationMetadata struct{ wire string }

type correlationScope [correlationScopeSize]byte

func newCorrelationScope() (correlationScope, error) {
	var scope correlationScope
	if _, err := rand.Read(scope[:]); err != nil {
		return correlationScope{}, err
	}
	if scope == (correlationScope{}) {
		return correlationScope{}, domain.ErrInvalidCausality
	}
	return scope, nil
}

func encodeCorrelation(causality domain.Causality, scope correlationScope) (correlationMetadata, error) {
	if scope == (correlationScope{}) {
		return correlationMetadata{}, domain.ErrInvalidCausality
	}
	cause, trace, err := causality.Components()
	if err != nil {
		return correlationMetadata{}, err
	}
	flags := byte(0)
	var causeText, traceText string
	if cause != nil {
		causeText, err = cause.Value()
		if err != nil || len(causeText) > maxCausalID {
			return correlationMetadata{}, domain.ErrInvalidCausality
		}
		flags |= correlationCause
	}
	if trace != nil {
		traceText, err = trace.Value()
		if err != nil || len(traceText) > maxCausalID {
			return correlationMetadata{}, domain.ErrInvalidCausality
		}
		flags |= correlationTrace
	}
	wire := append([]byte{correlationVersion, flags}, scope[:]...)
	for _, field := range []string{causeText, traceText} {
		if field != "" {
			wire = binary.BigEndian.AppendUint16(wire, uint16(len(field)))
			wire = append(wire, field...)
		}
	}
	return correlationMetadata{wire: string(wire)}, nil
}

func parseCorrelation(wire []byte) (correlationMetadata, error) {
	// Check the complete bound before copying or constructing domain IDs.
	if len(wire) < correlationHeader || len(wire) > maxCorrelation || wire[0] != correlationVersion || wire[1]&^(byte(correlationCause|correlationTrace)) != 0 {
		return correlationMetadata{}, domain.ErrInvalidCausality
	}
	var scope correlationScope
	copy(scope[:], wire[2:correlationHeader])
	if scope == (correlationScope{}) {
		return correlationMetadata{}, domain.ErrInvalidCausality
	}
	remaining := wire[correlationHeader:]
	for _, flag := range []byte{correlationCause, correlationTrace} {
		if wire[1]&flag == 0 {
			continue
		}
		if len(remaining) < 2 {
			return correlationMetadata{}, domain.ErrInvalidCausality
		}
		size := int(binary.BigEndian.Uint16(remaining))
		remaining = remaining[2:]
		if size == 0 || size > maxCausalID || size > len(remaining) || !utf8.Valid(remaining[:size]) {
			return correlationMetadata{}, domain.ErrInvalidCausality
		}
		remaining = remaining[size:]
	}
	if len(remaining) != 0 {
		return correlationMetadata{}, domain.ErrInvalidCausality
	}
	return correlationMetadata{wire: string(wire)}, nil
}

func (m correlationMetadata) causality() (domain.Causality, error) {
	if len(m.wire) < correlationHeader || len(m.wire) > maxCorrelation {
		return domain.Causality{}, domain.ErrInvalidCausality
	}
	if _, err := parseCorrelation([]byte(m.wire)); err != nil {
		return domain.Causality{}, err
	}
	remaining := []byte(m.wire)[correlationHeader:]
	var cause *domain.CausationID
	var trace *domain.CorrelationID
	if m.wire[1]&correlationCause != 0 {
		size := int(binary.BigEndian.Uint16(remaining))
		id, err := domain.ParseCausationID(string(remaining[2 : 2+size]))
		if err != nil {
			return domain.Causality{}, err
		}
		cause = &id
		remaining = remaining[2+size:]
	}
	if m.wire[1]&correlationTrace != 0 {
		size := int(binary.BigEndian.Uint16(remaining))
		id, err := domain.ParseCorrelationID(string(remaining[2 : 2+size]))
		if err != nil {
			return domain.Causality{}, err
		}
		trace = &id
	}
	return domain.NewCausality(cause, trace)
}

func (o Observation) stateCausality(scope correlationScope) domain.Causality {
	if o.class != ObservationLive || scope == (correlationScope{}) || len(o.correlation.wire) < correlationHeader ||
		o.correlation.wire[2:correlationHeader] != string(scope[:]) {
		return domain.Causality{}
	}
	cause, err := o.correlation.causality()
	if err != nil {
		return domain.Causality{}
	}
	return cause
}
