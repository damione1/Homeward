package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNHF009FixtureControlsRejectUnknownFieldsAndUnboundConfirmations(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "command.json")
	for _, input := range []string{
		`{"sequence":1,"action":"future"}`,
		`{"sequence":1,"action":"confirm-last"}`,
		`{"sequence":1,"action":"observe-last","command_id":""}`,
		`{"sequence":1,"action":"allow-write","command_id":"unexpected"}`,
		`{"sequence":0,"action":"allow-write"}`,
		`{"sequence":2,"action":"allow-write"}`,
		`{"sequence":1,"action":"allow-write","provider":"https://invalid.example"}`,
		`{"sequence":1,"action":"allow-write"} {}`,
	} {
		if err := os.WriteFile(file, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := nhf009ReadControl(root, 0); err == nil {
			t.Fatalf("accepted unbound control: %s", input)
		}
	}
	if err := os.WriteFile(file, []byte(`{"sequence":1,"action":"allow-write"}`), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := nhf009ReadControl(root, 0)
	if err != nil || value == nil || value.action != nhf009AllowWrite {
		t.Fatal(value, err)
	}
	value, err = nhf009ReadControl(root, 1)
	if err != nil || value != nil {
		t.Fatal("replayed filesystem control", err)
	}
}
