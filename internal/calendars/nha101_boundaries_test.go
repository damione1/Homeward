package calendars

import (
	"context"
	rrule "github.com/teambition/rrule-go"
	"testing"
	"time"
)

func nha101Wall(t *testing.T, s string) time.Time { t.Helper(); return nha100Local(t, s).wallUTC() }
func TestNHA101DomainBridgeCivilZeroAndCeilings(t *testing.T) {
	civilRule, err := ParseRule("FREQ=DAILY", nha100Day(t, "0001-01-01", "0001-01-02"))
	if err != nil || !civilRule.Present() {
		t.Fatal("admitted civil0001 was confused with Go zero", err)
	}
	ctx := context.Background()
	zero := nha101Wall(t, "0001-01-01T00:00:00")
	if !zero.IsZero() {
		t.Fatal("not the intended Go sentinel")
	}
	if _, e := NewInstant(zero); e == nil {
		t.Fatal("Instant Go zero contract silently widened")
	}
	if (LocalDateTime{}).Valid() || (Event{}).Valid() || (Span{}).Valid() {
		t.Fatal("domain zero admitted")
	}
	cases := []struct {
		name            string
		anchor, ceiling time.Time
		count           int
		expected        []time.Time
	}{
		{"civil-zero-count", zero, nha101Wall(t, "0002-01-01T00:00:00"), 1, []time.Time{zero}},
		{"zero-until-inclusive", zero, zero, 0, []time.Time{zero}},
		{"before-anchor", nha101Wall(t, "0001-01-02T00:00:00"), zero, 0, nil},
		{"9999", nha101Wall(t, "9999-01-01T00:00:00"), nha101Wall(t, "9999-12-31T23:59:59"), 0, []time.Time{nha101Wall(t, "9999-01-01T00:00:00")}},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			iterator, e := newPinnedNext(ctx, rrule.ROption{Freq: rrule.YEARLY, Dtstart: entry.anchor, Interval: 1, Count: entry.count}, entry.ceiling)
			if e != nil {
				t.Fatal(e)
			}
			for _, expected := range entry.expected {
				got, present, e := iterator.take(ctx)
				if e != nil || !present || !got.Equal(expected) || got.Nanosecond() != 0 {
					t.Fatalf("got=%v present=%t error=%v expected=%v", got, present, e, expected)
				}
			}
			if _, present, e := iterator.take(ctx); e != nil || present {
				t.Fatalf("not terminated: present=%t error=%v", present, e)
			}
		})
	}
	// Explicit distant ceiling, not the library's implicit~292year horizon.
	iterator, e := newPinnedNext(ctx, rrule.ROption{Freq: rrule.YEARLY, Dtstart: nha101Wall(t, "1970-03-01T02:00:00"), Interval: 1}, nha101Wall(t, "2300-03-01T02:00:00"))
	if e != nil {
		t.Fatal(e)
	}
	for year := 1970; year <= 2300; year++ {
		got, present, e := iterator.take(ctx)
		if e != nil || !present || got.Year() != year {
			t.Fatalf("lost year%d got%v present%t error%v", year, got, present, e)
		}
	}
	if _, present, e := iterator.take(ctx); e != nil || present {
		t.Fatal("explicit Until did not terminate")
	}
}

func TestNHA101DomainEmptyPredicateBeforeProducer(t *testing.T) {
	owner, e := beginAdmission()
	if e != nil {
		t.Fatal(e)
	}
	defer owner.close()
	ctx := context.WithValue(context.Background(), admissionKey{}, owner)
	anchor := nha100Local(t, "2026-02-28T02:00:00")
	_, e = parseObservanceRule("FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30", anchor)
	nha100Code(t, e, InconsistentObservance)
	if owner.observation.RuleConstructors != 0 || owner.observation.NextCalls != 0 {
		t.Fatal("empty predicate reached producer")
	}
	_, e = AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/Empty", Observances: []EmbeddedObservanceInput{{Anchor: anchor, Rule: "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30"}}})
	nha100Code(t, e, InconsistentObservance)
	if owner.observation.RuleConstructors != 0 || owner.observation.NextCalls != 0 {
		t.Fatal("zone empty predicate reached producer")
	}
}

func TestNHA101DomainObservanceDeduplication(t *testing.T) {
	ctx := context.Background()
	anchor := nha100Local(t, "2026-01-01T02:00:00")
	one := EmbeddedObservanceInput{Anchor: anchor, Rule: "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;COUNT=2"}
	z, e := AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/Duplicate", Observances: []EmbeddedObservanceInput{one, one}})
	if e != nil {
		t.Fatal(e)
	}
	if len(z.embedded.observances) != 1 {
		t.Fatal("identical producers not deduplicated")
	}
	// Different COUNT must remain a different producer; its later onset must
	// survive even though the two first onsets coincide.
	short := one
	short.Rule = "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;COUNT=1"
	z, e = AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/Counts", Observances: []EmbeddedObservanceInput{short, one}})
	if e != nil {
		t.Fatal(e)
	}
	if len(z.embedded.observances) != 2 {
		t.Fatal("different termination deduplicated")
	}
	owned, release, e := admissionScope(ctx)
	if e != nil {
		t.Fatal(e)
	}
	e = z.embedded.ensure(owned, 2027)
	release()
	if e != nil {
		t.Fatal(e)
	}
	if len(z.embedded.transitions) != 2 {
		t.Fatal("later distinct-rule onset lost")
	}
	altered := one
	altered.OffsetTo = 3600
	_, e = AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/Offsets", Observances: []EmbeddedObservanceInput{one, altered}})
	nha100Code(t, e, InvalidZone)
}

func TestNHA101DomainObservanceUntilOffsetAndDistinctRDates(t *testing.T) {
	ctx := context.Background()
	anchor := nha100Local(t, "2026-01-01T02:00:00")
	input := EmbeddedZoneInput{Name: "Candidate/Until", Observances: []EmbeddedObservanceInput{{Anchor: anchor, OffsetFrom: 20700, OffsetTo: 20700, Rule: "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;UNTIL=20261231T201500Z"}}}
	zone, e := AdmitEmbeddedZone(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	owned, release, e := admissionScope(ctx)
	if e != nil {
		t.Fatal(e)
	}
	e = zone.embedded.ensure(owned, 2027)
	release()
	if e != nil {
		t.Fatal(e)
	}
	if len(zone.embedded.transitions) != 2 || zone.embedded.transitions[1].at.Format(time.RFC3339) != "2026-12-31T20:15:00Z" {
		t.Fatal("wire UNTIL offset/inclusivity lost")
	}
	a := EmbeddedObservanceInput{Anchor: anchor, RDates: []LocalDateTime{nha100Local(t, "2027-01-01T02:00:00")}}
	b := EmbeddedObservanceInput{Anchor: anchor, RDates: []LocalDateTime{nha100Local(t, "2028-01-01T02:00:00")}}
	zone, e = AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/RDates", Observances: []EmbeddedObservanceInput{a, b}})
	if e != nil {
		t.Fatal(e)
	}
	if len(zone.embedded.observances) != 2 {
		t.Fatal("different RDATE sets deduplicated")
	}
	owned, release, e = admissionScope(ctx)
	if e != nil {
		t.Fatal(e)
	}
	e = zone.embedded.ensure(owned, 2028)
	release()
	if e != nil {
		t.Fatal(e)
	}
	if len(zone.embedded.transitions) != 3 {
		t.Fatal("union of distinct RDATE sets lost")
	}
}
func TestNHA101DomainLocalCodecRejectsNewResolvedOrigin(t *testing.T) {
	ctx := context.Background()
	zone, e := ParseZone("America/New_York")
	if e != nil {
		t.Fatal(e)
	}
	anchor := nha100Local(t, "2026-11-01T01:30:00")
	end, e := ParseInstant("2026-11-01T06:00:00Z")
	if e != nil {
		t.Fatal(e)
	}
	span, e := TimedSpan(ctx, anchor, end, zone)
	if e != nil {
		t.Fatal(e)
	}
	point, e := ParsePoint(Timed, "2026-11-01T01:30:00@2026-11-01T06:30:00Z")
	if e != nil {
		t.Fatal(e)
	}
	_, e = NewEvent(ctx, EventInput{Summary: "local", Span: span, Rule: NoRecurrence(), Additions: []Point{point}})
	nha100Code(t, e, Incompatible)
	wrong, e := ParsePoint(Timed, "2026-11-01T02:30:00@2026-11-01T06:30:00Z")
	if e != nil {
		t.Fatal(e)
	}
	content, e := NewEventContent(AbsentText(), AbsentText(), AbsentText())
	if e != nil {
		t.Fatal(e)
	}
	_, e = NewImportedEvent(ctx, ImportedEventInput{Content: content, Span: span, Rule: NoRecurrence(), Additions: []Point{wrong}})
	nha100Code(t, e, InvalidOccurrence)
}

func TestNHA101DomainPrivatePortsZeroAndTypedNil(t *testing.T) {
	ctx := context.Background()
	var iterator *pinnedNext
	_, _, e := iterator.take(ctx)
	nha100Code(t, e, InvalidInput)
	_, _, e = (&pinnedNext{}).take(ctx)
	nha100Code(t, e, InvalidInput)
	_, e = newPinnedNext(nil, rrule.ROption{}, time.Time{})
	nha100Code(t, e, InvalidInput)
	type nilContext struct{ context.Context }
	var typedNil *nilContext
	_, e = newPinnedNext(typedNil, rrule.ROption{}, time.Time{})
	nha100Code(t, e, InvalidInput)
	var owner *admissionSession
	nha100Code(t, owner.reserveTo(1), InvalidInput)
	nha100Code(t, (&admissionSession{}).reserveTo(1), InvalidInput)
}

func TestNHA101DomainRawZeroTransitionIsPresent(t *testing.T) {
	ctx := context.Background()
	anchor := nha100Local(t, "0001-01-01T00:00:00")
	zone, e := AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/ZeroTransition", Observances: []EmbeddedObservanceInput{{Anchor: anchor, OffsetFrom: 0, OffsetTo: 3600}}})
	if e != nil {
		t.Fatal(e)
	}
	owned, release, e := admissionScope(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	boundary, e := zone.boundsAt(owned, anchor.wallUTC().Add(-time.Second))
	if e != nil || !boundary.present || !boundary.at.IsZero() {
		t.Fatalf("raw zero transition lost: %+v %v", boundary, e)
	}
	value, present, e := resolveWall(owned, nha100Local(t, "0001-01-01T00:30:00"), zone, true)
	if e != nil || !present || value.String() != "0001-01-01T00:30:00Z" {
		t.Fatalf("valid civil edge lost: %s present=%t error=%v", value.String(), present, e)
	}
	if _, e = NewInstant(time.Time{}); e == nil {
		t.Fatal("Instant zero contract widened")
	}
}

func TestNHA101DomainNominalElapsedBoundAcrossLargeOffsetChange(t *testing.T) {
	ctx := context.Background()
	zone, e := AdmitEmbeddedZone(ctx, EmbeddedZoneInput{Name: "Candidate/LargeFold", Observances: []EmbeddedObservanceInput{
		{Anchor: nha100Local(t, "2025-01-01T00:00:00"), OffsetFrom: 23 * 3600, OffsetTo: 23 * 3600},
		{Anchor: nha100Local(t, "2026-03-08T00:00:00"), OffsetFrom: 23 * 3600, OffsetTo: -23 * 3600},
	}})
	if e != nil {
		t.Fatal(e)
	}
	length, e := NewNominalLength(366, 0)
	if e != nil {
		t.Fatal(e)
	}
	span, e := TimedNominalSpan(ctx, nha100Local(t, "2026-03-06T12:00:00"), length, zone)
	if e != nil {
		t.Fatal(e)
	}
	start, end, _, ok := span.Instants()
	if !ok || start.String() != "2026-03-05T13:00:00Z" || end.String() != "2027-03-08T11:00:00Z" {
		t.Fatalf("span=%s..%s valid=%t", start.String(), end.String(), ok)
	}
	if end.value.Sub(start.value) != 366*24*time.Hour+46*time.Hour {
		t.Fatal("nominal length confused with one-hour DST or exact elapsed limit")
	}
}
