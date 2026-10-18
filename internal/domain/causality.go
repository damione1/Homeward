package domain

import "errors"

var ErrInvalidCausality = errors.New("invalid causal identity")

// CausationID and CorrelationID are distinct, open identifiers. Their text is
// assigned by the producer of a command, event, or run; no UUID shape is imposed.
type CausationID struct{ causation string }
type CorrelationID struct{ correlation string }

func ParseCausationID(value string) (CausationID, error) {
	parsed, err := parseIdentity(value, ErrInvalidCausality)
	return CausationID{causation: parsed}, err
}

func ParseCorrelationID(value string) (CorrelationID, error) {
	parsed, err := parseIdentity(value, ErrInvalidCausality)
	return CorrelationID{correlation: parsed}, err
}

func (id CausationID) Value() (string, error) {
	return parseIdentity(id.causation, ErrInvalidCausality)
}

func (id CorrelationID) Value() (string, error) {
	return parseIdentity(id.correlation, ErrInvalidCausality)
}

// Causality names explicitly absent IDs. Its zero value means neither ID was
// supplied, which is valid for an unsolicited device report.
type Causality struct {
	cause       CausationID
	correlation CorrelationID
	hasCause    bool
	hasTrace    bool
}

func NewCausality(cause *CausationID, correlation *CorrelationID) (Causality, error) {
	var result Causality
	if cause != nil {
		if _, err := cause.Value(); err != nil {
			return Causality{}, err
		}
		result.cause, result.hasCause = *cause, true
	}
	if correlation != nil {
		if _, err := correlation.Value(); err != nil {
			return Causality{}, err
		}
		result.correlation, result.hasTrace = *correlation, true
	}
	return result, nil
}

func (c Causality) Components() (*CausationID, *CorrelationID, error) {
	if !c.hasCause && c.cause != (CausationID{}) || !c.hasTrace && c.correlation != (CorrelationID{}) {
		return nil, nil, ErrInvalidCausality
	}
	var cause *CausationID
	var correlation *CorrelationID
	if c.hasCause {
		if _, err := c.cause.Value(); err != nil {
			return nil, nil, err
		}
		copy := c.cause
		cause = &copy
	}
	if c.hasTrace {
		if _, err := c.correlation.Value(); err != nil {
			return nil, nil, err
		}
		copy := c.correlation
		correlation = &copy
	}
	return cause, correlation, nil
}
