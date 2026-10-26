package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHF010EnvironmentalFixtureKeepsZeroUnknownAbsentAndStaleDistinct(t *testing.T) {
	file := filepath.Join("..", "..", "qualification", "frontend", "environment", "devices.json")
	devices, err := nhf010ReadDevices(file)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 1, 6, 30, 0, 123, time.UTC)
	for _, device := range devices {
		if err := catalog.AdmitReadOnlyDriverCapability(device.capability()); err != nil {
			t.Fatal("fixture capability", device.kind, err)
		}
		for _, sample := range []nhf010Sample{nhf010Nominal, nhf010Stale, nhf010Unknown, nhf010Absent, nhf010Missing} {
			observation, err := device.observation(sample, at)
			if err != nil {
				t.Fatal(device.kind, sample, err)
			}
			endpoint, _, value, observed, received, _, _, err := observation.Components()
			if err != nil || endpoint != device.endpoint || !received.Equal(at) {
				t.Fatal("fixture observation envelope", err)
			}
			if sample == nhf010Missing {
				if value != nil || observed != nil {
					t.Fatal("missing fixture invented an observation")
				}
				continue
			}
			if value == nil || observed == nil {
				t.Fatal("present fixture lost value/date")
			}
			if sample == nhf010Stale && !observed.Equal(at.Add(-2*time.Hour)) {
				t.Fatal("stale fixture lost age")
			}
			switch device.kind {
			case nhf010Weather:
				weather, ok := value.Weather()
				if !ok {
					t.Fatal("weather family")
				}
				parts, err := weather.Components()
				if err != nil {
					t.Fatal(err)
				}
				if parts.Temperature == nil || parts.Temperature.Value != 0 || parts.HumidityPercent == nil || *parts.HumidityPercent != 0 {
					t.Fatal("zero weather lost")
				}
				if sample == nhf010Stale && !parts.Forecast.ValidUntil.Before(received) {
					t.Fatal("stale forecast not expired")
				}
				if sample == nhf010Unknown && (parts.Condition == nil || *parts.Condition != domain.WeatherUnknown) {
					t.Fatal("unknown weather lost")
				}
			case nhf010Presence:
				presence, ok := value.Presence()
				if !ok {
					t.Fatal("presence family")
				}
				parts, err := presence.Components()
				if err != nil {
					t.Fatal(err)
				}
				if sample == nhf010Unknown && parts.Residence != domain.ResidenceUnknown {
					t.Fatal("unknown presence inferred away")
				}
				if sample == nhf010Absent && parts.Residence != domain.ResidenceAway {
					t.Fatal("away presence lost")
				}
			case nhf010Details:
				details, ok := value.PresenceDetails()
				if !ok {
					t.Fatal("details family")
				}
				parts, err := details.Components()
				if err != nil || parts.Coordinates == nil || parts.Coordinates.LatitudeDegrees != 0 || parts.Coordinates.LongitudeDegrees != 0 {
					t.Fatal("explicit synthetic 0/0 lost", err)
				}
			case nhf010Solar:
				solar, ok := value.SolarEvent()
				if !ok {
					t.Fatal("solar event family")
				}
				_, occurrence, err := solar.Components()
				if err != nil {
					t.Fatal(err)
				}
				_, _, kind, err := occurrence.Components()
				if err != nil {
					t.Fatal(err)
				}
				if sample == nhf010Absent && kind != domain.SolarOccurrenceAbsent {
					t.Fatal("absent solar event became a timestamp")
				}
			default:
				t.Fatal("unparsed device family")
			}
		}
	}
}

func TestNHF010EnvironmentalFixtureRejectsUnknownProvidersAndClosedFamilies(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "qualification", "frontend", "environment", "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]byte{
		[]byte("[]"), append(append([]byte{}, raw...), []byte(" {}")...),
		bytes.Replace(raw, []byte(`"weather"`), []byte(`"future"`), 1),
		bytes.Replace(raw, []byte(`"kind":"weather"`), []byte(`"provider":"https://forbidden.invalid","kind":"weather"`), 1),
		bytes.Replace(raw, []byte(`"kind":"solar"`), []byte(`"kind":"weather"`), 1),
	} {
		path := filepath.Join(t.TempDir(), "devices.json")
		if err := os.WriteFile(path, input, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := nhf010ReadDevices(path); err == nil {
			t.Fatal("unbounded or unsupported environment source accepted")
		}
	}
}

func TestNHF010FixtureControlsRejectUnknownActionsProvidersAndSequenceGaps(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "command.json")
	for _, input := range []string{
		`{"sequence":1,"action":"unlock"}`,
		`{"sequence":1,"action":"stale","provider":"https://forbidden.invalid"}`,
		`{"sequence":1,"action":"revoke-details","account_id":"foreign"}`,
		`{"sequence":0,"action":"nominal"}`,
		`{"sequence":2,"action":"nominal"}`,
		`{"sequence":1,"action":"nominal"} {}`,
	} {
		if err := os.WriteFile(file, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := nhf010ReadControl(directory, 0); err == nil {
			t.Fatal("unsupported or unbound control accepted")
		}
	}
	if err := os.WriteFile(file, []byte(`{"sequence":1,"action":"revoke-details"}`), 0600); err != nil {
		t.Fatal(err)
	}
	command, err := nhf010ReadControl(directory, 0)
	if err != nil || command == nil || command.action != nhf010ControlRevoke {
		t.Fatal("typed control refused", err)
	}
	if replay, err := nhf010ReadControl(directory, 1); err != nil || replay != nil {
		t.Fatal("replayed filesystem control", err)
	}
}
