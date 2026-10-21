package calendars

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type oracleDocument struct {
	StageA []map[string]any `json:"stage_a_current"`
	StageB []map[string]any `json:"stage_b_not_wired"`
	Guard  []map[string]any `json:"guard"`
}

func oracleString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("oracle %s missing/string: %#v", key, m)
	}
	return s
}
func oracleStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("oracle strings: %#v", v)
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		s, ok := x.(string)
		if !ok {
			t.Fatalf("oracle string item: %#v", x)
		}
		out = append(out, s)
	}
	return out
}
func fixtureRaw(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", "nha101", "fixtures", name))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func oracles(t *testing.T) oracleDocument {
	t.Helper()
	b, e := os.ReadFile("testdata/nha101/oracles.json")
	if e != nil {
		t.Fatal(e)
	}
	var o oracleDocument
	if e = json.Unmarshal(b, &o); e != nil {
		t.Fatal(e)
	}
	return o
}
func importFixture(t *testing.T, id CalendarID, file string, projection string) (*remoteImage, error) {
	t.Helper()
	var z *Zone
	if projection != "" {
		parsed, e := ParseZone(projection)
		if e != nil {
			t.Fatal(e)
		}
		z = &parsed
	}
	image, err := admitRemoteICS(context.Background(), id, fixtureRaw(t, file), z)
	if err == nil {
		t.Cleanup(image.close)
	}
	return image, err
}
func queryDate(t *testing.T, c *remoteImage, year string) []Occurrence {
	t.Helper()
	from, e := ParseDate(year + "-01-01")
	if e != nil {
		t.Fatal(e)
	}
	nextYear := "2027"
	if year == "2108" {
		nextYear = "2109"
	}
	to, e := ParseDate(nextYear + "-01-01")
	if e != nil {
		t.Fatal(e)
	}
	w, e := DateWindow(from, to)
	if e != nil {
		t.Fatal(e)
	}
	out, e := nha101DomainQuery(c, w)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func queryTime(t *testing.T, c *remoteImage, year string) []Occurrence {
	t.Helper()
	from, e := ParseInstant(year + "-01-01T00:00:00Z")
	if e != nil {
		t.Fatal(e)
	}
	nextYear := "2027"
	if year == "2108" {
		nextYear = "2109"
	}
	to, e := ParseInstant(nextYear + "-01-01T00:00:00Z")
	if e != nil {
		t.Fatal(e)
	}
	z, e := ParseZone("UTC")
	if e != nil {
		t.Fatal(e)
	}
	w, e := TimeWindow(from, to, z)
	if e != nil {
		t.Fatal(e)
	}
	out, e := nha101DomainQuery(c, w)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func byUID(items []Occurrence, uid string) []Occurrence {
	out := make([]Occurrence, 0)
	for _, o := range items {
		value, ok := o.ID().Event().UID().Value()
		if ok && value == uid {
			out = append(out, o)
		}
	}
	return out
}
func requireTimes(t *testing.T, o Occurrence, start, end string) {
	t.Helper()
	a, b, _, ok := o.Span().Instants()
	if !ok || a.String() != start || b.String() != end {
		t.Fatalf("timed span got %s..%s valid=%t, want %s..%s", a.String(), b.String(), ok, start, end)
	}
}
func requireDates(t *testing.T, o Occurrence, start, end string) {
	t.Helper()
	a, b, ok := o.Span().Dates()
	if !ok || a.String() != start || b.String() != end {
		t.Fatalf("civil span got %s..%s valid=%t, want %s..%s", a.String(), b.String(), ok, start, end)
	}
}
func requireText(t *testing.T, v TextValue, want any) {
	t.Helper()
	expected, ok := want.(map[string]any)
	if !ok {
		t.Fatalf("text oracle: %#v", want)
	}
	state := oracleString(t, expected, "state")
	got, present := v.Value()
	if (state == "Present") != present {
		t.Fatalf("presence got %t want %s", present, state)
	}
	if present && got != oracleString(t, expected, "value") {
		t.Fatalf("text got %q want %q", got, oracleString(t, expected, "value"))
	}
}
func requireIssue(t *testing.T, err error, issue string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected issue %s", issue)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected domain issue %s, got %T: %v", issue, err, err)
	}
	want := map[string]ErrorCode{"MissingFloatingZone": MissingFloatingZone, "ConflictingEnd": ConflictingEnd, "ContradictoryIdentity": ContradictoryIdentity}[issue]
	if e.Code() != want {
		t.Fatalf("issue got %v want %s", e.Code(), issue)
	}
}
func TestNHA101DomainPinnedHistoricalOracles(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	o := oracles(t)
	for _, entry := range o.StageA {
		file := oracleString(t, entry, "file")
		t.Run("A/"+file, func(t *testing.T) {
			imported, err := importFixture(t, id, file, "")
			if err != nil {
				t.Fatal(err)
			}
			uid := oracleString(t, entry, "uid")
			if entry["kind"] == "AllDay" {
				items := byUID(queryDate(t, imported, "2026"), uid)
				if len(items) != 1 {
					t.Fatalf("occurrences %d", len(items))
				}
				requireDates(t, items[0], oracleString(t, entry, "start"), oracleString(t, entry, "end_exclusive"))
				if want, ok := entry["summary"].(string); ok && items[0].Summary() != want {
					t.Fatalf("summary %q", items[0].Summary())
				}
			} else {
				items := byUID(queryTime(t, imported, "2026"), uid)
				if len(items) != 1 {
					t.Fatalf("occurrences %d", len(items))
				}
				requireTimes(t, items[0], oracleString(t, entry, "start"), oracleString(t, entry, "end_exclusive"))
			}
		})
	}
	for i, entry := range o.StageB {
		file := oracleString(t, entry, "file")
		t.Run(file+"/"+string(rune('a'+i)), func(t *testing.T) {
			projection, _ := entry["configuration_projection"].(string)
			imported, err := importFixture(t, id, file, projection)
			if issue, ok := entry["issue"].(string); ok {
				requireIssue(t, err, issue)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch file {
			case "04-default-date.ics":
				items := queryDate(t, imported, "2026")
				if len(items) != 1 {
					t.Fatalf("occurrences %d", len(items))
				}
				requireDates(t, items[0], oracleString(t, entry, "start"), oracleString(t, entry, "end_exclusive"))
				c, ok := items[0].Content()
				if !ok {
					t.Fatal("missing imported content")
				}
				requireText(t, c.Summary(), entry["summary"])
			case "05-point.ics":
				items := queryTime(t, imported, "2026")
				if len(items) != 1 || !items[0].Span().IsPoint() {
					t.Fatalf("point occurrences %#v", items)
				}
				requireTimes(t, items[0], oracleString(t, entry, "at"), oracleString(t, entry, "at"))
				c, ok := items[0].Content()
				if !ok {
					t.Fatal("missing imported content")
				}
				requireText(t, c.Summary(), entry["summary"])
			case "06-duration.ics":
				uid := oracleString(t, entry, "uid")
				items := byUID(queryTime(t, imported, "2026"), uid)
				expected, ok := entry["occurrences"].([]any)
				if !ok || len(items) != len(expected) {
					t.Fatalf("occurrences %d expected %#v", len(items), entry["occurrences"])
				}
				for j, pair := range expected {
					times := oracleStrings(t, pair)
					if len(times) != 2 {
						t.Fatal("pair")
					}
					requireTimes(t, items[j], times[0], times[1])
				}
			case "07-overrides.ics":
				items := byUID(queryDate(t, imported, "2026"), oracleString(t, entry, "uid"))
				dates := make([]string, 0, len(items))
				for _, item := range items {
					start, _, ok := item.Span().Dates()
					if !ok {
						t.Fatal("not civil")
					}
					dates = append(dates, start.String())
				}
				if !reflect.DeepEqual(dates, oracleStrings(t, entry["effective_dates"])) {
					t.Fatalf("effective dates got %v", dates)
				}
				moved := entry["moved"].(map[string]any)
				last := items[len(items)-1]
				requireDates(t, last, oracleString(t, moved, "start"), oracleString(t, moved, "end_exclusive"))
				if last.ID().Point().String() != oracleString(t, moved, "original") {
					t.Fatalf("identity %s", last.ID().Point().String())
				}
				content, ok := last.Content()
				if !ok {
					t.Fatal("replacement content absent")
				}
				requireText(t, content.Summary(), moved["summary"])
				requireText(t, content.Description(), moved["description"])
				requireText(t, content.Location(), moved["location"])
				again := byUID(queryDate(t, imported, "2026"), oracleString(t, entry, "uid"))
				if !reflect.DeepEqual(items, again) {
					t.Fatal("repeat/reorder changed identities or payload")
				}
			case "08-vtimezone-fixed.ics", "09-vtimezone-collision.ics", "17-observance-sparse.ics":
				year := "2026"
				if file == "17-observance-sparse.ics" {
					year = "2108"
				}
				items := queryTime(t, imported, year)
				if file != "17-observance-sparse.ics" {
					items = byUID(items, oracleString(t, entry, "uid"))
				}
				if len(items) != 1 {
					t.Fatalf("occurrences %d", len(items))
				}
				requireTimes(t, items[0], oracleString(t, entry, "start"), oracleString(t, entry, "end_exclusive"))
				_, _, zone, _ := items[0].Span().Instants()
				if !strings.HasPrefix(zone.DataID(), "embedded-vtimezone/sha256:") {
					t.Fatalf("authority %s", zone.DataID())
				}
				if file == "17-observance-sparse.ics" {
					onsets, e := nha101EmbeddedOnsets(imported, "Custom/Sparse", 2108)
					if e != nil {
						t.Fatal(e)
					}
					got := make([]string, 0, len(onsets))
					for _, onset := range onsets {
						got = append(got, onset.String())
					}
					if !reflect.DeepEqual(got, oracleStrings(t, entry["observance_onsets_utc"])) {
						t.Fatalf("onsets got %v", got)
					}
				}
			case "10-vtimezone-dst.ics":
				uid := oracleString(t, entry, "uid")
				items := byUID(queryTime(t, imported, "2026"), uid)
				if raw, ok := entry["starts"]; ok {
					expected := oracleStrings(t, raw)
					got := make([]string, 0, len(items))
					for _, item := range items {
						a, _, _, _ := item.Span().Instants()
						got = append(got, a.String())
					}
					if !reflect.DeepEqual(got, expected) {
						t.Fatalf("starts got %v", got)
					}
					for _, item := range items {
						a, b, _, _ := item.Span().Instants()
						start, _ := a.Time()
						end, _ := b.Time()
						if end.Sub(start).Seconds() != 3600 {
							t.Fatal("duration")
						}
					}
				} else {
					if len(items) != 1 {
						t.Fatalf("occurrences %d", len(items))
					}
					requireTimes(t, items[0], oracleString(t, entry, "start"), oracleString(t, entry, "end_exclusive"))
				}
			case "11-floating-xwr.ics":
				items := queryTime(t, imported, "2026")
				if len(items) != 1 {
					t.Fatalf("occurrences %d", len(items))
				}
				a, _, _, _ := items[0].Span().Instants()
				if a.String() != oracleString(t, entry, "start") || !items[0].Span().Floating() {
					t.Fatalf("floating at %s retained=%t", a.String(), items[0].Span().Floating())
				}
			case "16-folded-text.ics":
				items := queryTime(t, imported, "2026")
				if len(items) != 1 {
					t.Fatalf("occurrences %d", len(items))
				}
				content, ok := items[0].Content()
				if !ok {
					t.Fatal("content absent")
				}
				summary, _ := content.Summary().Value()
				description, _ := content.Description().Value()
				location, _ := content.Location().Value()
				if summary != oracleString(t, entry, "summary") || description != oracleString(t, entry, "description") || location != oracleString(t, entry, "location") {
					t.Fatalf("folded text: %q %q %q", summary, description, location)
				}
			default:
				t.Fatalf("unwired oracle %s", file)
			}
		})
	}
	raw := fixtureRaw(t, "14-truncated.ics")
	_, _, guardErr := guard(context.Background(), raw)
	if g, ok := guardErr.(guardFailure); !ok || g.issue != UnclosedComponent {
		t.Fatalf("fixture14 guard got %v", guardErr)
	}
	if _, err := admitRemoteICS(context.Background(), id, raw, nil); err == nil {
		t.Fatal("fixture14 admitted")
	}
}
