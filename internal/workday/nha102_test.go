package workday

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
)

func nha102Settings(t *testing.T, zone string) Settings {
	t.Helper()
	settings, err := ParseSettings(Input{Timezone: zone})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}
func nha102Date(t *testing.T, text string) calendars.CivilDate {
	t.Helper()
	date, err := calendars.ParseDate(text)
	if err != nil {
		t.Fatal(err)
	}
	return date
}

func TestNHA102RetainedProfileChristmasIsWorkday(t *testing.T) {
	settings := nha102Settings(t, "America/Toronto")
	for _, test := range []struct {
		date string
		want bool
	}{
		{"2026-12-21", true}, {"2026-12-22", true}, {"2026-12-23", true},
		{"2026-12-24", true}, {"2026-12-25", true},
		{"2026-12-26", false}, {"2026-12-27", false},
	} {
		got, err := settings.IsWorkday(nha102Date(t, test.date))
		if err != nil || got != test.want {
			t.Fatalf("%s: got %t, want %t, err %v", test.date, got, test.want, err)
		}
	}
	if _, err := (Settings{}).IsWorkday(nha102Date(t, "2026-12-25")); err == nil {
		t.Fatal("zero profile admitted")
	}
	if _, err := settings.IsWorkday(calendars.CivilDate{}); err == nil {
		t.Fatal("zero date admitted")
	}
	for _, zone := range []string{"", "Local", "Unknown/Zone"} {
		if _, err := ParseSettings(Input{Timezone: zone}); err == nil {
			t.Fatalf("unknown zone %q admitted", zone)
		}
	}
	forged := settings
	forged.profile = profileKind(255)
	if forged.Valid() {
		t.Fatal("unknown profile variant admitted")
	}
	var nilClock *nha102Clock
	if _, err := NewPilotFactory(settings, nilClock); err == nil {
		t.Fatal("typed nil clock admitted")
	}
}

func TestNHA102PinnedZoneFindsDSTLocalMidnights(t *testing.T) {
	zone, _ := nha102Settings(t, "America/Toronto").Zone()
	for _, test := range []struct{ now, boundary string }{
		{"2026-03-08T04:59:00Z", "2026-03-08T05:00:00Z"},
		{"2026-03-08T05:01:00Z", "2026-03-09T04:00:00Z"},
		{"2026-11-01T03:59:00Z", "2026-11-01T04:00:00Z"},
		{"2026-11-01T04:01:00Z", "2026-11-02T05:00:00Z"},
	} {
		now, err := time.Parse(time.RFC3339, test.now)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse(time.RFC3339, test.boundary)
		if err != nil {
			t.Fatal(err)
		}
		date, err := zone.LocalDate(now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := nextBoundary(zone, now, date)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%s: boundary %s, want %s, err %v", test.now, got, want, err)
		}
	}
}

func TestNHA102ReaderRejectsZeroAndTypedNil(t *testing.T) {
	settings := nha102Settings(t, "UTC")
	if _, err := NewCalendarReader(calendars.CalendarID{}, settings); err == nil {
		t.Fatal("zero identity admitted")
	}
	var reader *calendarReader
	if err := ValidateReader(reader); err == nil {
		t.Fatal("typed nil reader admitted")
	}
	if _, err := reader.Dates(context.Background(), nha102Date(t, "2026-12-21"), nha102Date(t, "2026-12-22")); err == nil {
		t.Fatal("nil receiver read admitted")
	}
	var nilContext *nha102Context
	if err := validContext(nilContext); err == nil {
		t.Fatal("typed nil context admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation cause lost")
	}
}

type nha102Context struct{ context.Context }
