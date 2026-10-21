package storage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
)

// BackupServerKey is the admitted NHA-027 key file, not a second key or keyring.
// Its only plaintext output is an explicit writer owned by archive composition.
type BackupServerKey struct{ key masterKey }

func (k BackupServerKey) Valid() bool                  { return k.key.valid() }
func (BackupServerKey) String() string                 { return "[REDACTED BACKUP SERVER KEY]" }
func (k BackupServerKey) GoString() string             { return k.String() }
func (k BackupServerKey) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, k.String()) }
func (k BackupServerKey) MarshalJSON() ([]byte, error) { return json.Marshal(k.String()) }
func (k BackupServerKey) EncodedSize() int64 {
	if !k.Valid() {
		return 0
	}
	return int64(len(secretFilePrefix) + 44 + 1)
}

func (r *SecretRepository) BackupKey() (BackupServerKey, error) {
	if !r.Valid() {
		return BackupServerKey{}, &Error{kind: SecretInvalid}
	}
	return BackupServerKey{key: r.key}, nil
}

// ParseBackupServerKey uses exactly the live server key-file grammar.
func ParseBackupServerKey(raw []byte) (BackupServerKey, error) {
	key, err := parseMasterKey(raw)
	if err != nil {
		return BackupServerKey{}, &Error{kind: SecretKeyMismatch}
	}
	return BackupServerKey{key: key}, nil
}

func (k BackupServerKey) WriteTo(dst io.Writer) (int64, error) {
	if !k.Valid() || dst == nil {
		return 0, &Error{kind: SecretInvalid}
	}
	v := reflect.ValueOf(dst)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return 0, &Error{kind: SecretInvalid}
		}
	}
	raw := []byte(secretFilePrefix + base64.StdEncoding.EncodeToString(k.key.bytes[:]) + "\n")
	defer clear(raw)
	n, err := dst.Write(raw)
	if err != nil || n != len(raw) {
		return int64(n), &Error{kind: SecretUnavailable}
	}
	return int64(n), nil
}
