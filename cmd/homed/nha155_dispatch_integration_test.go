package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
)

func TestNHA155HomedDispatchRecoveryWithoutStartupOrSQL(t *testing.T) {
	operator, serverDir := nha155PrivateRecoveryDir(t), nha155PrivateRecoveryDir(t)
	private, public := filepath.Join(operator, "recovery.key"), filepath.Join(serverDir, "recovery.pub")
	var out, diagnostics bytes.Buffer
	// No server dependencies are admitted at all; the offline branch must run
	// before their validation, configuration reads, SQL admission and listeners.
	if code := runHomed(context.Background(), []string{"recovery", "export", "--private-output", private, "--recipient-output", public, "--server-data-dir", serverDir}, &out, &diagnostics, homedDependencies{}); code != 0 {
		t.Fatalf("offline export failed with admitted private fixtures (code %d)", code)
	}
	called := false
	dependencies := homedDependencies{
		read: func(string) ([]byte, error) { called = true; return nil, errors.New("forbidden configuration read") },
		runStartup: func(context.Context, config.StartupInputs, server.ListenFunc, func(net.Addr)) error {
			called = true
			return errors.New("forbidden SQL startup")
		},
		listen: func(context.Context, string, string) (net.Listener, error) {
			called = true
			return nil, errors.New("forbidden listener")
		},
	}
	for _, command := range [][]string{
		{"recovery", "verify", "--private-file", private, "--recipient-file", public, "--server-data-dir", serverDir},
		{"recovery", "copy", "--private-file", private, "--private-output", filepath.Join(operator, "second.key"), "--server-data-dir", serverDir},
	} {
		if code := runHomed(context.Background(), command, &out, &diagnostics, dependencies); code != 0 || called {
			t.Fatal("offline recovery called the server/SQL dependency path")
		}
	}
	key, err := os.ReadFile(private)
	if err != nil {
		t.Fatal("inspect offline fixture export")
	}
	if strings.Contains(out.String()+diagnostics.String(), strings.TrimSpace(string(key))) || strings.Contains(out.String()+diagnostics.String(), "AGE-SECRET-KEY-") {
		t.Fatal("offline dispatch leaked private material")
	}
	out.Reset()
	diagnostics.Reset()
	if code := runHomed(context.Background(), []string{"recovery", "verify", "--private-file", "NHA155_MISSING_PRIVATE_CANARY", "--recipient-file", public, "--server-data-dir", serverDir}, &out, &diagnostics, dependencies); code == 0 || called {
		t.Fatal("failed offline recovery fell through to startup")
	}
	if strings.Contains(out.String()+diagnostics.String(), "CANARY") {
		t.Fatal("offline dispatch rendered private input")
	}
}
