package integrations

import (
	"crypto/sha256"
	"testing"
	"time"
)

func TestNHA224ApplicationImportSettingsIsExplicitBoundedAndSourceBound(t *testing.T) {
	key, _ := NewSourceKey("synthetic", "calendar")
	resource, _ := PublicConditionalResource("https://calendar.example.invalid/events.ics")
	settings, err := NewConditionalSettings(key, resource, NoConditionalAuth(), PublicConditionalDestination(), sha256.Sum256([]byte("synthetic-parser")))
	if err != nil {
		t.Fatal(err)
	}
	first, err := settings.WithApplicationImport(15*time.Minute, 30*time.Minute)
	if err != nil || first.profile == settings.profile || settings.retention.mode != conditionalHTTPRetention || first.retention.mode != conditionalApplicationImport {
		t.Fatal("implicit import or unchanged source identity", err)
	}
	second, err := settings.WithApplicationImport(20*time.Minute, 30*time.Minute)
	if err != nil || first.profile == second.profile {
		t.Fatal("poll change lost source fingerprint", err)
	}
	third, err := settings.WithApplicationImport(15*time.Minute, time.Hour)
	if err != nil || first.profile == third.profile {
		t.Fatal("freshness change lost source fingerprint", err)
	}
	for _, values := range [][2]time.Duration{{0, time.Hour}, {time.Minute, 0}, {time.Hour, time.Minute}, {time.Hour, 25 * time.Hour}, {time.Second, time.Hour}, {time.Minute + 1, time.Hour}} {
		if _, err := settings.WithApplicationImport(values[0], values[1]); err == nil {
			t.Fatal("invalid import interval", values)
		}
	}
	if _, err := first.WithApplicationImport(time.Hour, time.Hour); err == nil {
		t.Fatal("redecorated policy")
	}
	if _, err := (ConditionalSettings{}).WithApplicationImport(time.Hour, time.Hour); err == nil {
		t.Fatal("zero settings decorated")
	}
}
