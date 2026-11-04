package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// These historical bytes are provenance, not a supported public codec. Current
// semantic admissions use auth_resource_golden and home.api.v2 exclusively.
func TestNHA203HistoricalAuthFixtureIntegrity(t *testing.T) {
	data, err := os.ReadFile("auth_historical_sha256.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]string
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 96 {
		t.Fatalf("historical inventory: %d", len(expected))
	}
	seen := 0
	for _, dir := range []string{"auth_golden", "authorization_golden"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				t.Fatal("unexpected historical directory")
			}
			path := filepath.ToSlash(filepath.Join(dir, entry.Name()))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			if hex.EncodeToString(digest[:]) != expected[path] {
				t.Fatalf("historical fixture changed: %s", path)
			}
			seen++
		}
	}
	if seen != len(expected) {
		t.Fatalf("historical file count: %d", seen)
	}
}
