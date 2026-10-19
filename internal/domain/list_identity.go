package domain

import (
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrInvalidListID = errors.New("invalid list ID")

// ListID is an opaque identity shared by the list domain and authorization.
// The display name, HA alias, account, and session are never identities here.
type ListID struct{ value string }

func ParseListID(value string) (ListID, error) {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return ListID{}, ErrInvalidListID
	}
	return ListID{value: value}, nil
}

func (id ListID) Value() (string, error) {
	parsed, err := ParseListID(id.value)
	return parsed.value, err
}
