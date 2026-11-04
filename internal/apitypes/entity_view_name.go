package apitypes

import "strings"

// EntityViewName identifies a caller-owned ephemeral native capture. Syntax
// grants no access and cannot substitute for its current credential/rights.
type EntityViewName struct{ value string }

func ParseEntityViewName(value string) (EntityViewName, error) {
	if err := resourceName(value, "homes", "entityViews"); err != nil {
		return EntityViewName{}, err
	}
	return EntityViewName{value}, nil
}
func (n EntityViewName) String() string { return n.value }
func (n EntityViewName) Valid() bool    { return n.value != "" }
func (n EntityViewName) Home() HomeName {
	if !n.Valid() {
		return HomeName{}
	}
	return HomeName{n.value[:strings.LastIndex(n.value, "/entityViews/")]}
}
