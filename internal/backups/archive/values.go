// Package archive seals prepared installation data into the documented age v1
// format. Snapshot consistency and scheduling belong to its callers.
package archive

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"filippo.io/age"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/storage"
)

type ErrorKind uint8

const (
	Invalid ErrorKind = iota + 1
	Integrity
	Unsupported
	Capacity
	Unavailable
	Cancelled
	Conflict
)

var errorNames = [...]string{"", "invalid input", "integrity failure", "unsupported version", "capacity exceeded", "unavailable", "cancelled", "destination exists"}

type Error struct{ kind ErrorKind }

func fail(kind ErrorKind) *Error {
	if kind < Invalid || kind > Conflict {
		kind = Unavailable
	}
	return &Error{kind: kind}
}
func (e *Error) Kind() ErrorKind {
	if e == nil || e.kind < Invalid || e.kind > Conflict {
		return Unavailable
	}
	return e.kind
}
func (e *Error) Error() string { return "backup archive: " + errorNames[e.Kind()] }
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e != nil && t != nil && e.Kind() == t.Kind()
}

// Only native X25519 age keys are admitted. Neither type exposes age's private
// identity String method, arbitrary Recipient callbacks, SSH or plugins.
type RecoveryPublic struct{ recipient *age.X25519Recipient }
type RecoveryPrivate struct{ identity *age.X25519Identity }

func ParseRecoveryPublic(raw string) (RecoveryPublic, error) {
	if len(raw) > 256 || strings.TrimSpace(raw) != raw {
		return RecoveryPublic{}, fail(Invalid)
	}
	r, err := age.ParseX25519Recipient(raw)
	if err != nil {
		return RecoveryPublic{}, fail(Invalid)
	}
	return RecoveryPublic{recipient: r}, nil
}
func ParseRecoveryPrivate(raw []byte) (RecoveryPrivate, error) {
	if len(raw) > 256 || len(raw) == 0 {
		return RecoveryPrivate{}, fail(Invalid)
	}
	text := strings.TrimSuffix(string(raw), "\n")
	if strings.TrimSpace(text) != text {
		return RecoveryPrivate{}, fail(Invalid)
	}
	i, err := age.ParseX25519Identity(text)
	if err != nil {
		return RecoveryPrivate{}, fail(Invalid)
	}
	return RecoveryPrivate{identity: i}, nil
}
func (r RecoveryPublic) Valid() bool  { return r.recipient != nil }
func (r RecoveryPrivate) Valid() bool { return r.identity != nil }
func (r RecoveryPublic) PublicText() (string, error) {
	if !r.Valid() {
		return "", fail(Invalid)
	}
	return r.recipient.String(), nil
}
func (r RecoveryPrivate) Public() (RecoveryPublic, error) {
	if !r.Valid() {
		return RecoveryPublic{}, fail(Invalid)
	}
	return RecoveryPublic{recipient: r.identity.Recipient()}, nil
}
func (RecoveryPrivate) String() string                 { return "[REDACTED RECOVERY PRIVATE KEY]" }
func (r RecoveryPrivate) GoString() string             { return r.String() }
func (r RecoveryPrivate) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, r.String()) }
func (r RecoveryPrivate) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }
func (RecoveryPublic) String() string                  { return "backup.RecoveryPublic(<public>)" }
func (r RecoveryPublic) GoString() string              { return r.String() }
func (r RecoveryPublic) Format(f fmt.State, _ rune)    { _, _ = io.WriteString(f, r.String()) }
func (r RecoveryPublic) MarshalJSON() ([]byte, error)  { return json.Marshal(r.String()) }

const MaxEntries = 1026
const MaxManifestBytes int64 = 1 << 20

type Limits struct {
	ciphertext, plaintext int64
	entries               int
}

func NewLimits(ciphertext, plaintext int64, entries int) (Limits, error) {
	if ciphertext < 1 || ciphertext > backups.MaxArtifactBytes || plaintext < 1 || plaintext > backups.MaxArtifactBytes || entries < 2 || entries > MaxEntries {
		return Limits{}, fail(Invalid)
	}
	return Limits{ciphertext: ciphertext, plaintext: plaintext, entries: entries}, nil
}
func DefaultLimits() Limits {
	l, _ := NewLimits(backups.MaxArtifactBytes, backups.MaxArtifactBytes, MaxEntries)
	return l
}
func (l Limits) Valid() bool { return l.ciphertext > 0 && l.plaintext > 0 && l.entries >= 2 }

type YAMLSource struct {
	logical string
	size    int64
	reader  io.Reader
}

func (YAMLSource) String() string                 { return "backup.YAMLSource(<private>)" }
func (s YAMLSource) GoString() string             { return s.String() }
func (s YAMLSource) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, s.String()) }
func (s YAMLSource) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func validLogicalPath(value string) bool {
	if !fs.ValidPath(value) || value == "." || len(value) > 1024 || path.Clean(value) != value || strings.ContainsAny(value, "\\\x00:\r\n") {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) > 16 {
		return false
	}
	for _, part := range parts {
		if len(part) > 128 {
			return false
		}
	}
	return true
}
func pathCollision(seen map[string]bool, next string) bool {
	if seen[next] {
		return true
	}
	for prior := range seen {
		if strings.HasPrefix(prior, next+"/") || strings.HasPrefix(next, prior+"/") {
			return true
		}
	}
	return false
}
func NewYAMLSource(logical string, size int64, reader io.Reader) (YAMLSource, error) {
	if !validLogicalPath(logical) || size < 0 || size > backups.MaxArtifactBytes || backups.IsNil(reader) {
		return YAMLSource{}, fail(Invalid)
	}
	return YAMLSource{logical: logical, size: size, reader: reader}, nil
}

type Input struct {
	database     io.Reader
	databaseSize int64
	yaml         []YAMLSource
	key          storage.BackupServerKey
	valid        bool
}

// Readers are single-use prepared snapshot streams; the caller owns their Close.
// This constructor does not claim they came from a coherent SQL snapshot.
func NewInput(database io.Reader, databaseSize int64, yaml []YAMLSource, key storage.BackupServerKey) (Input, error) {
	if backups.IsNil(database) || databaseSize < 1 || databaseSize > backups.MaxArtifactBytes || len(yaml) == 0 || len(yaml) > MaxEntries-2 || !key.Valid() {
		return Input{}, fail(Invalid)
	}
	seen := make(map[string]bool, len(yaml))
	for _, source := range yaml {
		if !validLogicalPath(source.logical) || source.size < 0 || source.size > backups.MaxArtifactBytes || backups.IsNil(source.reader) || pathCollision(seen, source.logical) {
			return Input{}, fail(Invalid)
		}
		seen[source.logical] = true
	}
	return Input{database: database, databaseSize: databaseSize, yaml: append([]YAMLSource(nil), yaml...), key: key, valid: true}, nil
}
func (Input) String() string                 { return "backup.Input(<private>)" }
func (i Input) GoString() string             { return i.String() }
func (i Input) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, i.String()) }
func (i Input) MarshalJSON() ([]byte, error) { return json.Marshal(i.String()) }
