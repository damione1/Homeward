package calendars

import (
	"context"
	"testing"
	"time"
)

func TestNHA225LocalTimeUsesPinnedZoneAndRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	zone, _ := ParseZone("America/Toronto")
	first := nha100Instant(t, "2026-11-01T05:30:00Z")
	second := nha100Instant(t, "2026-11-01T06:30:00Z")
	a, err := zone.LocalTime(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := zone.LocalTime(ctx, second)
	if err != nil || a != b || b.String() != "2026-11-01T01:30:00" {
		t.Fatal("fold projection", err)
	}
	if _, err = (Zone{}).LocalTime(ctx, first); err == nil {
		t.Fatal("invalid zone accepted")
	}
	if _, err = zone.LocalTime(ctx, Instant{}); err == nil {
		t.Fatal("invalid instant accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = zone.LocalTime(cancelled, first); err == nil {
		t.Fatal("cancel ignored")
	}
	east, _ := ParseZone("Pacific/Kiritimati")
	last, err := NewInstant(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = east.LocalTime(ctx, last); err == nil {
		t.Fatal("out of civil range accepted")
	}
}
