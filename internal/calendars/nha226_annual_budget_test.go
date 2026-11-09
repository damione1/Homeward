package calendars

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNHA226AnnualCertificateGapBoundsAndStrictProfile(t *testing.T) {
	cases := []struct {
		rule, anchor string
		gap          int
	}{
		{"FREQ=YEARLY;BYMONTH=3;BYDAY=2SU", "2000-03-12T02:00:00", 1},
		{"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", "2000-02-29T02:00:00", 8},
		{"FREQ=YEARLY;BYMONTH=2;BYDAY=5MO", "2016-02-29T02:00:00", 40},
		{"FREQ=YEARLY;BYMONTH=2;BYDAY=-5MO", "2016-02-01T02:00:00", 40},
		{"FREQ=YEARLY;BYMONTH=3;BYDAY=5SU", "2020-03-29T02:00:00", 5},
	}
	for _, test := range cases {
		t.Run(test.rule, func(t *testing.T) {
			ctx, release, err := admissionScope(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			budget := newBudget(ctx)
			ctx = withCalendarBudget(ctx, budget)
			anchor := nha100Local(t, test.anchor)
			rule, err := parseObservanceRule(ctx, test.rule, anchor)
			if err != nil || rule.certificate.maxYearGap != test.gap {
				t.Fatalf("gap=%d error=%v", rule.certificate.maxYearGap, err)
			}
			if budget.work != 400 {
				t.Fatal("certificate cycle not charged exactly once", budget.work)
			}
			work, err := rule.certificate.nextWork()
			if err != nil || work != int64(test.gap+1)*366 {
				t.Fatal("producer scan/lookahead not bounded", err)
			}
			// Compare the certificate to the actual pinned producer across two full
			// Gregorian cycles, including non-leap century and cycle-wrap gaps.
			observance := embeddedObservance{anchor: anchor, rule: rule}
			var iterator *pinnedNext
			var credit observanceMaskCredit
			previous, largest, count := 0, 0, 0
			lastYear := int(anchor.date.year)
			for {
				if err := budget.charge(work, false); err != nil {
					t.Fatal(err)
				}
				if iterator == nil {
					iterator, credit, err = newObservanceIterator(ctx, observance, int(anchor.date.year)+800)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := credit.beforeNext(admissionFrom(ctx), lastYear); err != nil {
					t.Fatal(err)
				}
				next, present, err := iterator.take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !present {
					break
				}
				lastYear = next.Year()
				if previous != 0 {
					gap := next.Year() - previous
					if gap > test.gap {
						t.Fatal("producer exceeds certificate", gap)
					}
					largest = max(largest, gap)
				}
				previous = next.Year()
				count++
			}
			if count < 2 || largest != test.gap {
				t.Fatal("sparse/wrap witness missing", count, largest)
			}
		})
	}
	for _, raw := range []string{"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30", "FREQ=YEARLY;BYMONTH=3;BYDAY=2SU;INTERVAL=2", "FREQ=YEARLY;BYMONTH=3;BYDAY=0SU"} {
		if _, err := parseObservanceRule(context.Background(), raw, nha100Local(t, "2000-03-12T02:00:00")); err == nil {
			t.Fatal("invalid/unsupported annual rule admitted")
		}
	}
	if _, err := (annualCertificate{}).nextWork(); err == nil {
		t.Fatal("unadmitted work certificate")
	}
}

func TestNHA226MultiYearEmbeddedZoneAdmissionCodecAndQuery(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	raw, err := os.ReadFile("testdata/nha226/multi-year-zone.ics")
	if err != nil {
		t.Fatal(err)
	}
	image, observation, err := admitRemoteICSObserved(context.Background(), id, raw, nil)
	if err != nil {
		t.Fatalf("small multi-year source refused: %v counters=%+v", err, observation)
	}
	if len(image.events) != 2 || observation.ZoneCompilations < 3 || observation.NextCalls*146400 <= 32000000 {
		image.close()
		t.Fatal("fixture no longer reproduces old cumulative overcharge", observation)
	}
	payload, err := encodeRemoteImage(image.ctx, image)
	observation = image.parse.observation
	image.close()
	if err != nil || observation.Work >= 32000000 || observation.ReservedBytes > parseReservationLimit {
		t.Fatal("encode exceeded unchanged limits", err, observation)
	}
	decoded, err := decodeRemoteImage(context.Background(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.close()
	for _, date := range []string{"2020-01-15", "2026-10-19"} {
		from, _ := ParseInstant(date + "T00:00:00Z")
		to, _ := NewInstant(from.value.Add(24 * time.Hour))
		zone, _ := ParseZone("UTC")
		window, _ := TimeWindow(from, to, zone)
		occurrences, err := nha101DomainQuery(decoded, window)
		if err != nil {
			t.Fatal(err)
		}
		if len(occurrences) != 1 {
			t.Fatal("stored application image lost event", date, len(occurrences))
		}
		start, end, _, ok := occurrences[0].Span().Instants()
		if !ok || end.value.Sub(start.value) != time.Hour {
			t.Fatal("event instant/duration changed")
		}
	}
}
