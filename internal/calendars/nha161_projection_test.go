package calendars

import (
	"context"
	"testing"
	"time"
)

func TestNHA161AllDayProjectionKeepsDSTCivilDates(t *testing.T) {
	zone, err := ParseZone("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		from, to string
		hours    time.Duration
	}{{"2026-03-08", "2026-03-09", 23}, {"2026-11-01", "2026-11-02", 25}} {
		from, _ := ParseDate(test.from)
		to, _ := ParseDate(test.to)
		span, _ := AllDaySpan(from, to)
		start, end, name, allDay, err := ProjectSpan(context.Background(), span, zone)
		if err != nil || !allDay || name != "America/Toronto" || end.Sub(start) != test.hours*time.Hour {
			t.Fatalf("projection %s: %v %v %v", test.from, start, end, err)
		}
		a, b, _ := span.Dates()
		if a != from || b != to {
			t.Fatal("civil dates rewritten")
		}
	}
}
