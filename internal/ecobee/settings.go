// Package ecobee implements the single, explicitly configured ECB601 profile.
// It never discovers LAN services, performs pair-setup, or replays commands.
package ecobee

import (
	"errors"
	"net/netip"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrSettings = errors.New("ecobee: invalid configured profile")
var ErrCredential = errors.New("ecobee: invalid pairing credential")
var ErrUnavailable = errors.New("ecobee: transport unavailable")
var ErrCommandUncertain = errors.New("ecobee: emitted command has no compatible readback")

const Alias = "climate.home"
const Endpoint = "climate"

// Input contains locators only. Pairing identity and keys never enter YAML.
type Input struct {
	Instance         string `yaml:"instance"`
	Address          string `yaml:"address"`
	AccessoryID      string `yaml:"accessory_id"`
	CredentialSecret string `yaml:"credential_secret"`
}

type Settings struct {
	instance  domain.IntegrationInstanceID
	address   netip.AddrPort
	accessory string
	handle    storage.SecretHandle
	key       integrations.SourceKey
	valid     bool
}

func ParseSettings(in Input) (Settings, error) {
	instance, err := domain.ParseIntegrationInstanceID(in.Instance)
	if err != nil {
		return Settings{}, ErrSettings
	}
	address, err := netip.ParseAddrPort(in.Address)
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" || address.Addr().IsUnspecified() || address.Addr().IsMulticast() {
		return Settings{}, ErrSettings
	}
	if in.AccessoryID == "" || len(in.AccessoryID) > 128 {
		return Settings{}, ErrSettings
	}
	name, err := storage.ParseSecretName(in.CredentialSecret)
	if err != nil {
		return Settings{}, ErrSettings
	}
	handle, err := storage.NewSecretHandle(instance, name)
	if err != nil {
		return Settings{}, ErrSettings
	}
	key, err := integrations.NewSourceKey(in.AccessoryID, "thermostat")
	if err != nil {
		return Settings{}, ErrSettings
	}
	return Settings{instance, address, in.AccessoryID, handle, key, true}, nil
}
func (s Settings) Valid() bool { return s.valid }
func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if !s.valid {
		return domain.IntegrationInstanceID{}, ErrSettings
	}
	return s.instance, nil
}
func (s Settings) SourceKey() integrations.SourceKey      { return s.key }
func (s Settings) CredentialHandle() storage.SecretHandle { return s.handle }
func (s Settings) Address() string                        { return s.address.String() }
