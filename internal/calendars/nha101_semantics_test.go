package calendars

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type preflightOccurrence struct {
	UID     string `json:"uid"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Origin  string `json:"origin"`
	Summary string `json:"summary"`
}
type preflightOracle struct {
	File          string                `json:"file"`
	Outcome       string                `json:"outcome"`
	Expected      []preflightOccurrence `json:"expected"`
	Zone          string                `json:"zone"`
	ThroughYear   int                   `json:"through_year"`
	RequiredOnset string                `json:"required_onset"`
}

func preflightQuery(t *testing.T, imported *remoteImage) []preflightOccurrence {
	t.Helper()
	from, err := ParseInstant("2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	to, err := ParseInstant("2027-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	zone, err := ParseZone("UTC")
	if err != nil {
		t.Fatal(err)
	}
	window, err := TimeWindow(from, to, zone)
	if err != nil {
		t.Fatal(err)
	}
	values, err := nha101DomainQuery(imported, window)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]preflightOccurrence, 0, len(values))
	for _, value := range values {
		uid, ok := value.ID().Event().UID().Value()
		if !ok {
			t.Fatal("invalid UID")
		}
		start, end, _, ok := value.Span().Instants()
		if !ok {
			t.Fatal("not timed")
		}
		result = append(result, preflightOccurrence{UID: uid, Start: start.String(), End: end.String(), Origin: value.ID().Point().String(), Summary: value.Summary()})
	}
	// Only compare a canonical representation, not a candidate's internal map order.
	sort.Slice(result, func(i, j int) bool {
		if result[i].UID != result[j].UID {
			return result[i].UID < result[j].UID
		}
		return result[i].Origin < result[j].Origin
	})
	return result
}

func TestNHA101DomainSemanticCounterexamples(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	raw, err := os.ReadFile("testdata/nha101/new-oracles.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Oracles []preflightOracle `json:"oracles"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, entry := range document.Oracles {
		t.Run(entry.File, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata", "nha101", "fixtures", entry.File))
			if err != nil {
				t.Fatal(err)
			}
			imported, err := admitRemoteICS(context.Background(), id, input, nil)
			if err == nil {
				t.Cleanup(imported.close)
			}
			switch entry.Outcome {
			case "rejected":
				if err == nil {
					t.Fatal("invalid input admitted; expected rejection")
				}
			case "occurrences":
				if err != nil {
					t.Fatalf("valid input rejected: %v", err)
				}
				got := preflightQuery(t, imported)
				want := append([]preflightOccurrence(nil), entry.Expected...)
				sort.Slice(want, func(i, j int) bool {
					if want[i].UID != want[j].UID {
						return want[i].UID < want[j].UID
					}
					return want[i].Origin < want[j].Origin
				})
				if len(got) != len(want) {
					t.Fatalf("got %d occurrences, want %d: %#v", len(got), len(want), got)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("occurrence %d got %#v, want %#v", i, got[i], want[i])
					}
				}
			case "required_onset":
				if err != nil {
					t.Fatalf("valid sparse zone rejected: %v", err)
				}
				onsets, err := nha101EmbeddedOnsets(imported, entry.Zone, entry.ThroughYear)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, onset := range onsets {
					if onset.String() == entry.RequiredOnset {
						found = true
					}
				}
				if !found {
					t.Fatalf("lost recurrence after library default Until: missing %s", entry.RequiredOnset)
				}
			default:
				t.Fatalf("unknown oracle outcome %q", entry.Outcome)
			}
		})
	}
}

// These are domain-unit helpers only, not a Repository/page implementation.
// They exercise expandEvent and the same budget held by remote admission.
func nha101DomainQuery(image *remoteImage, window Window) ([]Occurrence, error) {
	if !image.owned() || !window.Valid() {
		return nil, failure(InvalidInput)
	}
	if err := contextValid(image.ctx); err != nil {
		return nil, err
	}
	out := make([]Occurrence, 0)
	for _, event := range image.events {
		if err := expandEvent(image.ctx, event, window, zoneBudget(image.ctx), func(o Occurrence) error { out = append(out, o); return nil }); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return compareOrder(out[i].order, out[j].order) < 0 })
	return out, nil
}
func nha101EmbeddedOnsets(image *remoteImage, name string, year int) ([]Instant, error) {
	if !image.owned() {
		return nil, failure(InvalidInput)
	}
	z, ok := image.zones[name]
	if !ok || z.embedded == nil {
		return nil, failure(InvalidZone)
	}
	if err := z.embedded.ensure(image.ctx, year); err != nil {
		return nil, err
	}
	z.embedded.mu.Lock()
	defer z.embedded.mu.Unlock()
	out := make([]Instant, 0, len(z.embedded.transitions))
	for _, tr := range z.embedded.transitions {
		if tr.at.Year() > year {
			break
		}
		at, err := NewInstant(tr.at)
		if err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, nil
}
