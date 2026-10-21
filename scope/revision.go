package scope

import "errors"

var ErrInvalidRevision = errors.New("invalid scope revision")

// Revision is a closed, admitted selection. Its zero value is invalid; callers
// must explicitly choose the baseline or parse a configured revision.
type Revision struct{ value uint8 }

func BaselineV1() Revision  { return Revision{value: 1} }
func EcobeeMVPV1() Revision { return Revision{value: 2} }

func ParseRevision(text string) (Revision, error) {
	switch text {
	case "v1":
		return BaselineV1(), nil
	case "ecobee_mvp_v1":
		return EcobeeMVPV1(), nil
	default:
		return Revision{}, ErrInvalidRevision
	}
}

func (r Revision) Value() (string, error) {
	switch r.value {
	case 1:
		return "v1", nil
	case 2:
		return "ecobee_mvp_v1", nil
	default:
		return "", ErrInvalidRevision
	}
}
