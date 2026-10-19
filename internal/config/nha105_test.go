package config_test

import (
	"fmt"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/solar"
	"strings"
	"testing"
)

func TestNHA105PrivateConfigAdmission(t *testing.T) {
	base := "listen: '127.0.0.1:0'\n" + testPostgres(t)
	for _, section := range []string{"", "solar: {latitude: 0, longitude: 0, timezone: UTC}\n", "solar: {latitude: 12.3456789012345, longitude: -76.5432109876543, elevation_meters: 1000, timezone: America/New_York}\n"} {
		parsed, err := config.ParseWithEnv([]byte(base+section), testLookup)
		if err != nil {
			t.Fatal("valid synthetic configuration refused")
		}
		settings, enabled := parsed.SolarSettings()
		if enabled != (section != "") || enabled && !settings.Valid() {
			t.Fatal("solar presence was lost")
		}
	}
	for _, section := range []string{"solar: null", "solar: []", "solar: {}", "solar: {latitude: 0, timezone: UTC}", "solar: {latitude: 0, longitude: 0}", "solar: {latitude: .nan, longitude: 0, timezone: UTC}", "solar: {latitude: 0, longitude: .inf, timezone: UTC}", "solar: {latitude: 91, longitude: 0, timezone: UTC}", "solar: {latitude: 0, longitude: 0, timezone: Local}", "solar: {latitude: 0, longitude: 0, timezone: UTC, unknown: 1}", "solar: {latitude: 0, latitude: 1, longitude: 0, timezone: UTC}", "solar: {latitude: '0', longitude: 0, timezone: UTC}"} {
		if _, err := config.ParseWithEnv([]byte(base+section+"\n"), testLookup); err == nil {
			t.Fatal("invalid solar configuration admitted")
		}
	}
}

func TestNHA105NoPrivateObserverEscapes(t *testing.T) {
	const latitude = "12.3456789012345"
	const longitude = "-76.5432109876543"
	parsed, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+testPostgres(t)+"solar: {latitude: "+latitude+", longitude: "+longitude+", timezone: UTC}\n"), testLookup)
	if err != nil {
		t.Fatal("synthetic configuration refused")
	}
	settings, _ := parsed.SolarSettings()
	source, err := solar.NewTriggerSource(settings)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := solar.NewPilotFactory(settings, automationtriggers.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory()
	// fmt skips Format on unexported containing fields. The private pointer
	// slot must keep this fallback from traversing the observer's numbers.
	for _, value := range []any{parsed, &parsed, settings, source, pilot, struct{ Public config.Config }{parsed}, struct{ private config.Config }{parsed}, []config.Config{parsed}, struct{ private integrations.Pilot }{pilot}, struct{ private automationtriggers.SunSource }{source}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			rendered := fmt.Sprintf(format, value)
			if strings.Contains(rendered, latitude) || strings.Contains(rendered, longitude) {
				t.Fatal("private observer escaped formatting")
			}
		}
	}
}
