package hilo

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrSettings = errors.New("hilo: invalid settings")

type BindingInput struct {
	Alias    string `yaml:"alias"`
	DeviceID int    `yaml:"device_id"`
}
type Input struct {
	Instance            string         `yaml:"instance"`
	AccountID           string         `yaml:"account_id"`
	LocationID          int            `yaml:"location_id"`
	LocationHiloID      string         `yaml:"location_hilo_id"`
	CredentialSecret    string         `yaml:"credential_secret"`
	DailyLimit          int            `yaml:"daily_limit"`
	DayZone             string         `yaml:"day_zone"`
	LeaseWindowSeconds  int            `yaml:"lease_window_seconds"`
	EnergyMaxGapSeconds int            `yaml:"energy_max_gap_seconds"`
	APIBaseURL          string         `yaml:"api_base_url"`
	PlatformBaseURL     string         `yaml:"platform_base_url"`
	Bindings            []BindingInput `yaml:"bindings"`
}

type Binding struct {
	alias    string
	kind     AliasKind
	deviceID int
	key      integrations.SourceKey
}

func (b Binding) Alias() string                     { return b.alias }
func (b Binding) Kind() AliasKind                   { return b.kind }
func (b Binding) DeviceID() int                     { return b.deviceID }
func (b Binding) SourceKey() integrations.SourceKey { return b.key }

type EnergyPair struct{ SourceAlias, OutputAlias string }

type Settings struct {
	instance       domain.IntegrationInstanceID
	account        integrations.CloudAccountID
	accountText    string
	locationID     int
	locationHiloID string
	credential     storage.SecretHandle
	dailyLimit     int
	dayZone        string
	leaseWindow    time.Duration
	energyMaxGap   time.Duration
	apiBase        url.URL
	platformBase   url.URL
	bindings       []Binding
}

func ParseSettings(in Input) (Settings, error) {
	instance, err := domain.ParseIntegrationInstanceID(in.Instance)
	if err != nil {
		return Settings{}, ErrSettings
	}
	account, err := integrations.ParseCloudAccountID(in.AccountID)
	if err != nil || len(in.AccountID) > 128 || strings.ContainsAny(in.AccountID, "/?#\r\n") {
		return Settings{}, ErrSettings
	}
	if in.LocationID <= 0 || in.LocationID > 1<<31-1 || in.LocationHiloID == "" || len(in.LocationHiloID) > 256 || strings.ContainsAny(in.LocationHiloID, "\r\n") {
		return Settings{}, ErrSettings
	}
	secretName, err := storage.ParseSecretName(in.CredentialSecret)
	if err != nil {
		return Settings{}, ErrSettings
	}
	credential, err := storage.NewSecretHandle(instance, secretName)
	if err != nil {
		return Settings{}, ErrSettings
	}
	if in.DailyLimit < 1 || in.DailyLimit > 30000 {
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
	window := in.LeaseWindowSeconds
	if window == 0 {
		window = 180
	}
	if window < 120 || window > 3600 {
		return Settings{}, ErrSettings
	}
	if in.EnergyMaxGapSeconds < window || in.EnergyMaxGapSeconds > 86400 {
		return Settings{}, ErrSettings
	}
	apiBase, err := parseBaseURL(in.APIBaseURL, "https://api.hiloenergie.com")
	if err != nil {
		return Settings{}, ErrSettings
	}
	platformBase, err := parseBaseURL(in.PlatformBaseURL, "https://platform.hiloenergie.com")
	if err != nil {
		return Settings{}, ErrSettings
	}
	if len(in.Bindings) != 37 {
		return Settings{}, ErrSettings
	}
	devices := make(map[string]int, len(in.Bindings))
	seen := make(map[string]bool, len(in.Bindings))
	for _, row := range in.Bindings {
		entry := classifyAlias(row.Alias)
		if !entry.Kind.Direct() || entry.Kind == AliasNotifications || entry.Kind == AliasWeather || row.DeviceID <= 0 || seen[row.Alias] {
			return Settings{}, ErrSettings
		}
		seen[row.Alias] = true
		devices[row.Alias] = row.DeviceID
	}
	bindings := make([]Binding, 0, 48)
	for _, entry := range retainedProfile {
		if entry.Kind == AliasEnergy {
			continue
		}
		id := devices[entry.Alias]
		if entry.Kind.Direct() && entry.Kind != AliasNotifications && entry.Kind != AliasWeather && id == 0 {
			return Settings{}, ErrSettings
		}
		device := "hilo-account-" + in.AccountID
		if id != 0 {
			device = "hilo-device-" + strconv.Itoa(id)
		}
		key, err := integrations.NewSourceKey(device, entry.Alias)
		if err != nil {
			return Settings{}, ErrSettings
		}
		bindings = append(bindings, Binding{alias: entry.Alias, kind: entry.Kind, deviceID: id, key: key})
	}
	return Settings{instance, account, in.AccountID, in.LocationID, in.LocationHiloID, credential, in.DailyLimit, zone, time.Duration(window) * time.Second, time.Duration(in.EnergyMaxGapSeconds) * time.Second, apiBase, platformBase, bindings}, nil
}

func parseBaseURL(raw, fallback string) (url.URL, error) {
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return url.URL{}, ErrSettings
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if loopback {
		if u.Scheme != "http" && u.Scheme != "https" {
			return url.URL{}, ErrSettings
		}
	} else {
		provider, err := url.Parse(fallback)
		if err != nil || u.Scheme != "https" || u.Hostname() != provider.Hostname() || u.Port() != "" && u.Port() != "443" {
			return url.URL{}, ErrSettings
		}
	}
	return *u, nil
}

func (s Settings) Valid() bool {
	_, err := s.Instance()
	return err == nil && len(s.bindings) == 48 && s.locationID > 0 && s.leaseWindow > 0
}
func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if _, err := s.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, ErrSettings
	}
	return s.instance, nil
}
func (s Settings) Account() integrations.CloudAccountID   { return s.account }
func (s Settings) AccountText() string                    { return s.accountText }
func (s Settings) Budget() (int, string)                  { return s.dailyLimit, s.dayZone }
func (s Settings) Location() (int, string)                { return s.locationID, s.locationHiloID }
func (s Settings) CredentialHandle() storage.SecretHandle { return s.credential }
func (s Settings) LeaseWindow() time.Duration             { return s.leaseWindow }
func (s Settings) EnergyMaxGap() time.Duration            { return s.energyMaxGap }
func (s Settings) Bindings() []Binding                    { return append([]Binding(nil), s.bindings...) }
func (s Settings) EnergyPairs() []EnergyPair {
	out := make([]EnergyPair, 0, 10)
	for _, entry := range retainedProfile {
		if entry.Kind == AliasEnergy {
			out = append(out, EnergyPair{entry.Source, entry.Alias})
		}
	}
	return out
}
func (s Settings) APIEndpoint(path string) string { u := s.apiBase; u.Path = path; return u.String() }
func (s Settings) PlatformEndpoint(path string) string {
	u := s.platformBase
	u.Path = path
	return u.String()
}
