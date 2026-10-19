package scope

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"

	"github.com/damione1/Homeward/internal/domain"
)

// The policy records provenance from the retained four-button selection.
// No alias spelling implies an integration or an external action.
//
//go:embed helper_button_policy_v1.json
var helperButtonPolicyV1 []byte

type HelperButtonDecision uint8

const (
	HelperButtonDecisionInvalid HelperButtonDecision = iota
	HelperButtonLocalPress
	HelperButtonNoAuthorizedTarget
)

type HelperButtonPolicy struct {
	decisions map[domain.HAAlias]HelperButtonDecision
}

func ParseHelperButtonPolicy(data []byte) (HelperButtonPolicy, error) {
	var raw struct {
		Version int `json:"version"`
		Buttons []struct {
			Alias    string `json:"alias"`
			Decision string `json:"decision"`
		} `json:"buttons"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&raw) != nil || raw.Version != 1 || len(raw.Buttons) != 4 {
		return HelperButtonPolicy{}, errors.New("invalid helper button policy")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return HelperButtonPolicy{}, errors.New("trailing helper button policy data")
	}
	result := HelperButtonPolicy{decisions: make(map[domain.HAAlias]HelperButtonDecision, len(raw.Buttons))}
	for _, item := range raw.Buttons {
		alias, err := domain.ParseHAAlias(item.Alias)
		if err != nil || result.decisions[alias] != HelperButtonDecisionInvalid {
			return HelperButtonPolicy{}, errors.New("invalid helper button alias")
		}
		var decision HelperButtonDecision
		switch item.Decision {
		case "local_press":
			decision = HelperButtonLocalPress
		case "no_authorized_target":
			decision = HelperButtonNoAuthorizedTarget
		default:
			return HelperButtonPolicy{}, errors.New("invalid helper button decision")
		}
		result.decisions[alias] = decision
	}
	return result, nil
}

func RetainedHelperButtonPolicy() (HelperButtonPolicy, error) {
	return ParseHelperButtonPolicy(helperButtonPolicyV1)
}

func (p HelperButtonPolicy) Decision(alias domain.HAAlias) HelperButtonDecision {
	if p.decisions == nil {
		return HelperButtonDecisionInvalid
	}
	if _, err := alias.Value(); err != nil {
		return HelperButtonDecisionInvalid
	}
	if decision, known := p.decisions[alias]; known {
		return decision
	}
	// A newly configured button has only the intrinsic local press operation.
	// The retained Cast alias is the explicit exception in the provenance file.
	return HelperButtonLocalPress
}
