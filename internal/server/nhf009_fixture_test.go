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
		`{"sequence":1,"action":"observe-last","request_id":""}`,
		`{"sequence":1,"action":"confirm-last","request_id":"native-command"}`,
		`{"sequence":1,"action":"confirm-last","request_id":"01234567-89ab-4def-8123-0123456789ab","command_id":"legacy"}`,
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

func TestNHF009FixtureControlCarriesStrictPublicRequestUUID(t *testing.T) {
	root := t.TempDir()
	const id = "01234567-89ab-4def-8123-0123456789ab"
	for _, action := range []string{"observe-last", "confirm-last"} {
		input := `{"sequence":1,"action":"` + action + `","request_id":"` + id + `"}`
		if err := os.WriteFile(filepath.Join(root, "command.json"), []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := nhf009ReadControl(root, 0)
		if err != nil || got == nil || got.request == nil || got.request.String() != id {
			t.Fatal("request UUID not preserved", err)
		}
	}
}
