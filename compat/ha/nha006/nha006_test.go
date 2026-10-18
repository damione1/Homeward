package nha006

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestNHA006ObservedGonja(t *testing.T) {
	fixtures, err := os.ReadFile("cases.json")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := RunJSON(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile("observed.json")
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Inputs struct {
			CasesSHA256    string `json:"cases_sha256"`
			HAWorkerSHA256 string `json:"ha_worker_sha256"`
		} `json:"inputs"`
		Gonja struct {
			Cases json.RawMessage `json:"cases"`
		} `json:"gonja"`
	}
	if err := json.Unmarshal(archive, &recorded); err != nil {
		t.Fatal(err)
	}
	worker, err := os.ReadFile("ha_worker.py")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(fixtures)); got != recorded.Inputs.CasesSHA256 {
		t.Fatalf("recorded cases use an older fixture snapshot: %s != %s", recorded.Inputs.CasesSHA256, got)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(worker)); got != recorded.Inputs.HAWorkerSHA256 {
		t.Fatalf("recorded HA result uses an older worker: %s != %s", recorded.Inputs.HAWorkerSHA256, got)
	}
	var got, want any
	var result struct {
		Cases json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(actual, &result); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.Cases, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recorded.Gonja.Cases, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Gonja observations drifted:\nactual=%s\nrecorded=%s", result.Cases, recorded.Gonja.Cases)
	}
}

func TestNHA006BadFixtureFailsBeforeMeasurement(t *testing.T) {
	fixtures, err := os.ReadFile("cases.json")
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Replace(fixtures, []byte(`"clock_utc": "2026-03-08T04:30:00+00:00"`), []byte(`"clock_utc": "not-a-clock"`), 1)
	if bytes.Equal(bad, fixtures) {
		t.Fatal("fixture clock was not found")
	}
	if _, err := RunJSON(bad); err == nil {
		t.Fatal("malformed fixture was accepted as a compatibility observation")
	}
}
