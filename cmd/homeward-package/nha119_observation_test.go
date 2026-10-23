package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNHA119ObservationServeUsesExactGuardedInvocation(t *testing.T) {
	_, credentials, _ := packageFixture(t)
	password := strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(credentials, "password"), []byte(password), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"serve", "serve-observe-mqtt-readonly"} {
		t.Run(mode, func(t *testing.T) {
			for _, hint := range []string{"true", "false"} {
				existing := []string{"HOMEWARD_POSTGRES_PASSWORD=stale", "HOMEWARD_POSTGRES_PASSWORD=second-stale", "OTHER=preserved", "HOMEWARD_POSTGRES_PASSWORD_NOTE=preserved", "HOMEWARD_OBSERVE_MQTT_READONLY=" + hint}
				original := slices.Clone(existing)
				argv, env, err := serveInvocation(mode, credentials, existing)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"homed", "-config", "/var/lib/homeward/configuration.yaml"}
				if mode == "serve-observe-mqtt-readonly" {
					want = append(want, "-observe-mqtt-readonly")
				}
				if !slices.Equal(argv, want) || strings.Contains(strings.Join(argv, " "), password) {
					t.Fatal("server invocation differs from the explicit startup mode or includes a credential")
				}
				wantEnv := []string{"OTHER=preserved", "HOMEWARD_POSTGRES_PASSWORD_NOTE=preserved", "HOMEWARD_OBSERVE_MQTT_READONLY=" + hint, "HOMEWARD_POSTGRES_PASSWORD=" + password}
				if !slices.Equal(env, wantEnv) || !slices.Equal(existing, original) {
					t.Fatal("server credential environment was not deduplicated or caller environment was changed")
				}
			}
		})
	}
}

func TestNHA119ObservationServeRejectsUnknownModesAndAdditionalArguments(t *testing.T) {
	_, credentials, _ := packageFixture(t)
	if err := os.WriteFile(filepath.Join(credentials, "password"), []byte(strings.Repeat("c", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "unknown", "serve-observe", "serve-observe-mqtt-readonly=false", "serve -observe-mqtt-readonly"} {
		if argv, env, err := serveInvocation(mode, credentials, nil); err == nil || argv != nil || env != nil {
			t.Fatal("unknown startup mode admitted")
		}
	}
	// These are rejected before credential access or syscall.Exec. An operator
	// cannot append a second flag to disable the guarded command's startup mode.
	for _, args := range [][]string{{"serve-observe-mqtt-readonly", "-observe-mqtt-readonly=false"}, {"serve", "-observe-mqtt-readonly"}, {"serve-observe"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatal("unknown command or additional startup option admitted")
		}
	}
}

func TestNHA119ObservationServeRejectsLostOrInvalidPrivateCredential(t *testing.T) {
	for _, mode := range []string{"serve", "serve-observe-mqtt-readonly"} {
		t.Run(mode, func(t *testing.T) {
			_, credentials, _ := packageFixture(t)
			for _, value := range []string{"", "short", strings.Repeat("d", 63), strings.Repeat("e", 65)} {
				if value != "" {
					if err := os.WriteFile(filepath.Join(credentials, "password"), []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if argv, env, err := serveInvocation(mode, credentials, nil); err == nil || argv != nil || env != nil {
					t.Fatal("missing or invalid private credential admitted")
				}
			}
			if err := os.WriteFile(filepath.Join(credentials, "password"), []byte(strings.Repeat("f", 64)), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(credentials, "password"), 0644); err != nil {
				t.Fatal(err)
			}
			if argv, env, err := serveInvocation(mode, credentials, nil); err == nil || argv != nil || env != nil {
				t.Fatal("nonprivate credential admitted")
			}
		})
	}
}
