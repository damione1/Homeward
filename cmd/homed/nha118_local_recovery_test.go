package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

func TestNHA118LocalOperatorRestoresDatabaseRegistrySourcesAndIndependentSecrets(t *testing.T) {
	f := nha157LocalSetup(t)
	secrets, err := f.store.BootstrapSecretsForStartup(f.ctx, f.data, storage.RequireEstablishedSecretStartup())
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha118-local-fixture")
	name, _ := storage.ParseSecretName("synthetic-token")
	handle, _ := storage.NewSecretHandle(instance, name)
	value, _ := storage.NewSecretValue([]byte("NHA118_SYNTHETIC_ONLY"))
	if err = secrets.RotateCredential(f.ctx, handle, value); err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(f.store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("nha118-synthetic", "boolean")
	binding, err := identities.ConfigureBinding(f.ctx, instance, key, "input_boolean.away_mode")
	if err != nil {
		t.Fatal(err)
	}
	active, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	originalKey, err := os.ReadFile(filepath.Join(f.data, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(originalKey)
	lastCommit := time.Now()
	id := f.backup(t)
	newer, _ := storage.NewSecretValue([]byte("NHA118_AFTER_SNAPSHOT_NOT_RESTORED"))
	if err = secrets.RotateCredential(f.ctx, handle, newer); err != nil {
		t.Fatal(err)
	}
	var databaseDir string
	if err = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error { return tx.QueryRow(f.ctx, `SHOW data_directory`).Scan(&databaseDir) }); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(databaseDir) != "data" || !strings.Contains(databaseDir, "TestNHA118LocalOperatorRestores") {
		t.Fatal("refuse cleanup outside this owned fixture")
	}
	failureAt := time.Now()
	f.store.Close()
	if err = f.cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(databaseDir); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(f.data, ".env")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{databaseDir, filepath.Join(f.data, ".env")} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("source database/key survived the loss")
		}
	}
	// The private recovery identity is retained independently of source DB/.env.
	// The local encrypted provider is the only source of restored installation data.
	target := testpg.Start(t)
	destination := filepath.Join(f.operator, "restored")
	targetFile := filepath.Join(f.operator, "target.yaml")
	if err = os.WriteFile(targetFile, nha035CLIBootstrap(target.Port, destination), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	started := time.Now()
	code := runHomed(f.ctx, []string{"installation", "restore", "--local-root", filepath.Join(f.data, "backups"), "--artifact-id", id.String(), "--private-file", f.private, "--output", destination, "--target-bootstrap", targetFile, "--pg-bin-dir", f.pgBin}, &out, &diagnostics, homedDependencies{})
	elapsed := time.Since(started)
	if code != 0 || diagnostics.Len() != 0 || !strings.Contains(out.String(), "server remains stopped") {
		t.Fatalf("restore code=%d out=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	fields := strings.Split(strings.TrimSpace(out.String()), ";")
	if len(fields) != 5 {
		t.Fatal("missing measured recovery output")
	}
	rto, err := time.ParseDuration(strings.TrimPrefix(strings.TrimSpace(fields[2]), "recovery duration "))
	if err != nil || rto < 0 || rto > 30*time.Minute {
		t.Fatal("invalid measured RTO")
	}
	dataAge, err := time.ParseDuration(strings.TrimPrefix(strings.TrimSpace(fields[3]), "snapshot age "))
	if err != nil || dataAge < 0 || dataAge > time.Hour {
		t.Fatal("invalid measured snapshot age")
	}
	lossWindow := failureAt.Sub(lastCommit)
	if lossWindow < 0 || lossWindow > time.Hour {
		t.Fatal("invalid measured operational RPO window")
	}
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: target.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := storage.Open(f.ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	recoveredSecrets, err := restored.BootstrapSecretsForStartup(f.ctx, destination, storage.RequireEstablishedSecretStartup())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredSecrets.Read(f.ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := recovered.Bytes()
	if err != nil || string(raw) != "NHA118_SYNTHETIC_ONLY" {
		t.Fatal("restored key cannot decrypt the saved credential")
	}
	clear(raw)
	restoredKey, err := os.ReadFile(filepath.Join(destination, ".env"))
	if err != nil || !bytes.Equal(originalKey, restoredKey) {
		t.Fatal("server key changed")
	}
	clear(restoredKey)
	after, present, err := restored.ReadActiveConfig(f.ctx)
	if err != nil || !present || after.Revision().Number() <= active.Revision().Number() || len(after.Versions()) != len(active.Versions()) {
		t.Fatal("restored configuration was not fenced into a new generation")
	}
	for i, v := range active.Versions() {
		if after.Versions()[i].SHA256() != v.SHA256() {
			t.Fatal("saved source changed")
		}
	}
	entity, _ := binding.Entity.Value()
	var registryRows int
	if err = restored.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT count(*) FROM public.registry_entities WHERE id=$1`, entity).Scan(&registryRows)
	}); err != nil || registryRows != 1 {
		t.Fatal("stable identity missing", err)
	}
	if _, err = os.Stat(filepath.Join(destination, "configuration.yaml")); err != nil {
		t.Fatal("bundle not restored", err)
	}
	t.Logf("NHA118 MEASURED_SYNTHETIC artifact=%s operational_loss_window=%s snapshot_age=%s restore_duration=%s cli_elapsed=%s; historical gap after snapshot is not replayed", id.String(), lossWindow, dataAge, rto, elapsed)
	if strings.Contains(out.String(), "NHA118_SYNTHETIC_ONLY") || strings.Contains(out.String(), fmt.Sprint(testpg.Password())) {
		t.Fatal("recovery disclosed a secret")
	}
}
