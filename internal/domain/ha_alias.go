package domain

import (
	"errors"
	"regexp"
)

var ErrInvalidHAAlias = errors.New("invalid Home Assistant alias")

var haAliasPattern = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)

// HAAlias is a historical Home Assistant label, distinct from an EntityID.
type HAAlias struct{ alias string }

func ParseHAAlias(value string) (HAAlias, error) {
	if len(value) == 0 || len(value) > 255 || !haAliasPattern.MatchString(value) {
		return HAAlias{}, ErrInvalidHAAlias
	}
	return HAAlias{alias: value}, nil
}

func (a HAAlias) Value() (string, error) {
	if _, err := ParseHAAlias(a.alias); err != nil {
		return "", err
	}
	return a.alias, nil
}
