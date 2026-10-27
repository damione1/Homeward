package calendars

import (
	"context"
	"testing"
)

func TestNHF015ExplicitEditingResolutionKeepsPinnedLegacyProfile(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		local, zone string
		instants    []string
		offsets     []int
	}{
		{"2026-11-01T01:30:00", "America/Toronto", []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"}, []int{-14400, -18000}},
		{"2026-03-08T02:30:00", "America/Toronto", nil, nil},
		{"2026-11-02T09:00:00", "America/Toronto", []string{"2026-11-02T14:00:00Z"}, []int{-18000}},
		{"2011-12-30T12:00:00", "Pacific/Apia", nil, nil},
	}
	for _, test := range tests {
		t.Run(test.local+test.zone, func(t *testing.T) {
			candidates, err := ResolveLocalTime(ctx, nha100Local(t, test.local), nha100Zone(t, test.zone))
			if err != nil || len(candidates) != len(test.instants) {
				t.Fatalf("candidates=%v error=%v", candidates, err)
			}
			for i, c := range candidates {
				if c.Instant().String() != test.instants[i] || c.OffsetSeconds() != test.offsets[i] {
					t.Fatalf("wrong candidate %v", c)
				}
			}
		})
	}
	zone := nha100Zone(t, "America/Toronto")
	legacy, err := TimedSpan(ctx, nha100Local(t, "2026-03-08T02:30:00"), nha100Instant(t, "2026-03-08T08:30:00Z"), zone)
	if err != nil || legacy.start.String() != "2026-03-08T07:30:00Z" {
		t.Fatalf("legacy gap profile changed: %v %v", legacy, err)
	}
	_, err = SelectedTimedSpan(ctx, nha100Local(t, "2026-03-08T02:30:00"), legacy.start, legacy.end, zone)
	if err == nil {
		t.Fatal("interactive gap was silently normalized")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = ResolveLocalTime(canceled, nha100Local(t, "2026-11-01T01:30:00"), zone); err == nil {
		t.Fatal("canceled resolution admitted")
	}
}
func TestNHF015SelectedFoldRoundTripsWithoutChangingLegacyRows(t *testing.T) {
	ctx := context.Background()
	zone := nha100Zone(t, "America/Toronto")
	anchor := nha100Local(t, "2026-11-01T01:30:00")
	end := nha100Instant(t, "2026-11-01T08:30:00Z")
	for _, at := range []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"} {
		t.Run(at, func(t *testing.T) {
			selected, err := SelectedTimedSpan(ctx, anchor, nha100Instant(t, at), end, zone)
			if err != nil {
				t.Fatal(err)
			}
			event, err := NewEvent(ctx, EventInput{Summary: "Explicit fold", Span: selected, Rule: NoRecurrence()})
			if err != nil || !event.Valid() {
				t.Fatalf("selected event invalid: %v", err)
			}
			encoded := encodeSpan(selected)
			if encoded.data.String != selectedTimezoneDataID || len(encoded.data.String) > 128 {
				t.Fatal("explicit codec not tagged within SQL bounds")
			}
			decoded, err := decodeSpan(ctx, encoded)
			if err != nil || decoded != selected {
				t.Fatalf("round trip changed selection: %v", err)
			}
			occurrence, present, err := occurrenceSpan(ctx, decoded, decoded.point(), true, newBudget(ctx))
			if err != nil || !present || occurrence.start != selected.start {
				t.Fatal("query changed selected fold")
			}
			if at == "2026-11-01T06:30:00Z" {
				encoded.data.String = TimezoneDataID
				if _, err = decodeSpan(ctx, encoded); err == nil {
					t.Fatal("untagged second fold admitted as legacy data")
				}
			}
		})
	}
	legacy, err := TimedSpan(ctx, anchor, end, zone)
	if err != nil {
		t.Fatal(err)
	}
	encoded := encodeSpan(legacy)
	if encoded.data.String != TimezoneDataID {
		t.Fatal("legacy encoding changed")
	}
	decoded, err := decodeSpan(ctx, encoded)
	if err != nil || decoded != legacy {
		t.Fatal("legacy round trip changed")
	}
	if _, err = SelectedTimedSpan(ctx, anchor, nha100Instant(t, "2026-11-01T07:30:00Z"), end, zone); err == nil {
		t.Fatal("foreign wall projection admitted")
	}
	if _, err = SelectedTimedSpan(ctx, anchor, legacy.start, legacy.start, zone); err == nil {
		t.Fatal("zero-duration editing admitted")
	}
}

func TestNHF015AgendaWindowIncludesTimedAndSkippedCivilDates(t *testing.T) {
	ctx := context.Background()
	from, to := nha100Date(t, "2026-11-01"), nha100Date(t, "2026-11-02")
	zone := nha100Zone(t, "America/Toronto")
	window, err := AgendaWindow(ctx, from, to, zone)
	if err != nil {
		t.Fatal(err)
	}
	if window.to.value.Sub(window.from.value).Hours() != 25 {
		t.Fatal("fold day was not 25h")
	}
	timed, err := SelectedTimedSpan(ctx, nha100Local(t, "2026-11-01T01:30:00"), nha100Instant(t, "2026-11-01T06:30:00Z"), nha100Instant(t, "2026-11-01T07:30:00Z"), zone)
	if err != nil {
		t.Fatal(err)
	}
	if selected, err := window.ContainsSpan(ctx, timed); err != nil || !selected {
		t.Fatal("mixed agenda omitted timed event")
	}
	day := nha100Day(t, "2026-11-01", "2026-11-02")
	if selected, err := window.ContainsSpan(ctx, day); err != nil || !selected {
		t.Fatal("mixed agenda omitted all-day event")
	}
	legacy, _ := DateWindow(from, to)
	if selected, _ := legacy.ContainsSpan(ctx, timed); selected {
		t.Fatal("legacy date window changed")
	}
	if window.identity() == legacy.identity() {
		t.Fatal("different window modes share a cursor identity")
	}
	skipped, err := AgendaWindow(ctx, nha100Date(t, "2011-12-30"), nha100Date(t, "2011-12-31"), nha100Zone(t, "Pacific/Apia"))
	if err != nil {
		t.Fatal(err)
	}
	civil := nha100Day(t, "2011-12-30", "2011-12-31")
	if selected, err := skipped.ContainsSpan(ctx, civil); err != nil || !selected {
		t.Fatal("skipped civil date lost its all-day event")
	}
	if selected, err := skipped.ContainsSpan(ctx, timed); err != nil || selected {
		t.Fatal("skipped date selected an unrelated timed event")
	}
}

func TestNHF015SkippedDayHasNoTimedOverlapEvenAcrossItsBoundary(t *testing.T) {
	ctx := context.Background()
	zone := nha100Zone(t, "Pacific/Apia")
	window, err := AgendaWindow(ctx, nha100Date(t, "2011-12-30"), nha100Date(t, "2011-12-31"), zone)
	if err != nil {
		t.Fatal(err)
	}
	span, err := SelectedTimedSpan(ctx, nha100Local(t, "2011-12-29T23:30:00"), nha100Instant(t, "2011-12-30T09:30:00Z"), nha100Instant(t, "2011-12-30T10:30:00Z"), zone)
	if err != nil {
		t.Fatal(err)
	}
	if selected, err := window.ContainsSpan(ctx, span); err != nil || selected {
		t.Fatal("zero elapsed window selected a timed interval")
	}
}
