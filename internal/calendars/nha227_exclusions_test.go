package calendars

import (
	"context"
	"os"
	"testing"
)

func TestNHA227ExclusionsSubtractWithoutMembershipAndSurviveExtension(t *testing.T) {
	span := nha100Day(t, "2026-11-02", "2026-11-03")
	rule, err := ParseRule("FREQ=DAILY;COUNT=3", span)
	if err != nil {
		t.Fatal(err)
	}
	before := nha100Point(t, AllDay, "2026-11-01")
	within := nha100Point(t, AllDay, "2026-11-03")
	after := nha100Point(t, AllDay, "2026-11-05")
	added := nha100Point(t, AllDay, "2026-11-06")
	input := EventInput{Summary: "Synthetic subtraction", Span: span, Rule: rule, Additions: []Point{added}, Exclusions: []Point{before, within, after, added, within}}
	event, err := NewEvent(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(event.Exclusions()) != 4 {
		t.Fatal("exclusion dropped or duplicate retained")
	}
	nha100ExpectStarts(t, nha100Expand(t, event, "2026-11-01", "2026-11-08"), "2026-11-02", "2026-11-04")
	input.Rule, err = ParseRule("FREQ=DAILY;COUNT=5", span)
	if err != nil {
		t.Fatal(err)
	}
	extended, err := NewEvent(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(extended.Exclusions()) != 4 {
		t.Fatal("extension lost exclusions")
	}
	nha100ExpectStarts(t, nha100Expand(t, extended, "2026-11-01", "2026-11-08"), "2026-11-02", "2026-11-04")
	input.Exclusions = []Point{nha100Point(t, Timed, "2026-11-03T12:00:00")}
	_, err = NewEvent(context.Background(), input)
	nha100Code(t, err, InvalidOccurrence)
	input.Exclusions = nil
	detached, _ := Cancel(nha100Point(t, AllDay, "2026-11-20"))
	input.Exceptions = []Exception{detached}
	_, err = NewEvent(context.Background(), input)
	nha100Code(t, err, InvalidOccurrence)
}

func TestNHA227ImportedExclusionsSurviveCodecWithoutInventedOccurrences(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	raw, err := os.ReadFile("testdata/nha227/exclusions.ics")
	if err != nil {
		t.Fatal(err)
	}
	image, err := admitRemoteICS(context.Background(), id, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(image.events) != 1 || len(image.events[0].definition.Exclusions()) != 4 {
		image.close()
		t.Fatal("import lost exact exclusion set")
	}
	payload, err := encodeRemoteImage(image.ctx, image)
	image.close()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRemoteImage(context.Background(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.close()
	if len(decoded.events[0].definition.Exclusions()) != 4 {
		t.Fatal("codec lost exclusions")
	}
	values := queryTime(t, decoded, "2026")
	if len(values) != 2 {
		t.Fatal("exclusions were ignored or became occurrences", len(values))
	}
	requireTimes(t, values[0], "2026-11-02T12:00:00Z", "2026-11-02T13:00:00Z")
	requireTimes(t, values[1], "2026-11-16T12:00:00Z", "2026-11-16T13:00:00Z")
}
