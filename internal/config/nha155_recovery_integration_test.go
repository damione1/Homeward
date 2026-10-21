package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func nha155PublicFixture(t *testing.T) string {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("generate public configuration fixture")
	}
	return identity.Recipient().String()
}
func nha155Combined(t *testing.T, dataDir, public string) []byte {
	t.Helper()
	raw := nha029Combined(dataDir)
	if public != "" {
		raw = append(raw, []byte(fmt.Sprintf("backup_recovery_recipient: %q\n", public))...)
	}
	return raw
}
func TestNHA155RecoveryLaunchPublicAdmission(t *testing.T) {
	directory := t.TempDir()
	for _, public := range []string{"", nha155PublicFixture(t)} {
		configured, err := ParseWithEnv(nha155Combined(t, directory, public), nha029Lookup)
		if err != nil {
			t.Fatal("recovery launch configuration refused")
		}
		admitted, present, err := configured.BackupRecovery()
		if err != nil || present != (public != "") || admitted.Valid() != present {
			t.Fatal("absent and admitted recovery states conflated")
		}
		if present {
			text, err := admitted.PublicText()
			if err != nil || text != public {
				t.Fatal("public recipient changed during admission")
			}
		}
	}
	if _, _, err := (Config{}).BackupRecovery(); !errors.Is(err, ErrUninitialized) {
		t.Fatal("zero configuration acquired recovery authority")
	}
	for name, field := range map[string]string{
		"empty": "''", "null": "null", "bool": "true", "list": "[]", "map": "{}",
		"unknown": "'NHA155_PRIVATE_RECOVERY_CANARY'", "private": "'AGE-SECRET-KEY-NHA155_PRIVATE_RECOVERY_CANARY'",
	} {
		t.Run(name, func(t *testing.T) {
			raw := append(nha155Combined(t, directory, ""), []byte("backup_recovery_recipient: "+field+"\n")...)
			_, err := ParseWithEnv(raw, nha029Lookup)
			if err == nil {
				t.Fatal("invalid public recovery material admitted")
			}
			if strings.Contains(err.Error(), "CANARY") {
				t.Fatal("configuration error rendered private input")
			}
		})
	}
}

func TestNHA155RecoverySidecarIsRestartOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configuration.yaml")
	publicA, publicB := nha155PublicFixture(t), nha155PublicFixture(t)
	rawA := nha155Combined(t, filepath.Join(dir, "data"), publicA)
	if os.WriteFile(path, rawA, 0600) != nil {
		t.Fatal("write launch fixture")
	}
	startup, err := LoadStartupPaths(path, DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal("load launch fixture")
	}
	if err := startup.PublishDefaultBootstrap(); err != nil {
		t.Fatal("publish recovery sidecar")
	}
	sidecar, exists := startup.BootstrapPath()
	if !exists {
		t.Fatal("missing bootstrap path")
	}
	rawSidecar, err := os.ReadFile(sidecar)
	if err != nil || !strings.Contains(string(rawSidecar), publicA) || strings.Contains(string(rawSidecar), "NHA029_private_env_value") || strings.Contains(string(rawSidecar), "AGE-SECRET-KEY-") {
		t.Fatal("sidecar lost public recipient or exposed a private value")
	}
	rawB := nha155Combined(t, filepath.Join(dir, "data"), publicB)
	if os.WriteFile(path, rawB, 0600) != nil {
		t.Fatal("replace operational workspace fixture")
	}
	restarted, err := LoadStartupPaths(path, DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal("restart from existing public sidecar")
	}
	launch, err := restarted.LaunchConfig()
	if err != nil {
		t.Fatal("read restarted launch")
	}
	public, present, err := launch.BackupRecovery()
	if err != nil || !present {
		t.Fatal("sidecar lost recovery configuration")
	}
	text, _ := public.PublicText()
	if text != publicA {
		t.Fatal("workspace mutation changed restart-only public recovery")
	}
	operational, err := ParseOperationalWithEnv(rawB, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal("operational replay consulted launch credentials")
	}
	if _, _, err := operational.BackupRecovery(); !errors.Is(err, ErrUninitialized) {
		t.Fatal("operational replay acquired recovery authority")
	}
	if os.WriteFile(path, []byte("dirty: ["), 0600) != nil {
		t.Fatal("dirty workspace fixture")
	}
	if _, err := LoadStartupPaths(path, DefaultBootstrapChoice(), nha029Lookup); err != nil {
		t.Fatal("dirty workspace prevented launch through recovery sidecar")
	}
}
