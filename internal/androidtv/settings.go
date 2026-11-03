// Package androidtv observes explicitly configured Android TV Remote v2 devices.
// It does not use ADB, Home Assistant, or replayable remote key queues.
package androidtv

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"unicode"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

const Schema = "androidtv.media.v1"
const Endpoint = "player"
const PowerEndpoint = "power"

var ErrSettings = errors.New("androidtv: invalid configuration")
var ErrCredential = errors.New("androidtv: invalid or untrusted pairing credential")
var ErrUnavailable = errors.New("androidtv: observation unavailable")
var ErrProtocol = errors.New("androidtv: invalid remote protocol")

type PlayerInput struct {
	Address          string `yaml:"address"`
	MAC              string `yaml:"mac"`
	Alias            string `yaml:"alias"`
	Label            string `yaml:"label"`
	CredentialSecret string `yaml:"credential_secret"`
	// IME can make some devices display their mobile-keyboard screen. It is
	// opt-in and is only used to observe the foreground application.
	EnableIME bool `yaml:"enable_ime"`
}
type Input struct {
	Instance string        `yaml:"instance"`
	Players  []PlayerInput `yaml:"players"`
}
type Settings struct {
	instance domain.IntegrationInstanceID
	players  []PlayerInput
}

func ParseSettings(in Input) (Settings, error) {
	id, e := domain.ParseIntegrationInstanceID(in.Instance)
	if e != nil || len(in.Players) == 0 || len(in.Players) > 16 {
		return Settings{}, ErrSettings
	}
	players := append([]PlayerInput(nil), in.Players...)
	seen := map[string]bool{}
	for i, p := range players {
		ip, e := netip.ParseAddr(p.Address)
		mac, m := net.ParseMAC(p.MAC)
		_, a := domain.ParseHAAlias(p.Alias)
		_, s := storage.ParseSecretName(p.CredentialSecret)
		if e != nil || !ip.Is4() || !ip.IsPrivate() || m != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" || a != nil || !strings.HasPrefix(p.Alias, "media_player.") || s != nil || p.Label == "" || len(p.Label) > 128 || strings.IndexFunc(p.Label, unicode.IsControl) >= 0 {
			return Settings{}, ErrSettings
		}
		players[i].MAC = mac.String()
		for _, key := range []string{"ip:" + p.Address, "mac:" + mac.String(), "alias:" + p.Alias} {
			if seen[key] {
				return Settings{}, ErrSettings
			}
			seen[key] = true
		}
	}
	return Settings{id, players}, nil
}
func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if _, e := s.instance.Value(); e != nil || len(s.players) == 0 {
		return domain.IntegrationInstanceID{}, ErrSettings
	}
	return s.instance, nil
}
func (s Settings) Players() []PlayerInput { return append([]PlayerInput(nil), s.players...) }
func (p PlayerInput) SourceKey() integrations.SourceKey {
	k, _ := integrations.NewSourceKey(p.MAC, Endpoint)
	return k
}
func (p PlayerInput) CredentialHandle(instance domain.IntegrationInstanceID) (storage.SecretHandle, error) {
	n, e := storage.ParseSecretName(p.CredentialSecret)
	if e != nil {
		return storage.SecretHandle{}, ErrSettings
	}
	return storage.NewSecretHandle(instance, n)
}
