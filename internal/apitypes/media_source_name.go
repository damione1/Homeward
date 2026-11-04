package apitypes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

// MediaSourceName identifies one exact native entity/schema/endpoint. It is
// independent of playback routes, labels, configuration generations and leases.
type MediaSourceName struct {
	home HomeName
	id   string
}

// MediaSourceNameForIdentity is the sole pure public-name derivation for a
// configured content source. It neither discovers a source nor grants access.
func MediaSourceNameForIdentity(home HomeName, entity domain.EntityID, schema, endpoint string) (MediaSourceName, error) {
	raw, err := entity.Value()
	if !home.Valid() || err != nil || len(raw) > 255 || len(schema) < 1 || len(schema) > 255 || len(endpoint) < 1 || len(endpoint) > 256 {
		return MediaSourceName{}, ErrResourceName
	}
	for _, value := range []string{raw, schema, endpoint} {
		if !utf8.ValidString(value) {
			return MediaSourceName{}, ErrResourceName
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return MediaSourceName{}, ErrResourceName
			}
		}
	}
	hash := sha256.New()
	hash.Write([]byte("homeward-media-source-name/2"))
	var length [8]byte
	for _, value := range []string{raw, schema, endpoint} {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	return MediaSourceName{home, "source-" + hex.EncodeToString(hash.Sum(nil)[:24])}, nil
}
func ParseMediaSourceName(raw string) (MediaSourceName, error) {
	const maximum = len("homes/") + 63 + len("/mediaSources/") + 55
	if len(raw) > maximum {
		return MediaSourceName{}, ErrResourceName
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 4 || parts[2] != "mediaSources" || len(parts[3]) != 55 || !strings.HasPrefix(parts[3], "source-") {
		return MediaSourceName{}, ErrResourceName
	}
	for _, character := range parts[3][7:] {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return MediaSourceName{}, ErrResourceName
		}
	}
	home, err := ParseHomeName(strings.Join(parts[:2], "/"))
	if err != nil {
		return MediaSourceName{}, err
	}
	return MediaSourceName{home, parts[3]}, nil
}
func (n MediaSourceName) Valid() bool { return n.home.Valid() && len(n.id) == 55 }
func (n MediaSourceName) String() string {
	if !n.Valid() {
		return ""
	}
	return n.home.String() + "/mediaSources/" + n.id
}
func (n MediaSourceName) Home() HomeName { return n.home }
