package storage

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
)

// FactorAPIIdentityKey encodes the actual native composite primary key. IDs may
// contain punctuation, Unicode or separators; concatenation cannot conflate
// another account's factor. This key is private and is never a resource name.
func FactorAPIIdentityKey(account contracts.AccountID, factor contracts.FactorCredentialID) (string, error) {
	a, err := account.Value()
	if err != nil || !authIdentityPart(a) {
		return "", ErrAPIResourceIdentity
	}
	f, err := factor.Value()
	if err != nil || !authIdentityPart(f) {
		return "", ErrAPIResourceIdentity
	}
	return strconv.Itoa(len(a)) + ":" + a + f, nil
}

func authIdentityPart(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// ParseFactorAPIIdentityKey admits persisted identity data, not caller input.
// The account must still equal the requested authorized parent before any use.
func ParseFactorAPIIdentityKey(key string) (contracts.AccountID, contracts.FactorCredentialID, error) {
	length, rest, ok := strings.Cut(key, ":")
	n, err := strconv.Atoi(length)
	if !ok || err != nil || strconv.Itoa(n) != length || n < 1 || n > 128 || n >= len(rest) {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, ErrAPIResourceIntegrity
	}
	if !authIdentityPart(rest[:n]) || !authIdentityPart(rest[n:]) {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, ErrAPIResourceIntegrity
	}
	account, err := contracts.ParseAccountID(rest[:n])
	if err != nil {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, ErrAPIResourceIntegrity
	}
	factor, err := contracts.ParseFactorCredentialID(rest[n:])
	if err != nil {
		return contracts.AccountID{}, contracts.FactorCredentialID{}, ErrAPIResourceIntegrity
	}
	return account, factor, nil
}
