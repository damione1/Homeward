package calendarnames

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"
)

func Etag(name string, number uint64) (string, error) {
	if name == "" || number < 1 || number > math.MaxInt64 {
		return "", ErrInvalid
	}
	var raw [41]byte
	raw[0] = 2
	digest := sha256.Sum256([]byte(name))
	copy(raw[1:33], digest[:])
	binary.BigEndian.PutUint64(raw[33:], number)
	return "W/\"" + base64.RawURLEncoding.EncodeToString(raw[:]) + "\"", nil
}
