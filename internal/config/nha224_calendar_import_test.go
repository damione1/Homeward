package config

import (
	"testing"
)

func TestNHA224ExplicitCalendarImportConfiguration(t *testing.T) {
	base := "remote_calendar:\n  label: Synthetic\n  url: https://calendar.example.invalid/events.ics\n  view_timezone: UTC\n"
	parse := func(raw string) (Config, error) {
		return ParseOperationalWithEnv([]byte(base+raw), func(string) (string, bool) { t.Fatal("resolved secret"); return "", false })
	}
	for _, value := range []string{"", "  import: {poll_seconds: 900, freshness_seconds: 1800}\n", "  import: {poll_seconds: 86400, freshness_seconds: 86400}\n"} {
		if _, err := parse(value); err != nil {
			t.Fatal("valid explicit import", err)
		}
	}
	for _, value := range []string{"null", "{}", "{poll_seconds: 900}", "{poll_seconds: 1, freshness_seconds: 1800}", "{poll_seconds: 900, freshness_seconds: 600}", "{poll_seconds: 900, freshness_seconds: 86401}", "{poll_seconds: 900, freshness_seconds: 1800, ignored: true}", "{poll_seconds: '900', freshness_seconds: 1800}"} {
		if _, err := parse("  import: " + value + "\n"); err == nil {
			t.Fatal("invalid import admitted", value)
		}
	}
}
