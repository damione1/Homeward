package config

import (
	"strings"
	"testing"
)

func TestNHA223DisplayProfileStrictExplicitOperationalZone(t *testing.T) {
	lookup := func(string) (string, bool) { t.Fatal("profile resolved environment"); return "", false }
	for _, zone := range []string{"", "UTC", "Europe/Paris", "America/New_York"} {
		raw := "{}\n"
		if zone != "" {
			raw = "display_profile: {time_zone: " + zone + "}\n"
		}
		cfg, err := ParseOperationalWithEnv([]byte(raw), lookup)
		if err != nil || cfg.DisplayTimeZone() != zone {
			t.Fatalf("zone %q: %q %v", zone, cfg.DisplayTimeZone(), err)
		}
		if zone != "" {
			if _, err := ParseBootstrapLaunchWithEnv([]byte(raw), lookup); err != ErrInvalidYAML {
				t.Fatal("bootstrap admitted presentation authority", err)
			}
		}
	}
	for _, raw := range []string{
		"display_profile: null", "display_profile: {}", "display_profile: []",
		"display_profile: {time_zone: ''}", "display_profile: {time_zone: Local}",
		"display_profile: {time_zone: Invalid/PRIVATE_CANARY}", "display_profile: {time_zone: 1}",
		"display_profile: {time_zone: UTC, extra: PRIVATE_CANARY}",
		"display_profile: {time_zone: UTC, time_zone: Europe/Paris}",
		"display_profile: {timezone: UTC}",
		"display_profile: &p {time_zone: UTC}\ndisplay_profile: *p",
	} {
		if _, err := ParseOperationalWithEnv([]byte(raw), lookup); err == nil || strings.Contains(err.Error(), "PRIVATE_CANARY") {
			t.Fatalf("invalid or disclosed profile: %v", err)
		}
	}
}
