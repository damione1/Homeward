package storage

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash"
	"strings"
)

// ErrAccountVersionConflict never includes the actual account metadata or etag.
var ErrAccountVersionConflict = errors.New("account metadata version conflict")

// AccountVersion binds the complete nonsecret native account metadata. It is
// distinct from the revocation revision: display-only writes must also conflict.
// The weak etag makes no claim about byte-for-byte serialization of the resource.
type AccountVersion struct {
	digest [32]byte
	valid  bool
}

func ParseAccountVersion(etag string) (AccountVersion, error) {
	value, ok := strings.CutPrefix(etag, `W/"account-`)
	if !ok {
		return AccountVersion{}, ErrAccountInvalid
	}
	value, ok = strings.CutSuffix(value, `"`)
	if !ok {
		return AccountVersion{}, ErrAccountInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != value {
		return AccountVersion{}, ErrAccountInvalid
	}
	var digest [32]byte
	copy(digest[:], raw)
	return AccountVersion{digest: digest, valid: true}, nil
}
func (v AccountVersion) Etag() (string, error) {
	if !v.valid {
		return "", ErrAccountInvalid
	}
	return `W/"account-` + base64.RawURLEncoding.EncodeToString(v.digest[:]) + `"`, nil
}
func accountVersionText(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}
func (r AccountRecord) Version() (AccountVersion, error) {
	if !r.valid() {
		return AccountVersion{}, ErrAccountUnavailable
	}
	id, _ := r.id.Value()
	login, _, _ := r.login.Values()
	display, _ := r.display.Value()
	h := sha256.New()
	accountVersionText(h, "homeward.account-metadata/1")
	accountVersionText(h, id)
	accountVersionText(h, login)
	accountVersionText(h, display)
	var numbers [49]byte
	binary.BigEndian.PutUint64(numbers[0:8], uint64(r.role))
	binary.BigEndian.PutUint64(numbers[8:16], uint64(r.status))
	binary.BigEndian.PutUint64(numbers[16:24], uint64(r.revision.number))
	binary.BigEndian.PutUint64(numbers[24:32], uint64(r.createdAt.Unix()))
	binary.BigEndian.PutUint64(numbers[32:40], uint64(r.updatedAt.Unix()))
	binary.BigEndian.PutUint32(numbers[40:44], uint32(r.createdAt.Nanosecond()))
	binary.BigEndian.PutUint32(numbers[44:48], uint32(r.updatedAt.Nanosecond()))
	if r.secondFactorEnabled {
		numbers[48] = 1
	}
	_, _ = h.Write(numbers[:])
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return AccountVersion{digest: digest, valid: true}, nil
}
func (v AccountVersion) matches(current AccountRecord) bool {
	actual, err := current.Version()
	return err == nil && v.valid && actual == v
}
