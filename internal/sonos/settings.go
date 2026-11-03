// Package sonos observes explicitly configured Sonos players over local UPnP
// and accepts bounded, Host-admitted standalone playback and volume commands.
package sonos

import (
	"errors"
	"net/netip"
	"regexp"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

const Schema = "home.media.v1"
const Endpoint = "player"
const VolumeEndpoint = "volume"

var ErrSettings = errors.New("sonos: invalid configuration")
var ErrUnavailable = errors.New("sonos: observation unavailable")
var uidPattern = regexp.MustCompile(`^RINCON_[0-9A-F]{17}$`)

type PlayerInput struct {
	Address string `yaml:"address"`
	UID     string `yaml:"uid"`
	Alias   string `yaml:"alias"`
	Label   string `yaml:"label"`
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
	id, err := domain.ParseIntegrationInstanceID(in.Instance)
	if err != nil || len(in.Players) == 0 || len(in.Players) > 16 {
		return Settings{}, ErrSettings
	}
	seen := map[string]bool{}
	for _, p := range in.Players {
		ip, e := netip.ParseAddr(p.Address)
		_, a := domain.ParseHAAlias(p.Alias)
		if e != nil || !ip.Is4() || !ip.IsPrivate() || a != nil || len(p.Alias) < 14 || p.Alias[:13] != "media_player." || !uidPattern.MatchString(p.UID) || len(p.Label) == 0 || len(p.Label) > 128 {
			return Settings{}, ErrSettings
		}
		for _, r := range p.Label {
			if r < 32 || r == 127 {
				return Settings{}, ErrSettings
			}
		}
		for _, key := range []string{"ip:" + p.Address, "uid:" + p.UID, "alias:" + p.Alias} {
			if seen[key] {
				return Settings{}, ErrSettings
			}
			seen[key] = true
		}
	}
	return Settings{id, append([]PlayerInput(nil), in.Players...)}, nil
}
func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if _, e := s.instance.Value(); e != nil || len(s.players) == 0 {
		return domain.IntegrationInstanceID{}, ErrSettings
	}
	return s.instance, nil
}
func (s Settings) Players() []PlayerInput { return append([]PlayerInput(nil), s.players...) }
func (p PlayerInput) SourceKey() integrations.SourceKey {
	key, _ := integrations.NewSourceKey(p.UID, "player")
	return key
}
