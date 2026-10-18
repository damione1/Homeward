// Package accounts admits account credentials and names before persistence.
package accounts

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var ErrInvalidLogin = errors.New("accounts: invalid login name")
var ErrInvalidDisplayName = errors.New("accounts: invalid display name")
var ErrInvalidPassword = errors.New("accounts: invalid password")
var ErrBlockedPassword = errors.New("accounts: password blocked")

// LoginName keeps the displayed spelling separate from the canonical SQL key.
type LoginName struct {
	display string
	key     string
}

func ParseLoginName(raw string) (LoginName, error) {
	if !validTextBounds(raw, 1, 128, 256) || hasControl(raw) || hasBorderSpace(raw) {
		return LoginName{}, ErrInvalidLogin
	}
	display := norm.NFC.String(raw)
	if !validTextBounds(display, 1, 128, 256) || hasControl(display) || hasBorderSpace(display) {
		return LoginName{}, ErrInvalidLogin
	}
	key := norm.NFC.String(cases.Fold().String(display))
	if !validTextBounds(key, 1, 128, 256) || hasControl(key) || hasBorderSpace(key) {
		return LoginName{}, ErrInvalidLogin
	}
	return LoginName{display: display, key: key}, nil
}

func (n LoginName) Values() (display, key string, err error) {
	if n.display == "" || n.key == "" {
		return "", "", ErrInvalidLogin
	}
	return n.display, n.key, nil
}

type DisplayName struct{ value string }

func ParseDisplayName(raw string) (DisplayName, error) {
	if !validTextBounds(raw, 1, 256, 1024) || hasControl(raw) {
		return DisplayName{}, ErrInvalidDisplayName
	}
	value := norm.NFC.String(raw)
	if !validTextBounds(value, 1, 256, 1024) || hasControl(value) || strings.TrimSpace(value) == "" {
		return DisplayName{}, ErrInvalidDisplayName
	}
	return DisplayName{value: value}, nil
}

func (n DisplayName) Value() (string, error) {
	if n.value == "" {
		return "", ErrInvalidDisplayName
	}
	return n.value, nil
}

func validTextBounds(value string, minimum, maximumRunes, maximumBytes int) bool {
	if !utf8.ValidString(value) || len(value) > maximumBytes {
		return false
	}
	count := utf8.RuneCountInString(value)
	return count >= minimum && count <= maximumRunes
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func hasBorderSpace(value string) bool {
	first, _ := utf8.DecodeRuneInString(value)
	last, _ := utf8.DecodeLastRuneInString(value)
	return unicode.IsSpace(first) || unicode.IsSpace(last)
}

// NormalizePasswordForVerification is shared with future NHA-141 login.
// Blocklist policy is deliberately absent, so a later blocklist change cannot
// make an existing correct password unverifiable.
func NormalizePasswordForVerification(raw string) (string, error) {
	if !validTextBounds(raw, 15, 1024, 4096) {
		return "", ErrInvalidPassword
	}
	value := norm.NFC.String(raw)
	if !validTextBounds(value, 15, 1024, 4096) {
		return "", ErrInvalidPassword
	}
	return value, nil
}

// Version 1 is deliberately small: OWASP/NIST recommend checking whole common
// passwords, and Homeward supplies the contextual entries. This is not a
// comprehensive breached-password corpus or a remote lookup.
// https://pages.nist.gov/800-63-4/sp800-63b.html#passwordver
const blocklistVersion = 1

var blockedWholePasswordsV1 = map[string]struct{}{
	"123456789012345":           {},
	"passwordpassword":          {},
	"correcthorsebatterystaple": {},
	"homewardhomeward":          {},
}

func AdmitNewPassword(raw string) (string, error) {
	value, err := NormalizePasswordForVerification(raw)
	if err != nil {
		return "", err
	}
	if _, blocked := blockedWholePasswordsV1[value]; blocked {
		return "", ErrBlockedPassword
	}
	return value, nil
}
