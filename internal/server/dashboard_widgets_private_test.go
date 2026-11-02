package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Mutable control/status snapshots belong to a fresh, private fixture directory.
// Rename publishes complete JSON; readers never see a partially replaced file.
func dashboardWidgetsWritePrivate(t *testing.T, directory, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(directory, ".dashboard-writing-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(file.Name(), filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
}
