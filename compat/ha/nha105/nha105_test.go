package nha105

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

type haSunResult struct {
	sunResult
	Error string `json:"error,omitempty"`
}

type haCaseResult struct {
	ID        string        `json:"id"`
	AfterUTC  string        `json:"after_utc"`
	Date      string        `json:"date"`
	AnchorUTC string        `json:"anchor_utc,omitempty"`
	Events    []eventResult `json:"events"`
	Sun       haSunResult   `json:"sun"`
	Position  struct {
		State     string  `json:"state"`
		Elevation float64 `json:"elevation"`
		Azimuth   float64 `json:"azimuth"`
		Rising    bool    `json:"rising"`
	} `json:"position"`
}

func nha105Read(t *testing.T, name string) []byte {
	t.Helper()
	value, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nha105Time(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	_, offset := value.Zone()
	if offset != 0 {
		t.Fatal("fixture result is not UTC")
	}
	return value.UTC()
}

func nha105CompareTime(t *testing.T, actual, expected string) {
	t.Helper()
	delta := nha105Time(t, actual).Sub(nha105Time(t, expected))
	if delta < -time.Second || delta > time.Second {
		t.Fatalf("HA event difference %s exceeds one second", delta)
	}
}

func nha105CompareOccurrence(t *testing.T, actual, expected occurrenceResult) {
	t.Helper()
	if actual.Kind != expected.Kind {
		t.Fatalf("presence differs: %s vs %s", actual.Kind, expected.Kind)
	}
	switch actual.Kind {
	case "present":
		if actual.Reason != "" || expected.Reason != "" {
			t.Fatal("present timestamp contains an absence")
		}
		nha105CompareTime(t, actual.Instant, expected.Instant)
	case "absent":
		if actual.Instant != "" || expected.Instant != "" || actual.Reason != expected.Reason || (actual.Reason != "no_transit_on_date" && actual.Reason != "no_event_within_search_horizon") {
			t.Fatal("absence contains a forged timestamp or unknown reason")
		}
	default:
		t.Fatal("unknown occurrence branch")
	}
}

func TestNHA105HAProfileFixtures(t *testing.T) {
	fixtureBytes := nha105Read(t, "cases.json")
	workerBytes := nha105Read(t, "ha_worker.py")
	archiveBytes, err := os.ReadFile("observed.json")
	if err != nil {
		t.Fatalf("NHA-105 differential P0 has not produced an observed HA archive: %v", err)
	}
	var archive struct {
		Inputs struct {
			CasesSHA256  string `json:"cases_sha256"`
			WorkerSHA256 string `json:"ha_worker_sha256"`
		} `json:"inputs"`
		HAProfileCommit string `json:"ha_profile_commit"`
		HA              struct {
			HomeAssistant string         `json:"homeassistant"`
			Astral        string         `json:"astral"`
			Cases         []haCaseResult `json:"cases"`
		} `json:"ha"`
	}
	if err := json.Unmarshal(archiveBytes, &archive); err != nil {
		t.Fatal(err)
	}
	fixtureHash, workerHash := sha256.Sum256(fixtureBytes), sha256.Sum256(workerBytes)
	if archive.Inputs.CasesSHA256 != hex.EncodeToString(fixtureHash[:]) || archive.Inputs.WorkerSHA256 != hex.EncodeToString(workerHash[:]) || archive.HA.HomeAssistant != "2026.10.1" || archive.HA.Astral != "2.2" || archive.HAProfileCommit != "abe1fdd7cd6b4de1715c87b6fd87d92699514ef9" {
		t.Fatal("HA oracle is stale or uses a different reference profile")
	}
	actualJSON, err := RunJSON(context.Background(), fixtureBytes)
	if err != nil {
		t.Fatal(err)
	}
	var actual probeResult
	if err := json.Unmarshal(actualJSON, &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual.Cases) != len(archive.HA.Cases) || len(actual.Cases) < 20 {
		t.Fatal("differential archive does not cover the intended synthetic cases")
	}
	for index, got := range actual.Cases {
		want := archive.HA.Cases[index]
		t.Run(got.ID, func(t *testing.T) {
			if got.ID != want.ID || got.Date != want.Date || len(got.Events) != 6 || len(want.Events) != 6 {
				t.Fatal("oracle identity, civil date, or event count differs")
			}
			for eventIndex, event := range got.Events {
				expected := want.Events[eventIndex]
				if event.Event != expected.Event {
					t.Fatal("oracle event order differs")
				}
				nha105CompareOccurrence(t, event.Date, expected.Date)
				nha105CompareOccurrence(t, event.Next, expected.Next)
				if event.Next.Kind == "present" && !nha105Time(t, event.Next.Instant).After(nha105Time(t, got.AfterUTC)) {
					t.Fatal("next event is not strictly future")
				}
			}
			if got.Sun.State != want.Position.State || got.Sun.Rising != want.Position.Rising || math.Abs(got.Sun.Elevation-want.Position.Elevation) > 0.010000001 || math.Abs(got.Sun.Azimuth-want.Position.Azimuth) > 0.010000001 {
				t.Fatal("primary Astral/HA position, horizon, or rising differs")
			}
			if want.Sun.Error != "" {
				if want.Sun.Error != "no_event_within_search_horizon" {
					t.Fatal("unexpected reference entity error")
				}
				return // Daily/next helper comparisons above still apply.
			}
			if got.Sun.State != want.Sun.State || got.Sun.Rising != want.Sun.Rising || got.Sun.Phase != want.Sun.Phase || math.Abs(got.Sun.Elevation-want.Sun.Elevation) > 0.010000001 || math.Abs(got.Sun.Azimuth-want.Sun.Azimuth) > 0.010000001 {
				t.Fatal("HA horizon, position, rising or phase differs")
			}
			nha105CompareTime(t, got.Sun.NextChange, want.Sun.NextChange)
			if (got.Sun.NextPosition == "") != (want.Sun.NextPosition == "") {
				t.Fatal("HA position timer presence differs")
			}
			if got.Sun.NextPosition != "" {
				nha105CompareTime(t, got.Sun.NextPosition, want.Sun.NextPosition)
			}
			if len(got.Sun.Next) != 6 || len(want.Sun.Next) != 6 {
				t.Fatal("HA entity is missing next-event attributes")
			}
			for i := range got.Sun.Next {
				nha105CompareOccurrence(t, got.Sun.Next[i], want.Sun.Next[i])
			}
		})
	}
}

func TestNHA105ProbeRejectsUnadmittedFixtureInput(t *testing.T) {
	valid := nha105Read(t, "cases.json")
	for _, bad := range [][]byte{
		nil,
		bytes.Repeat([]byte(" "), (64<<10)+1),
		[]byte(`{"profile":"future","cases":[]}`),
		append(append([]byte(nil), valid...), []byte(` {}`)...),
		bytes.Replace(valid, []byte(`"profile": "ha-2026.10/core-1"`), []byte(`"unsupported":true,"profile":"ha-2026.10/core-1"`), 1),
	} {
		if _, err := RunJSON(context.Background(), bad); err == nil {
			t.Fatal("invalid fixture admitted as a compatibility observation")
		}
	}
}
