package config

import (
	"go.yaml.in/yaml/v3"
	"strings"
	"testing"
)

func TestNHA101RemoteCalendarStrictBoundaryAndRedaction(t *testing.T) {
	valid := "label: Famille\nurl: https://example.com/calendar.ics\nview_timezone: America/Toronto\n"
	var node yaml.Node
	if yaml.Unmarshal([]byte(valid), &node) != nil {
		t.Fatal("fixture")
	}
	settings, err := parseRemoteCalendarInput(*node.Content[0])
	if err != nil || settings == nil || !settings.Valid() {
		t.Fatal("valid explicit remote settings refused", err)
	}
	for _, raw := range []string{valid + "label: duplicate\n", valid + "unknown: true\n", valid + "secret_url: token-url\n", strings.ReplaceAll(valid, "America/Toronto", "Missing/Zone"), valid + "auth:\n  bearer_secret: b\n  basic_username_secret: u\n  basic_password_secret: p\n", valid + "network:\n  origin: http://127.0.0.1:8080\n  prefixes: [0.0.0.0/0]\n"} {
		if yaml.Unmarshal([]byte(raw), &node) != nil {
			continue
		}
		if _, err := parseRemoteCalendarInput(*node.Content[0]); err == nil {
			t.Fatal("invalid or ambiguous settings admitted")
		}
	}
	raw := "remote_calendar:\n  label: Famille\n  url: https://example.com/calendar.ics\n  view_timezone: America/Toronto\n"
	c, err := ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if settings, enabled := c.RemoteCalendarSettings(); !enabled || !settings.Valid() {
		t.Fatal("operational source lost remote calendar")
	}
	if _, err := ParseBootstrapLaunchWithEnv([]byte(raw), func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("launch sidecar acquired operational calendar")
	}
}
