package reactivetemplate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nativeObservationFixture struct {
	alias string
	value domain.StateValue
}

func nativeObservationFixtures(t *testing.T) []nativeObservationFixture {
	t.Helper()
	state := func(value domain.StateValue, err error) domain.StateValue {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	closed := domain.CoverStateClosed
	cover, err := domain.NewCoverValue(&closed, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	weather, err := domain.NewWeatherValue(domain.WeatherParts{Temperature: &domain.Temperature{Value: 21, Unit: domain.TemperatureCelsius}, SourceLabel: "native-weather-canary"})
	if err != nil {
		t.Fatal(err)
	}
	person, err := domain.ParsePersonID("native-fixture-person")
	if err != nil {
		t.Fatal(err)
	}
	presence, err := domain.NewPresenceValue(domain.PresenceParts{Person: person, Residence: domain.ResidenceHome, Confidence: domain.PresenceConfidenceHigh, SourceLabel: "native-presence-canary"})
	if err != nil {
		t.Fatal(err)
	}
	details, err := domain.NewPresenceDetailsValue(domain.PresenceDetailsParts{Person: person, Coordinates: &domain.Coordinates{LatitudeDegrees: 45.51, LongitudeDegrees: -73.56}, SourceLabel: "private-coordinates-canary"})
	if err != nil {
		t.Fatal(err)
	}
	return []nativeObservationFixture{
		{"cover.native_fixture", state(domain.NewCoverStateValue(cover))},
		{"lock.native_fixture", state(domain.NewLockStateValue(domain.LockStateLocked))},
		{"alarm_control_panel.native_fixture", state(domain.NewAlarmStateValue(domain.AlarmStateArmedAway))},
		{"weather.native_fixture", state(domain.NewWeatherStateValue(weather))},
		{"person.native_fixture", state(domain.NewPresenceStateValue(presence))},
		{"sensor.native_fixture_details", state(domain.NewPresenceDetailsStateValue(details))},
	}
}
func TestAdmittedNativeObservationsDoNotBreakGlobalReactiveReadViewOrInventHASemantics(t *testing.T) {
	fixtures := nativeObservationFixtures(t)
	switchValue := domain.NewSwitchValue(true)
	fixtures = append(fixtures, nativeObservationFixture{"switch.supported_fixture", switchValue})
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	snapshot := sequencer.Snapshot{RegistryReady: true, Registry: map[domain.EntityID]integrations.RegistryRecord{}, States: map[domain.EntityID]domain.ObservedState{}}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stamp, _ := domain.NewSourceStamp("native-fixture-session", nil)
	for index, fixture := range fixtures {
		device, _ := domain.ParseDeviceID("native-fixture-device")
		entity, _ := domain.ParseEntityID(fmt.Sprintf("native-fixture-entity-%d", index))
		instance, _ := domain.ParseIntegrationInstanceID("native-fixture")
		key, _ := integrations.NewSourceKey("native-fixture", fmt.Sprintf("source-%d", index))
		alias, _ := domain.ParseHAAlias(fixture.alias)
		record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		report, err := domain.NewStateReport(quality, &fixture.value, &at, at, stamp, uint64(index+1))
		if err != nil {
			t.Fatal(err)
		}
		observed, _, err := domain.ApplyStateReport(entity, nil, report)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Registry[entity] = record
		snapshot.States[entity] = observed
	}
	view, err := ReadViewFromSnapshot(snapshot, nil)
	if err != nil {
		t.Fatal("valid native observations stopped the global reactor", err)
	}
	inputs, err := template.NewInputsFromReadView(view, nil, at, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for index, fixture := range fixtures {
		program, err := template.Compile("{{ states('" + fixture.alias + "') }}")
		if err != nil {
			t.Fatal(err)
		}
		tracked, err := template.EvaluateTracked(context.Background(), program, inputs, template.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		result, ok := tracked.Result()
		if !ok || len(tracked.Reads()) != 1 {
			t.Fatal("registered native identity lost")
		}
		read := tracked.Reads()[0]
		if _, known := read.Entity(); !known {
			t.Fatal("native registry identity dropped")
		}
		if index == len(fixtures)-1 {
			if result.Text() != "on" || !read.Present() {
				t.Fatal("supported HA switch projection regressed")
			}
			continue
		}
		if result.Text() != "unknown" || read.Present() {
			t.Fatalf("native contract %s invented a HA state: %q", fixture.alias, result.Text())
		}
		if _, reported := read.Quality(); reported {
			t.Fatal("unmapped native observation invented a HA quality report")
		}
		attributes, err := template.Compile("{{ state_attr('" + fixture.alias + "','latitude') }}")
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := template.EvaluateTracked(context.Background(), attributes, inputs, template.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		result, ok = outcome.Result()
		kind, kindErr := result.Native().Kind()
		if !ok || kindErr != nil || kind != template.KindNone {
			t.Fatalf("native details leaked an undeclared HA attribute: %q", result.Text())
		}
	}
}
func TestOnlyKnownValidatedNativeContractsCanBypassHAValueFormatting(t *testing.T) {
	for _, fixture := range nativeObservationFixtures(t) {
		if _, _, err := formatCurrent(fixture.value); !errors.Is(err, errNoHAProjection) {
			t.Fatal("native contract did not use explicit unmapped state", fixture.alias, err)
		}
	}
	if _, _, err := formatCurrent(domain.StateValue{}); err == nil || errors.Is(err, errNoHAProjection) {
		t.Fatal("invalid or future state acquired a fail-open projection")
	}
}
