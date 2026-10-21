package calendars

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func nha100Date(t *testing.T, text string) CivilDate {
	t.Helper()
	v, e := ParseDate(text)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Local(t *testing.T, text string) LocalDateTime {
	t.Helper()
	v, e := ParseLocalDateTime(text)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Instant(t *testing.T, text string) Instant {
	t.Helper()
	v, e := ParseInstant(text)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Zone(t *testing.T, text string) Zone {
	t.Helper()
	v, e := ParseZone(text)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Day(t *testing.T, from, to string) Span {
	t.Helper()
	v, e := AllDaySpan(nha100Date(t, from), nha100Date(t, to))
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Timed(t *testing.T, anchor, end, zone string) Span {
	t.Helper()
	v, e := TimedSpan(context.Background(), nha100Local(t, anchor), nha100Instant(t, end), nha100Zone(t, zone))
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Point(t *testing.T, kind TemporalKind, text string) Point {
	t.Helper()
	v, e := ParsePoint(kind, text)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func nha100Event(t *testing.T, span Span, raw string, edit func(*EventInput)) Event {
	t.Helper()
	rule := NoRecurrence()
	var err error
	if raw != "" {
		rule, err = ParseRule(raw, span)
		if err != nil {
			t.Fatal(err)
		}
	}
	input := EventInput{Summary: "Synthetic NHA100 event", Span: span, Rule: rule}
	if edit != nil {
		edit(&input)
	}
	event, err := NewEvent(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
func nha100Code(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code() != code {
		t.Fatalf("expected code=%d obtained=%v", code, err)
	}
}

// These identities are only for pure temporal fixtures. DB fixtures below use
// real ConfigureBinding/Resolve identities from the frozen manifest.
func nha100PureKey(t *testing.T) EventKey {
	t.Helper()
	entity, _ := domain.ParseEntityID("nha100-pure-synthetic-only")
	alias, _ := domain.ParseHAAlias("calendar.family")
	uid, _ := ParseUID("synthetic-uid")
	return EventKey{calendar: CalendarID{entity: entity, alias: alias, origin: Local}, uid: uid}
}
func nha100Expand(t *testing.T, event Event, from, to string) []Occurrence {
	t.Helper()
	var window Window
	var err error
	if event.span.kind == AllDay {
		window, err = DateWindow(nha100Date(t, from), nha100Date(t, to))
	} else {
		window, err = TimeWindow(nha100Instant(t, from), nha100Instant(t, to), nha100Zone(t, "UTC"))
	}
	if err != nil {
		t.Fatal(err)
	}
	var result []Occurrence
	err = expandEvent(context.Background(), EventSnapshot{key: nha100PureKey(t), definition: event}, window, newBudget(context.Background()), func(o Occurrence) error {
		if !o.Valid() {
			t.Fatal("invalid occurrence")
		}
		result = append(result, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(result, func(a, b Occurrence) int { return compareOrder(a.order, b.order) })
	return result
}
func nha100ExpectStarts(t *testing.T, items []Occurrence, want ...string) {
	t.Helper()
	got := make([]string, len(items))
	for i, o := range items {
		got[i] = startKey(o.span)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expected=%v obtained=%v", want, got)
	}
	t.Logf("expected=%v obtained=%v", want, got)
}
func TestNHA100NormativeProfile(t *testing.T) {
	for _, f := range []struct {
		name, from, end, rule, to string
		want                      []string
	}{
		{"daily", "2026-01-01", "2026-01-02", "FREQ=DAILY;INTERVAL=2;COUNT=3", "2026-01-10", []string{"2026-01-01", "2026-01-03", "2026-01-05"}},
		{"until-inclusive", "2026-01-01", "2026-01-02", "FREQ=DAILY;INTERVAL=2;UNTIL=20260103", "2026-01-10", []string{"2026-01-01", "2026-01-03"}},
		{"weekly", "2026-01-05", "2026-01-06", "FREQ=WEEKLY;BYDAY=MO,WE;INTERVAL=2;COUNT=4", "2026-02-01", []string{"2026-01-05", "2026-01-07", "2026-01-19", "2026-01-21"}},
		{"monthly-invalid", "2026-01-31", "2026-02-01", "FREQ=MONTHLY;BYMONTHDAY=31;COUNT=4", "2026-11-01", []string{"2026-01-31", "2026-03-31", "2026-05-31", "2026-07-31"}},
		{"RFC-month-set", "2007-01-15", "2007-01-16", "FREQ=MONTHLY;BYMONTHDAY=15,30;COUNT=5", "2007-06-01", []string{"2007-01-15", "2007-01-30", "2007-02-15", "2007-03-15", "2007-03-30"}},
	} {
		t.Run(f.name, func(t *testing.T) {
			e := nha100Event(t, nha100Day(t, f.from, f.end), f.rule, nil)
			nha100ExpectStarts(t, nha100Expand(t, e, f.from, f.to), f.want...)
		})
	}
	leap := nha100Event(t, nha100Day(t, "2096-02-29", "2096-03-01"), "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=3", nil)
	for _, f := range []struct {
		from, to string
		want     []string
	}{{"2100-01-01", "2101-01-01", nil}, {"2104-01-01", "2105-01-01", []string{"2104-02-29"}}, {"2108-01-01", "2109-01-01", []string{"2108-02-29"}}} {
		nha100ExpectStarts(t, nha100Expand(t, leap, f.from, f.to), f.want...)
	}
}
func TestNHA100DSTAndPinnedTimezone(t *testing.T) {
	// An ambient nonexistent ZONEINFO must not affect lookup, including a cache
	// miss; this package uses only its hash-versioned embedded bytes.
	t.Setenv("ZONEINFO", "/nha100/synthetic/does-not-exist")
	hash := sha256.Sum256(timezoneArchive)
	if hex.EncodeToString(hash[:]) != timezoneSHA256 {
		t.Fatal("timezone data changed")
	}
	if _, e := ParseZone("Pacific/Chatham"); e != nil {
		t.Fatal(e)
	}
	for _, f := range []struct {
		name, anchor, end, rule, from, to string
		want                              []string
	}{
		{"generated-gap-count", "2026-03-07T02:30:00", "2026-03-07T08:30:00Z", "FREQ=DAILY;COUNT=3", "2026-03-07T00:00:00Z", "2026-03-12T00:00:00Z", []string{"2026-03-07T07:30:00Z", "2026-03-09T06:30:00Z", "2026-03-10T06:30:00Z"}},
		{"business-until", "2026-03-07T02:30:00", "2026-03-07T08:30:00Z", "FREQ=DAILY;UNTIL=20260309T063000Z", "2026-03-07T00:00:00Z", "2026-03-12T00:00:00Z", []string{"2026-03-07T07:30:00Z", "2026-03-09T06:30:00Z"}},
		{"initial-gap", "2026-03-08T02:30:00", "2026-03-08T08:30:00Z", "FREQ=DAILY;COUNT=2", "2026-03-08T00:00:00Z", "2026-03-11T00:00:00Z", []string{"2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z"}},
		{"first-fold", "2026-10-31T01:30:00", "2026-10-31T06:30:00Z", "FREQ=DAILY;COUNT=3", "2026-10-31T00:00:00Z", "2026-11-04T00:00:00Z", []string{"2026-10-31T05:30:00Z", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}},
	} {
		t.Run(f.name, func(t *testing.T) {
			e := nha100Event(t, nha100Timed(t, f.anchor, f.end, "America/New_York"), f.rule, nil)
			items := nha100Expand(t, e, f.from, f.to)
			nha100ExpectStarts(t, items, f.want...)
			if f.name == "initial-gap" && items[0].id.point.String() != f.anchor {
				t.Fatal("declared anchor lost")
			}
		})
	}
	e := nha100Event(t, nha100Day(t, "2026-03-07", "2026-03-10"), "", nil)
	nha100ExpectStarts(t, nha100Expand(t, e, "2026-03-08", "2026-03-09"), "2026-03-07")
	nha100ExpectStarts(t, nha100Expand(t, e, "2026-03-10", "2026-03-11"))
	// The 24h civil jump can make an initial gap and next generated date share an
	// instant. The recurrence set retains the original identity once.
	samoa := nha100Event(t, nha100Timed(t, "2011-12-30T10:00:00", "2011-12-30T21:00:00Z", "Pacific/Apia"), "FREQ=DAILY;COUNT=2", nil)
	nha100ExpectStarts(t, nha100Expand(t, samoa, "2011-12-30T00:00:00Z", "2012-01-03T00:00:00Z"), "2011-12-30T20:00:00Z", "2011-12-31T20:00:00Z")
	c := &zoneCache{}
	c.once.Do(c.index)
	c.mu.Lock()
	c.used = 4 * 1024 * 1024
	c.mu.Unlock()
	if _, e := c.location("UTC"); e == nil {
		t.Fatal("zone byte ceiling bypassed")
	}
	c = &zoneCache{}
	c.once.Do(c.index)
	c.mu.Lock()
	for i := 0; i < 64; i++ {
		c.locations[string(rune(i+1))] = time.UTC
	}
	c.mu.Unlock()
	if _, e := c.location("UTC"); e == nil {
		t.Fatal("zone name ceiling bypassed")
	}
}
func TestNHA100ExceptionsOverlapAndCopies(t *testing.T) {
	original := nha100Point(t, AllDay, "2026-08-24")
	moved, _ := Move(original, nha100Day(t, "2026-08-27", "2026-08-29"))
	cancel, _ := Cancel(nha100Point(t, AllDay, "2026-08-25"))
	e := nha100Event(t, nha100Day(t, "2026-08-22", "2026-08-23"), "FREQ=DAILY;COUNT=4", func(in *EventInput) {
		in.Additions = []Point{nha100Point(t, AllDay, "2026-08-24"), nha100Point(t, AllDay, "2026-08-26")}
		in.Exclusions = []Point{nha100Point(t, AllDay, "2026-08-23")}
		in.Exceptions = []Exception{moved, cancel}
	})
	items := nha100Expand(t, e, "2026-08-22", "2026-08-30")
	nha100ExpectStarts(t, items, "2026-08-22", "2026-08-26", "2026-08-27")
	if items[2].id.point != original {
		t.Fatal("move changed original identity")
	}
	items = nha100Expand(t, e, "2026-08-28", "2026-08-29")
	nha100ExpectStarts(t, items, "2026-08-27")
	if items[0].id.point != original {
		t.Fatal("outside-window origin lost")
	}
	nha100ExpectStarts(t, nha100Expand(t, e, "2026-08-29", "2026-08-30"))
	added := e.Additions()
	added[0] = Point{}
	exceptions := e.Exceptions()
	exceptions[0] = Exception{}
	if !e.Valid() || !e.additions[0].Valid() || !e.exceptions[0].valid() {
		t.Fatal("getter exposed mutable state")
	}
	ancientMoved, _ := Move(nha100Point(t, AllDay, "1600-01-01"), nha100Day(t, "2026-08-28", "2026-08-29"))
	ancient := nha100Event(t, nha100Day(t, "1600-01-01", "1600-01-02"), "", func(in *EventInput) { in.Exceptions = []Exception{ancientMoved} })
	nha100ExpectStarts(t, nha100Expand(t, ancient, "2026-08-28", "2026-08-29"), "2026-08-28")
	input := EventInput{Summary: "Synthetic", Span: nha100Day(t, "2026-01-01", "2026-01-02"), Rule: NoRecurrence(), Additions: []Point{nha100Point(t, AllDay, "2026-01-02")}}
	copy, e2 := NewEvent(context.Background(), input)
	if e2 != nil {
		t.Fatal(e2)
	}
	input.Additions[0] = Point{}
	if !copy.additions[0].Valid() {
		t.Fatal("constructor retained caller slice")
	}
	invalid := nha100Point(t, AllDay, "2026-02-01")
	_, err := NewEvent(context.Background(), EventInput{Summary: "Synthetic", Span: e.span, Rule: e.rule, Exclusions: []Point{invalid}})
	nha100Code(t, err, InvalidOccurrence)
}
func TestNHA100StrictRejectionsAndBudgets(t *testing.T) {
	span := nha100Day(t, "2026-02-02", "2026-02-03")
	for _, raw := range []string{"FREQ=SECONDLY", "FREQ=DAILY;BYHOUR=3", "FREQ=MONTHLY;BYMONTHDAY=30;BYMONTH=2", "FREQ=MONTHLY;BYMONTHDAY=-1", "FREQ=WEEKLY;BYDAY=2MO", "FREQ=WEEKLY;BYDAY=TU", "FREQ=WEEKLY;BYDAY=MO,MO", "FREQ=WEEKLY;WKST=SU", "FREQ=YEARLY;BYMONTH=3", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;INTERVAL=13", "FREQ=DAILY;COUNT=2;UNTIL=20260204", "FREQ=DAILY;FREQ=YEARLY", "FREQ=DAILY;UNTIL=20260230"} {
		_, err := ParseRule(raw, span)
		nha100Code(t, err, UnsupportedRule)
		var e *Error
		errors.As(err, &e)
		if e.RuleIssue() == RuleIssueNone {
			t.Fatal("imprecise unsupported rule")
		}
	}
	_, err := ParseRule("FREQ=DAILY", nha100Day(t, "0001-01-01", "0001-01-02"))
	// NHA-101 separates a present civil0001 anchor from Go's absent Instant;
	// the recurrence adapter no longer delegates that anchor to a zero-time sentinel.
	if err != nil {
		t.Fatalf("present civil0001 recurrence rejected: %v", err)
	}
	if (Event{}).Valid() || (Rule{}).validFor(span) || (Span{}).Valid() || (CalendarID{}).Valid() || (EventKey{}).Valid() || (Revision{}).Presence() != PresenceInvalid {
		t.Fatal("Go zero admitted")
	}
	_, err = AllDaySpan(nha100Date(t, "2026-01-01"), CivilDate{})
	nha100Code(t, err, MissingEnd)
	for _, text := range []string{"2026-02-29", "2100-02-29", "2026-04-31", "0000-01-01"} {
		_, err := ParseDate(text)
		nha100Code(t, err, InvalidDate)
	}
	type nilContext struct{ context.Context }
	var typedNil *nilContext
	for _, ctx := range []context.Context{nil, typedNil} {
		_, err := NewEvent(ctx, EventInput{})
		nha100Code(t, err, InvalidInput)
	}
	old := nha100Event(t, nha100Day(t, "2006-01-01", "2006-01-02"), "FREQ=DAILY", nil)
	budget := newBudget(context.Background())
	count := 0
	started := time.Now()
	err = walkBase(context.Background(), old, nha100Point(t, AllDay, "2026-01-02"), budget, func(Point, Span) error { count++; return nil })
	if err != nil || count != 7307 || budget.work > 32000000 {
		t.Fatalf("twenty years count=%d work=%d err=%v", count, budget.work, err)
	}
	t.Logf("twenty-year daily accepted calls=%d units=%d elapsed=%s", budget.candidates, budget.work, time.Since(started))
	rare := nha100Event(t, nha100Day(t, "2096-02-29", "2096-03-01"), "FREQ=MONTHLY;INTERVAL=12", nil)
	budget = newBudget(context.Background())
	budget.work = 32000000 - rare.rule.workCharge() + 1
	err = walkBase(context.Background(), rare, nha100Point(t, AllDay, "2101-01-01"), budget, func(Point, Span) error { return nil })
	nha100Code(t, err, BudgetExceeded)
	budget = newBudget(context.Background())
	err = walkBase(context.Background(), old, nha100Point(t, AllDay, "2207-01-01"), budget, func(Point, Span) error { return nil })
	nha100Code(t, err, BudgetExceeded)
	if budget.candidates != 0 {
		t.Fatal("age rejection after iterator work")
	}
	// Terminal hard UTC ceiling also guards weekly daysets entering year10000.
	terminal := nha100Timed(t, "9999-12-31T12:00:00", "9999-12-31T13:00:00Z", "UTC")
	rule, err := ParseRule("FREQ=WEEKLY;BYDAY=FR,SU", terminal)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ruleIterator(rule, time.Date(10000, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := next(); !ok || v.Year() != 9999 {
		t.Fatal("terminal missing")
	}
	if _, ok := next(); ok {
		t.Fatal("year10000 escaped")
	}
	if _, ok := next(); ok {
		t.Fatal("exhaustion revived")
	}
}

func TestNHA100MidnightProjectionAndSkippedCivilDay(t *testing.T) {
	// IANA Cuba2026: spring00:00->01:00 at05Z; autumn01:00->00:00
	// repeats midnight, whose first instant is04Z and second is05Z.
	for _, f := range []struct {
		name, zone, day, end, from, to string
		want                           bool
	}{
		{"midnight-gap-exclusive", "America/Havana", "2026-03-08", "2026-03-09", "2026-03-08T04:30:00Z", "2026-03-08T05:00:00Z", false},
		{"midnight-gap-overlap", "America/Havana", "2026-03-08", "2026-03-09", "2026-03-08T05:00:00Z", "2026-03-08T05:30:00Z", true},
		{"second-midnight-fold", "America/Havana", "2026-11-01", "2026-11-02", "2026-11-01T04:30:00Z", "2026-11-01T05:00:00Z", true},
		{"skipped-Apia-date", "Pacific/Apia", "2011-12-30", "2011-12-31", "2011-12-30T09:00:00Z", "2011-12-30T12:00:00Z", false},
		{"civil-zero-is-present", "UTC", "0001-01-01", "0001-01-02", "0001-01-01T01:00:00Z", "0001-01-01T02:00:00Z", true},
	} {
		t.Run(f.name, func(t *testing.T) {
			window, err := TimeWindow(nha100Instant(t, f.from), nha100Instant(t, f.to), nha100Zone(t, f.zone))
			if err != nil {
				t.Fatal(err)
			}
			if !window.Valid() {
				t.Fatal("valid instant window rejected")
			}
			event := nha100Event(t, nha100Day(t, f.day, f.end), "", nil)
			var count int
			err = expandEvent(context.Background(), EventSnapshot{key: nha100PureKey(t), definition: event}, window, newBudget(context.Background()), func(Occurrence) error { count++; return nil })
			if err != nil || (count == 1) != f.want {
				t.Fatalf("expected overlap=%t obtained count=%d err=%v", f.want, count, err)
			}
		})
	}
	// A skipped civil DATE is still a DATE, with its original exclusive end.
	e := nha100Event(t, nha100Day(t, "2011-12-30", "2011-12-31"), "", nil)
	nha100ExpectStarts(t, nha100Expand(t, e, "2011-12-30", "2011-12-31"), "2011-12-30")
}
