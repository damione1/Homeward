// Package wled implements the native local WLED JSON protocol. Configuration
// pins hardware identity and explicitly separates controller and segment routes.
package wled

import (
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

var ErrSettings = errors.New("wled: invalid settings")
var ErrUnavailable = errors.New("wled: observation or transport unavailable")
var macPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

type RouteInput struct {
	Alias string `yaml:"alias"`
	Label string `yaml:"label"`
	// Omitted means the controller. Zero explicitly means segment zero.
	Segment *uint32 `yaml:"segment"`
}
type ControllerInput struct {
	Address string       `yaml:"address"`
	MAC     string       `yaml:"mac"`
	Routes  []RouteInput `yaml:"routes"`
}
type Input struct {
	Instance    string            `yaml:"instance"`
	Controllers []ControllerInput `yaml:"controllers"`
}
type Settings struct {
	instance    domain.IntegrationInstanceID
	controllers []ControllerInput
}

func ParseSettings(in Input) (Settings, error) {
	id, e := domain.ParseIntegrationInstanceID(in.Instance)
	if e != nil || len(in.Controllers) == 0 || len(in.Controllers) > 16 {
		return Settings{}, ErrSettings
	}
	seen := map[string]bool{}
	for _, c := range in.Controllers {
		ip, e := netip.ParseAddr(c.Address)
		if e != nil || !ip.Is4() || !ip.IsPrivate() || !macPattern.MatchString(c.MAC) || len(c.Routes) == 0 || len(c.Routes) > 33 {
			return Settings{}, ErrSettings
		}
		for _, k := range []string{"ip:" + c.Address, "mac:" + c.MAC} {
			if seen[k] {
				return Settings{}, ErrSettings
			}
			seen[k] = true
		}
		for _, r := range c.Routes {
			_, e := domain.ParseHAAlias(r.Alias)
			if e != nil || !strings.HasPrefix(r.Alias, "light.") || len(r.Label) == 0 || len(r.Label) > 128 || r.Segment != nil && *r.Segment > 31 {
				return Settings{}, ErrSettings
			}
			for _, v := range r.Label {
				if v < 32 || v == 127 {
					return Settings{}, ErrSettings
				}
			}
			for _, k := range []string{"alias:" + r.Alias, "route:" + c.MAC + ":" + r.component()} {
				if seen[k] {
					return Settings{}, ErrSettings
				}
				seen[k] = true
			}
		}
	}
	s := Settings{instance: id, controllers: in.Controllers}
	s.controllers = s.Controllers()
	return s, nil
}
func (r RouteInput) component() string {
	if r.Segment == nil {
		return "controller"
	}
	return "segment-" + strconv.FormatUint(uint64(*r.Segment), 10)
}
func (r RouteInput) SourceKey(mac string) integrations.SourceKey {
	key, _ := integrations.NewSourceKey(mac, r.component())
	return key
}
func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if _, e := s.instance.Value(); e != nil || len(s.controllers) == 0 {
		return domain.IntegrationInstanceID{}, ErrSettings
	}
	return s.instance, nil
}
func (s Settings) Controllers() []ControllerInput {
	out := append([]ControllerInput(nil), s.controllers...)
	for i := range out {
		out[i].Routes = append([]RouteInput(nil), out[i].Routes...)
		for j := range out[i].Routes {
			if p := out[i].Routes[j].Segment; p != nil {
				v := *p
				out[i].Routes[j].Segment = &v
			}
		}
	}
	return out
}
func Factory(s Settings) (integrations.PilotFactory, error) {
	if _, e := s.Instance(); e != nil {
		return nil, e
	}
	return func() integrations.Pilot { return &pilot{settings: s, client: newClient()} }, nil
}
