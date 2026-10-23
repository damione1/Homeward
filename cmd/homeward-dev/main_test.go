package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestNoImplicitMockOrInstallationMutation(t *testing.T) {
	data := filepath.Join(t.TempDir(), "instance")
	for _, args := range [][]string{nil, {"--data-dir", data, "--tls-cert", "unused", "--tls-key", "unused"}, {"--mock"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatal("incomplete or implicit mock startup accepted")
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Fatalf("implicit mock startup mutated storage: %v", err)
		}
	}
}
