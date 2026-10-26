package server

// Synthetic readings owned by the isolated Go fixture. Nothing in this file
// is linked into homed; the client and kits have no installation catalogue.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/contracts"
	env "github.com/damione1/Homeward/gen/home/environment/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type nhf010DeviceKind string

const (
	nhf010Weather  nhf010DeviceKind = "weather"
	nhf010Presence nhf010DeviceKind = "presence"
	nhf010Details  nhf010DeviceKind = "details"
	nhf010Solar    nhf010DeviceKind = "solar"
)

func (k *nhf010DeviceKind) UnmarshalText(text []byte) error {
	switch value := nhf010DeviceKind(text); value {
	case nhf010Weather, nhf010Presence, nhf010Details, nhf010Solar:
		*k = value
		return nil
	default:
		return errors.New("unknown synthetic environmental family")
	}
}

type nhf010Sample uint8

const (
	nhf010Nominal nhf010Sample = iota + 1
	nhf010Stale
	nhf010Unknown
	nhf010Absent
	nhf010Missing
)

type nhf010Device struct {
	kind            nhf010DeviceKind
	alias           domain.HAAlias
	key             integrations.SourceKey
	title, endpoint string
}

func nhf010ReadDevices(file string) ([]nhf010Device, error) {
	input, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	raw, err := io.ReadAll(io.LimitReader(input, 16385))
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 {
		return nil, errors.New("environment fixture exceeds bound")
	}
	var rows []struct {
		Kind     nhf010DeviceKind `json:"kind"`
		Alias    string           `json:"alias"`
		Title    string           `json:"title"`
		Endpoint string           `json:"endpoint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing environment fixture")
	}
	if len(rows) != 4 {
		return nil, errors.New("four environmental families required")
	}
	kinds, aliases := make(map[nhf010DeviceKind]bool), make(map[string]bool)
	var result []nhf010Device
	for _, row := range rows {
		if row.Kind == "" || kinds[row.Kind] || aliases[row.Alias] || row.Title == "" || len(row.Title) > 128 {
			return nil, errors.New("invalid environment fixture identity")
		}
		kinds[row.Kind] = true
		aliases[row.Alias] = true
		alias, err := domain.ParseHAAlias(row.Alias)
		if err != nil {
			return nil, err
		}
		key, err := integrations.NewSourceKey(string(row.Kind), row.Endpoint)
		if err != nil {
			return nil, err
		}
		result = append(result, nhf010Device{row.Kind, alias, key, row.Title, row.Endpoint})
	}
	return result, nil
}
func (d nhf010Device) capability() *registrywire.CapabilityDescription {
	result := &registrywire.CapabilityDescription{Major: 1, Endpoint: d.endpoint, DisplayName: d.title}
	switch d.kind {
	case nhf010Weather:
		result.SchemaId = contracts.WeatherSchema
		result.Constraints = &registrywire.CapabilityDescription_Weather{Weather: &env.WeatherConstraints{}}
	case nhf010Presence:
		result.SchemaId = contracts.PresenceSchema
		result.Constraints = &registrywire.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}
	case nhf010Details:
		result.SchemaId = contracts.PresenceDetailsSchema
		result.Constraints = &registrywire.CapabilityDescription_PresenceDetails{PresenceDetails: &env.PresenceDetailsConstraints{}}
	case nhf010Solar:
		result.SchemaId = contracts.SolarEventSchema
		result.Constraints = &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_RISING}}
	default:
		panic("unparsed environmental family")
	}
	return result
}
func (d nhf010Device) observation(sample nhf010Sample, received time.Time) (integrations.StateObservation, error) {
	switch sample {
	case nhf010Nominal, nhf010Stale, nhf010Unknown, nhf010Absent, nhf010Missing:
	default:
		return integrations.StateObservation{}, errors.New("unknown environmental sample")
	}
	at := received.UTC()
	freshness := domain.FreshnessFresh
	availability := domain.AvailabilityAvailable
	if sample == nhf010Stale {
		at = at.Add(-2 * time.Hour)
		freshness = domain.FreshnessStale
	}
	if sample == nhf010Missing {
		availability = domain.AvailabilityUnknown
	}
	quality, err := domain.NewQuality(availability, freshness, domain.ProvenanceEstimated)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	if sample == nhf010Missing {
		return integrations.NewStateObservation(d.endpoint, quality, nil, nil, received, nil)
	}
	var value domain.StateValue
	switch d.kind {
	case nhf010Weather:
		condition := domain.WeatherRainy
		if sample == nhf010Unknown {
			condition = domain.WeatherUnknown
		}
		issued := at.Add(-time.Hour)
		weather, e := domain.NewWeatherValue(domain.WeatherParts{
			Condition: &condition, Temperature: &domain.Temperature{Value: 0, Unit: domain.TemperatureCelsius}, HumidityPercent: nhf008Pointer(0.),
			Wind: &domain.Wind{Speed: 0, Unit: domain.SpeedKilometersPerHour}, SourceLabel: "Fournisseur météo simulé",
			Forecast: &domain.Forecast{IssuedAt: issued, ValidUntil: at.Add(time.Hour), Periods: []domain.ForecastPeriod{
				{StartsAt: at, EndsAt: at.Add(time.Hour), Kind: domain.ForecastHourly, Condition: &condition, Temperature: &domain.Temperature{Value: 0, Unit: domain.TemperatureCelsius}, PrecipitationProbabilityPercent: nhf008Pointer(0.)},
			}},
		})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewWeatherStateValue(weather)
	case nhf010Presence:
		person, _ := domain.ParsePersonID("synthetic-person")
		residence, confidence := domain.ResidenceHome, domain.PresenceConfidenceHigh
		if sample == nhf010Unknown {
			residence, confidence = domain.ResidenceUnknown, domain.PresenceConfidenceUnknown
		}
		if sample == nhf010Absent {
			residence = domain.ResidenceAway
		}
		presence, e := domain.NewPresenceValue(domain.PresenceParts{Person: person, Residence: residence, Confidence: confidence, SourceLabel: "Agrégat de présence simulé"})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewPresenceStateValue(presence)
	case nhf010Details:
		person, _ := domain.ParsePersonID("synthetic-person")
		zone, _ := domain.ParsePresenceZoneID("synthetic-zone")
		details, e := domain.NewPresenceDetailsValue(domain.PresenceDetailsParts{Person: person, Zone: &domain.PresenceZone{ID: zone, Label: "Zone privée simulée"}, Coordinates: &domain.Coordinates{}, SourceLabel: "Localisation fictive · 0/0"})
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewPresenceDetailsStateValue(details)
	case nhf010Solar:
		occurrence, e := domain.SolarOccurrenceAt(at.Add(6 * time.Hour))
		if sample == nhf010Absent {
			occurrence, e = domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
		}
		if e != nil {
			return integrations.StateObservation{}, e
		}
		solar, e := domain.NewSolarEventValue(domain.SolarRising, occurrence)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewSolarEventStateValue(solar)
	default:
		return integrations.StateObservation{}, errors.New("unparsed environmental family")
	}
	if err != nil {
		return integrations.StateObservation{}, err
	}
	return integrations.NewStateObservation(d.endpoint, quality, &value, &at, received, nil)
}
