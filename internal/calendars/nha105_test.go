package calendars_test

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
)

func TestNHA105CivilProjectionUsesPinnedZoneAndCalendarDays(t *testing.T) {
	zone, err := calendars.ParseZone("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	if zone.DataID() != calendars.TimezoneDataID {
		t.Fatal("projection did not retain the pinned timezone identity")
	}
	for _, raw := range []string{"2024-11-03T05:30:00Z", "2024-11-03T06:30:00Z"} {
		instant, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatal(err)
		}
		date, err := zone.LocalDate(instant)
		if err != nil || date.String() != "2024-11-03" {
			t.Fatalf("fold projected to %s: %v", date.String(), err)
		}
	}
	date, err := calendars.ParseDate("2024-03-10")
	if err != nil {
		t.Fatal(err)
	}
	next, err := date.AddDays(1)
	if err != nil || next.String() != "2024-03-11" {
		t.Fatalf("civil DST addition: %s, %v", next.String(), err)
	}
	year, month, day, err := next.Components()
	if err != nil || year != 2024 || month != time.March || day != 11 {
		t.Fatalf("scalar components: %d %d %d, %v", year, month, day, err)
	}
	if date.String() != "2024-03-10" {
		t.Fatal("addition mutated the original date")
	}
	leap, err := calendars.ParseDate("2024-02-28")
	if err != nil {
		t.Fatal(err)
	}
	leap, err = leap.AddDays(1)
	if err != nil || leap.String() != "2024-02-29" {
		t.Fatalf("leap addition: %s, %v", leap.String(), err)
	}
}

func TestNHA105CivilProjectionRejectsInvalidAndUnboundedInputs(t *testing.T) {
	if _, _, _, err := (calendars.CivilDate{}).Components(); err == nil {
		t.Fatal("zero civil date admitted")
	}
	date, err := calendars.ParseDate("2024-01-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-367, 367} {
		if _, err := date.AddDays(delta); err == nil {
			t.Fatalf("unbounded delta %d admitted", delta)
		}
	}
	if _, err := (calendars.Zone{}).LocalDate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("zero zone admitted")
	}
	zone, err := calendars.ParseZone("Pacific/Kiritimati")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zone.LocalDate(time.Time{}); err == nil {
		t.Fatal("zero instant admitted")
	}
	if _, err := zone.LocalDate(time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("projection outside civil year range admitted")
	}
}
