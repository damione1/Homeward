// Package derivedpower admits the three versioned MVP estimated-power rules.
// Inputs are observed source states; an absent or unqualified input is never a
// numeric zero.
package derivedpower

import (
	"errors"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

var ErrInvalidPower = errors.New("derived power: invalid input or composition")

type Kind uint8

const (
	KindUnspecified Kind = iota
	KindUPS
	KindFloorKitchen
	KindHVAC
)

func (k Kind) valid() bool { return k >= KindUPS && k <= KindHVAC }

func Kinds() []Kind { return []Kind{KindUPS, KindFloorKitchen, KindHVAC} }

func (k Kind) ProgramName() (string, error) {
	switch k {
	case KindUPS:
		return "nha107.ups-power-v1", nil
	case KindFloorKitchen:
		return "nha107.floor-kitchen-power-v1", nil
	case KindHVAC:
		return "nha107.hvac-power-v1", nil
	default:
		return "", ErrInvalidPower
	}
}

func (k Kind) SourceAlias() (string, error) {
	switch k {
	case KindUPS:
		return "sensor.ups_load", nil
	case KindFloorKitchen:
		return "climate.plancher_cuisine", nil
	case KindHVAC:
		return "climate.home", nil
	default:
		return "", ErrInvalidPower
	}
}

func (k Kind) OutputAlias() (string, error) {
	switch k {
	case KindUPS:
		return "sensor.ups_power_consumption", nil
	case KindFloorKitchen:
		return "sensor.plancher_cuisine_power_2", nil
	case KindHVAC:
		return "sensor.hvac_power_consumption", nil
	default:
		return "", ErrInvalidPower
	}
}

func (k Kind) SourceKey() (integrations.SourceKey, error) {
	alias, err := k.OutputAlias()
	if err != nil {
		return integrations.SourceKey{}, err
	}
	return integrations.NewSourceKey("derived-power-mvp-v1", alias)
}

func Instance() (domain.IntegrationInstanceID, error) {
	return domain.ParseIntegrationInstanceID("homeward-derived-power-v1")
}

type Binding struct {
	kind   Kind
	id     reactivetemplate.ProgramID
	target domain.EntityID
	key    integrations.SourceKey
}

func NewBinding(kind Kind, target domain.EntityID) (Binding, error) {
	name, err := kind.ProgramName()
	if err != nil {
		return Binding{}, err
	}
	if _, err := target.Value(); err != nil {
		return Binding{}, ErrInvalidPower
	}
	id, err := reactivetemplate.ParseProgramID(name)
	if err != nil {
		return Binding{}, err
	}
	key, err := kind.SourceKey()
	if err != nil {
		return Binding{}, err
	}
	return Binding{kind: kind, id: id, target: target, key: key}, nil
}

func (b Binding) Kind() Kind                        { return b.kind }
func (b Binding) ID() reactivetemplate.ProgramID    { return b.id }
func (b Binding) Target() domain.EntityID           { return b.target }
func (b Binding) SourceKey() integrations.SourceKey { return b.key }
