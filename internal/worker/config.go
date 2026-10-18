// Package worker adapts one supervised local process to the session Host.
package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrInvalidConfig = errors.New("worker: invalid local worker configuration")
var ErrUnavailable = errors.New("worker: session or process unavailable")
var ErrCapacity = errors.New("worker: session capacity exhausted")
var ErrWorkerExit = errors.New("worker: process exited")
var ErrHeartbeat = errors.New("worker: heartbeat lease expired")
var ErrCommandFailed = errors.New("worker: command failed")
var ErrCasetaHAProfileUnavailable = errors.New("worker: configured Caseta HA light profile requires Host 1.3 and its qualified light descriptor")
var ErrCasetaHAFanProfileUnavailable = errors.New("worker: configured Caseta HA four-speed fan profile requires Host 1.1 or 1.3 and its qualified fan descriptor")
var ErrJoinTimeout = errors.New("worker: process join timed out")

type Config struct {
	instance    domain.IntegrationInstanceID
	executable  string
	args        []string
	secretNames map[string]struct{}
	bindings    []ConfiguredBinding
}

// ConfiguredBinding is an operator-supplied source-to-manifest mapping. The
// registry remains the sole identity authority and persists it idempotently.
type ConfiguredBinding struct {
	key          integrations.SourceKey
	alias        domain.HAAlias
	lightProfile configuredLightProfile
	fanProfile   configuredFanProfile
}

// Only profiles explicitly configured for a binding may cross the worker/Host
// boundary. The executable path and arguments are not profile evidence.
type configuredLightProfile uint8

const (
	configuredLightProfileNone configuredLightProfile = iota
	configuredLightProfileCasetaHA100V1
)

type configuredFanProfile uint8

const (
	configuredFanProfileNone configuredFanProfile = iota
	configuredFanProfileCasetaHA4V1
)

func NewConfiguredBinding(device, entity, alias string) (ConfiguredBinding, error) {
	key, err := integrations.NewSourceKey(device, entity)
	if err != nil {
		return ConfiguredBinding{}, ErrInvalidConfig
	}
	label, err := domain.ParseHAAlias(alias)
	if err != nil {
		return ConfiguredBinding{}, ErrInvalidConfig
	}
	return ConfiguredBinding{key: key, alias: label}, nil
}

func (b ConfiguredBinding) Components() (integrations.SourceKey, string, error) {
	if _, _, err := b.key.Components(); err != nil {
		return integrations.SourceKey{}, "", ErrInvalidConfig
	}
	alias, err := b.alias.Value()
	if err != nil || b.lightProfile > configuredLightProfileCasetaHA100V1 || b.fanProfile > configuredFanProfileCasetaHA4V1 ||
		b.lightProfile != configuredLightProfileNone && b.fanProfile != configuredFanProfileNone {
		return integrations.SourceKey{}, "", ErrInvalidConfig
	}
	return b.key, alias, nil
}

// WithCasetaHA100V1 opts this exact SourceKey and alias into the pinned HA
// Caseta percentage projection. Host 1.3 is checked after worker handshake.
func (b ConfiguredBinding) WithCasetaHA100V1() (ConfiguredBinding, error) {
	_, alias, err := b.Components()
	if err != nil || b.lightProfile != configuredLightProfileNone || !strings.HasPrefix(alias, "light.") {
		return ConfiguredBinding{}, ErrInvalidConfig
	}
	b.lightProfile = configuredLightProfileCasetaHA100V1
	return b, nil
}

func (b ConfiguredBinding) CasetaHA100V1() bool {
	return b.lightProfile == configuredLightProfileCasetaHA100V1
}

// WithCasetaHAFan4V1 qualifies only this configured SourceKey and fan alias.
// The actual protocol and descriptor are checked after handshake/discovery.
func (b ConfiguredBinding) WithCasetaHAFan4V1() (ConfiguredBinding, error) {
	_, alias, err := b.Components()
	if err != nil || b.lightProfile != configuredLightProfileNone || b.fanProfile != configuredFanProfileNone || !strings.HasPrefix(alias, "fan.") {
		return ConfiguredBinding{}, ErrInvalidConfig
	}
	b.fanProfile = configuredFanProfileCasetaHA4V1
	return b, nil
}

func (b ConfiguredBinding) CasetaHAFan4V1() bool {
	return b.fanProfile == configuredFanProfileCasetaHA4V1
}

func (c Config) WithBindings(bindings []ConfiguredBinding) (Config, error) {
	if _, err := c.Instance(); err != nil || len(bindings) > 256 {
		return Config{}, ErrInvalidConfig
	}
	keys := make(map[integrations.SourceKey]bool, len(bindings))
	aliases := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		key, alias, err := binding.Components()
		if err != nil || keys[key] || aliases[alias] {
			return Config{}, ErrInvalidConfig
		}
		keys[key], aliases[alias] = true, true
	}
	c.bindings = append([]ConfiguredBinding(nil), bindings...)
	return c, nil
}

func (c Config) Bindings() []ConfiguredBinding {
	return append([]ConfiguredBinding(nil), c.bindings...)
}

func NewConfig(instanceText, executable string, args, secretNames []string) (Config, error) {
	instance, err := domain.ParseIntegrationInstanceID(instanceText)
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || strings.ContainsRune(executable, '\x00') || len(args) > 16 || len(secretNames) > 32 {
		return Config{}, ErrInvalidConfig
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return Config{}, ErrInvalidConfig
	}
	for _, arg := range args {
		if len(arg) > 4096 || strings.ContainsRune(arg, '\x00') {
			return Config{}, ErrInvalidConfig
		}
	}
	names := make(map[string]struct{}, len(secretNames))
	for _, raw := range secretNames {
		if _, err := storage.ParseSecretName(raw); err != nil {
			return Config{}, ErrInvalidConfig
		}
		if _, exists := names[raw]; exists {
			return Config{}, ErrInvalidConfig
		}
		names[raw] = struct{}{}
	}
	return Config{instance: instance, executable: executable, args: append([]string(nil), args...), secretNames: names}, nil
}

func (c Config) Instance() (domain.IntegrationInstanceID, error) {
	if _, err := c.instance.Value(); err != nil || c.executable == "" {
		return domain.IntegrationInstanceID{}, ErrInvalidConfig
	}
	return c.instance, nil
}

func NewPilotSpec(c Config) (integrations.PilotSpec, error) {
	instance, err := c.Instance()
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, func() integrations.Pilot {
		pilot, _ := NewPilot(c)
		return pilot
	})
}

// NewPilot is the same validated factory used by NewPilotSpec. A test Adapter
// may wrap its SessionHost while leaving the production process and RPC path intact.
func NewPilot(c Config) (integrations.Pilot, error) {
	if _, err := c.Instance(); err != nil {
		return nil, err
	}
	return newPilot(c), nil
}
