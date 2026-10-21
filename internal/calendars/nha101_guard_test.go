package calendars

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func costRaw(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "nha101", "cost", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func costEntries(t *testing.T) []struct {
	File  string `json:"file"`
	Guard string `json:"guard"`
	Phase string `json:"phase"`
} {
	t.Helper()
	raw, err := os.ReadFile("testdata/nha101/cost-oracles.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Entries []struct {
			File  string `json:"file"`
			Guard string `json:"guard"`
			Phase string `json:"phase"`
		} `json:"entries"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Entries
}
func guardIssueName(issue GuardIssue) string {
	switch issue {
	case BodyTooLarge:
		return "BodyTooLarge"
	case LineTooLarge:
		return "LineTooLarge"
	case TooManyLines:
		return "TooManyLines"
	case TooDeep:
		return "TooDeep"
	case TooManyComponents:
		return "TooManyComponents"
	case TooManyEvents:
		return "TooManyEvents"
	case TooManyZones:
		return "TooManyZones"
	case InvalidText:
		return "InvalidText"
	case UnbalancedComponent:
		return "UnbalancedComponent"
	case UnclosedComponent:
		return "UnclosedComponent"
	case InvalidEnvelope:
		return "InvalidEnvelope"
	case GuardInvalid:
		return "GuardInvalid"
	}
	return "InvalidIssue"
}

func TestNHA101DomainGuardLimitsAndPlusOne(t *testing.T) {
	for _, entry := range costEntries(t) {
		t.Run(entry.File, func(t *testing.T) {
			_, _, err := guard(context.Background(), costRaw(t, entry.File))
			if entry.Guard == "admit" {
				if err != nil {
					t.Fatalf("guard rejected boundary/control: %v", err)
				}
				return
			}
			failure, ok := err.(guardFailure)
			if !ok || guardIssueName(failure.issue) != entry.Guard {
				t.Fatalf("got %v, want %s", err, entry.Guard)
			}
		})
	}
}
