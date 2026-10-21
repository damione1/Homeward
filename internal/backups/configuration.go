package backups

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// ProviderKind is closed. Adding S3 requires an explicit admitted variant and
// settings constructor here; producers continue to use Provider unchanged.
type ProviderKind uint8

const (
	Local ProviderKind = iota + 1
	S3
)

var providerNames = [...]string{"", "local", "s3"}

func (k ProviderKind) Valid() bool { return k == Local || k == S3 }
func (k ProviderKind) String() string {
	if !k.Valid() {
		return ""
	}
	return providerNames[k]
}
func ParseProviderKind(raw string) (ProviderKind, error) {
	for k := Local; int(k) < len(providerNames); k++ {
		if k.Valid() && raw == providerNames[k] {
			return k, nil
		}
	}
	return 0, NewError(Unsupported)
}

const (
	MaxInstances                  = 16
	MaxArtifactBytes        int64 = 8 << 30
	MaxPageSize                   = 128
	MaxScanEntries                = 4096
	MaxConcurrentOperations       = 4
)

// Limits bound a whole operation, including foreign/pending directory entries.
// These are technical budgets, not retention or scheduling policy.
type Limits struct {
	artifactBytes                     int64
	pageSize, scanEntries, concurrent int
}

func NewLimits(artifactBytes int64, pageSize, scanEntries, concurrent int) (Limits, error) {
	if artifactBytes < 1 || artifactBytes > MaxArtifactBytes || pageSize < 1 || pageSize > MaxPageSize || scanEntries < pageSize || scanEntries > MaxScanEntries || concurrent < 1 || concurrent > MaxConcurrentOperations {
		return Limits{}, NewError(InvalidInput)
	}
	return Limits{artifactBytes, pageSize, scanEntries, concurrent}, nil
}
func DefaultLimits() Limits {
	limits, _ := NewLimits(MaxArtifactBytes, MaxPageSize, MaxScanEntries, MaxConcurrentOperations)
	return limits
}
func (l Limits) Valid() bool {
	return l.artifactBytes > 0 && l.pageSize > 0 && l.scanEntries >= l.pageSize && l.concurrent > 0
}
func (l Limits) ArtifactBytes() int64      { return l.artifactBytes }
func (l Limits) PageSize() int             { return l.pageSize }
func (l Limits) ScanEntries() int          { return l.scanEntries }
func (l Limits) ConcurrentOperations() int { return l.concurrent }

type LocalSettings struct {
	root   string
	limits Limits
}

func (s LocalSettings) Root() string                 { return s.root }
func (s LocalSettings) Limits() Limits               { return s.limits }
func (s LocalSettings) Valid() bool                  { return s.root != "" && s.limits.Valid() }
func (LocalSettings) String() string                 { return "backup.LocalSettings(<private>)" }
func (s LocalSettings) GoString() string             { return s.String() }
func (s LocalSettings) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(s.String())) }
func (s LocalSettings) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// InstanceSettings is an admitted private sum, not a bag of provider options.
type InstanceSettings struct {
	id      InstanceID
	kind    ProviderKind
	enabled bool
	local   LocalSettings
	s3      S3Settings
}

func NewLocalSettings(id InstanceID, enabled bool, root string, limits Limits) (InstanceSettings, error) {
	if !id.Valid() || !limits.Valid() || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || strings.ContainsRune(root, '\x00') {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	return InstanceSettings{id: id, kind: Local, enabled: enabled, local: LocalSettings{root: root, limits: limits}}, nil
}
func (s InstanceSettings) Valid() bool {
	if !s.id.Valid() {
		return false
	}
	switch s.kind {
	case Local:
		return s.local.Valid() && !s.s3.Valid()
	case S3:
		return s.s3.Valid() && !s.local.Valid()
	default:
		return false
	}
}
func (s InstanceSettings) ID() InstanceID     { return s.id }
func (s InstanceSettings) Kind() ProviderKind { return s.kind }
func (s InstanceSettings) Enabled() bool      { return s.enabled }
func (s InstanceSettings) Local() (LocalSettings, error) {
	if !s.Valid() || s.kind != Local {
		return LocalSettings{}, NewError(InvalidInput)
	}
	return s.local, nil
}
func (InstanceSettings) String() string                 { return "backup.InstanceSettings(<private>)" }
func (s InstanceSettings) GoString() string             { return s.String() }
func (s InstanceSettings) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(s.String())) }
func (s InstanceSettings) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// An explicitly empty configuration disables all providers. Its valid bit
// distinguishes that deliberate choice from an uninitialized Config.
type Configuration struct {
	instances []InstanceSettings
	valid     bool
}

func NewConfiguration(instances []InstanceSettings) (Configuration, error) {
	if len(instances) > MaxInstances {
		return Configuration{}, NewError(Capacity)
	}
	seen := make(map[InstanceID]bool, len(instances))
	for _, instance := range instances {
		if !instance.Valid() || seen[instance.ID()] {
			return Configuration{}, NewError(InvalidInput)
		}
		seen[instance.ID()] = true
	}
	return Configuration{instances: append([]InstanceSettings(nil), instances...), valid: true}, nil
}
func (c Configuration) Valid() bool { return c.valid }
func (c Configuration) Instances() []InstanceSettings {
	return append([]InstanceSettings(nil), c.instances...)
}
func (Configuration) String() string                 { return "backup.Configuration(<private>)" }
func (c Configuration) GoString() string             { return c.String() }
func (c Configuration) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(c.String())) }
func (c Configuration) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }
