// Package neviweb adapts the pinned sinope-130 v4.3.1 Neviweb contract to
// Homeward's Go Host. Adapted behavior is GPLv3; see provenance.go.
package neviweb

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrSettings = errors.New("neviweb: invalid configured account or binding")
var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var retainedAliases = []string{
	"climate.neviweb130_climate_chambre_eugenie",
	"climate.neviweb130_climate_chambre_principale",
	"climate.neviweb130_climate_chambre_sous_sol",
	"climate.neviweb130_climate_living_room",
	"climate.neviweb130_climate_plancher_cuisine",
	"climate.neviweb130_climate_salle_a_manger",
	"climate.neviweb130_climate_salle_de_bain",
	"climate.neviweb130_climate_sous_sol",
	"switch.neviweb130_switch_chauffe_eau",
	"sensor.neviweb130_daily_requests",
	"sensor.neviweb130_sensor_tableau_elec",
}

type Input struct {
	Instance        string         `yaml:"instance"`
	BaseURL         string         `yaml:"base_url"`
	AccountID       string         `yaml:"account_id"`
	NetworkID       string         `yaml:"network_id"`
	UsernameSecret  string         `yaml:"username_secret"`
	PasswordSecret  string         `yaml:"password_secret"`
	SessionSecret   string         `yaml:"session_secret"`
	PollSeconds     int            `yaml:"poll_seconds"`
	DailyLimit      int            `yaml:"daily_limit"`
	ExternalReserve *int           `yaml:"external_reserve"`
	DayZone         string         `yaml:"day_zone"`
	Bindings        []BindingInput `yaml:"bindings"`
}

type BindingInput struct {
	Alias    string `yaml:"alias"`
	DeviceID string `yaml:"device_id"`
}
type BindingKind uint8

const (
	BindingUnknown BindingKind = iota
	BindingClimate
	BindingSwitch
	BindingDailyCount
	BindingGateway
)

type Binding struct {
	alias    string
	deviceID string
	kind     BindingKind
	key      integrations.SourceKey
}

func (b Binding) Alias() string                     { return b.alias }
func (b Binding) DeviceID() string                  { return b.deviceID }
func (b Binding) Kind() BindingKind                 { return b.kind }
func (b Binding) SourceKey() integrations.SourceKey { return b.key }

type Settings struct {
	instance                    domain.IntegrationInstanceID
	baseURL                     url.URL
	account                     integrations.CloudAccountID
	accountText                 string
	networkID                   string
	username, password, session storage.SecretHandle
	poll                        time.Duration
	dailyLimit, externalReserve int
	dayZone                     string
	bindings                    []Binding
}

func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if _, err := s.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, ErrSettings
	}
	return s.instance, nil
}
func (s Settings) Bindings() []Binding                  { return append([]Binding(nil), s.bindings...) }
func (s Settings) PollInterval() time.Duration          { return s.poll }
func (s Settings) Budget() (int, int, string)           { return s.dailyLimit, s.externalReserve, s.dayZone }
func (s Settings) Account() integrations.CloudAccountID { return s.account }
func (s Settings) AccountText() string                  { return s.accountText }
func (s Settings) SessionHandle() storage.SecretHandle  { return s.session }

func ParseSettings(in Input) (Settings, error) {
	instance, err := domain.ParseIntegrationInstanceID(in.Instance)
	if err != nil {
		return Settings{}, ErrSettings
	}
	base := in.BaseURL
	if base == "" {
		base = "https://neviweb.com"
	}
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return Settings{}, ErrSettings
	}
	if u.Scheme != "https" {
		host, _, e := net.SplitHostPort(u.Host)
		if e != nil {
			host = u.Hostname()
		}
		if u.Scheme != "http" || (host != "localhost" && net.ParseIP(host) == nil) {
			return Settings{}, ErrSettings
		}
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			return Settings{}, ErrSettings
		}
	}
	account, err := integrations.ParseCloudAccountID(in.AccountID)
	if err != nil || !deviceIDPattern.MatchString(in.AccountID) || !deviceIDPattern.MatchString(in.NetworkID) {
		return Settings{}, ErrSettings
	}
	secret := func(raw string) (storage.SecretHandle, error) {
		name, e := storage.ParseSecretName(raw)
		if e != nil {
			return storage.SecretHandle{}, e
		}
		return storage.NewSecretHandle(instance, name)
	}
	username, err := secret(in.UsernameSecret)
	if err != nil {
		return Settings{}, ErrSettings
	}
	password, err := secret(in.PasswordSecret)
	if err != nil {
		return Settings{}, ErrSettings
	}
	session, err := secret(in.SessionSecret)
	if err != nil || in.UsernameSecret == in.PasswordSecret || in.UsernameSecret == in.SessionSecret || in.PasswordSecret == in.SessionSecret {
		return Settings{}, ErrSettings
	}
	poll := in.PollSeconds
	if poll == 0 {
		poll = 540
	}
	if poll < 300 || poll >= 600 {
		return Settings{}, ErrSettings
	}
	limit := in.DailyLimit
	if limit == 0 {
		limit = 5000
	}
	reserve := 25000
	// A zero reserve is an explicit statement by the operator that Homeward
	// may use the whole advertised community envelope.
	if in.ExternalReserve != nil {
		reserve = *in.ExternalReserve
	}
	if limit < 1 || limit > 30000 || reserve < 0 || reserve >= 30000 || limit+reserve > 30000 {
		return Settings{}, ErrSettings
	}
	zone := in.DayZone
	if zone == "" {
		zone = "America/Toronto"
	}
	if zone == "Local" {
		return Settings{}, ErrSettings
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return Settings{}, ErrSettings
	}
	if len(in.Bindings) != len(retainedAliases) {
		return Settings{}, ErrSettings
	}
	allowed := make(map[string]BindingKind, len(retainedAliases))
	for i, a := range retainedAliases {
		kind := BindingClimate
		if i == 8 {
			kind = BindingSwitch
		} else if i == 9 {
			kind = BindingDailyCount
		} else if i == 10 {
			kind = BindingGateway
		}
		allowed[a] = kind
	}
	seen := make(map[string]bool, len(retainedAliases))
	devices := make(map[string]bool, 10)
	bindings := make([]Binding, 0, len(retainedAliases))
	for _, item := range in.Bindings {
		kind, ok := allowed[item.Alias]
		if !ok || seen[item.Alias] {
			return Settings{}, ErrSettings
		}
		seen[item.Alias] = true
		if kind == BindingDailyCount {
			if item.DeviceID != "" {
				return Settings{}, ErrSettings
			}
		} else {
			if !deviceIDPattern.MatchString(item.DeviceID) || devices[item.DeviceID] {
				return Settings{}, ErrSettings
			}
			devices[item.DeviceID] = true
		}
		device := item.DeviceID
		entity := "climate"
		if kind == BindingDailyCount {
			device = "account-" + in.AccountID
			entity = "daily_requests"
		} else if kind == BindingSwitch {
			entity = "switch"
		} else if kind == BindingGateway {
			entity = "connectivity"
		}
		key, e := integrations.NewSourceKey("neviweb-"+device, entity)
		if e != nil {
			return Settings{}, ErrSettings
		}
		bindings = append(bindings, Binding{alias: item.Alias, deviceID: item.DeviceID, kind: kind, key: key})
	}
	for _, alias := range retainedAliases {
		if !seen[alias] {
			return Settings{}, ErrSettings
		}
	}
	return Settings{instance: instance, baseURL: *u, account: account, accountText: in.AccountID, networkID: in.NetworkID, username: username, password: password, session: session, poll: time.Duration(poll) * time.Second, dailyLimit: limit, externalReserve: reserve, dayZone: zone, bindings: bindings}, nil
}

func (s Settings) endpoint(path string) string { u := s.baseURL; u.Path = path; return u.String() }
func (s Settings) endpointQuery(path string, query url.Values) string {
	u := s.baseURL
	u.Path = path
	u.RawQuery = query.Encode()
	return u.String()
}
func (s Settings) valid() bool {
	_, err := s.Instance()
	return err == nil && s.poll > 0 && len(s.bindings) == len(retainedAliases) && !strings.Contains(s.networkID, "/")
}
