package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/backups/installation"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha157LocalFixture struct {
	ctx                                                  context.Context
	store                                                *storage.Store
	cluster                                              testpg.Cluster
	data, operator, bootstrap, private, pgBin, recipient string
}

func nha157LocalSetup(t *testing.T) *nha157LocalFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	t.Setenv("HOMEWARD_TEST_PASSWORD", testpg.Password())
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "source")
	startup, err := config.NewDetachedStartup(nha035CLIBootstrap(cluster.Port, data), "configuration.yaml", nil, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = configactivation.ReconcileStartup(ctx, store, startup); err != nil {
		t.Fatal(err)
	}
	operator := nha155PrivateRecoveryDir(t)
	private, publicFile := filepath.Join(operator, "retained.key"), filepath.Join(data, "recovery.pub")
	if _, err = backuparchive.ExportRecovery(private, publicFile, data); err != nil {
		t.Fatal(err)
	}
	public, err := backuparchive.LoadRecoveryPublic(publicFile)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := public.PublicText()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(operator, "source.yaml")
	raw := append(nha035CLIBootstrap(cluster.Port, data), []byte(fmt.Sprintf("backup_recovery_recipient: %q\n", recipient))...)
	if err = os.WriteFile(bootstrap, raw, 0600); err != nil {
		t.Fatal(err)
	}
	bin, err := exec.LookPath("pg_dump")
	if err != nil {
		t.Fatal(err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	return &nha157LocalFixture{ctx, store, cluster, data, operator, bootstrap, private, filepath.Dir(bin), recipient}
}

func (f *nha157LocalFixture) backup(t *testing.T) backups.ArtifactID {
	t.Helper()
	var out, diagnostics bytes.Buffer
	// Invalid dependencies prove this is the actual pre-startup homed dispatch.
	code := runHomed(f.ctx, []string{"installation", "backup", "--bootstrap", f.bootstrap, "--pg-bin-dir", f.pgBin}, &out, &diagnostics, homedDependencies{})
	if code != 0 || diagnostics.Len() != 0 || !strings.Contains(out.String(), "backup verified;") {
		t.Fatalf("backup code=%d output=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	id := nha157ArtifactFromOutput(t, out.String())
	for _, stream := range []string{out.String(), diagnostics.String()} {
		if strings.Contains(stream, testpg.Password()) || strings.Contains(stream, "AGE-SECRET-KEY") || strings.Contains(stream, "HOMEWARD_SECRETS_KEY_V1") || strings.Contains(stream, f.data) {
			t.Fatal("operator output disclosed private material")
		}
	}
	return id
}

func nha157ArtifactFromOutput(t *testing.T, output string) backups.ArtifactID {
	t.Helper()
	line := strings.Split(output, "\n")[0]
	if !strings.HasSuffix(line, "; instance local") {
		t.Fatal("missing local identity")
	}
	idText := strings.TrimSuffix(strings.TrimPrefix(line, "backup prepared; artifact id "), "; instance local")
	id, err := backups.ParseArtifactID(idText)
	if err != nil {
		t.Fatal("missing admitted artifact id")
	}
	return id
}

func TestNHA157LocalOperatorBackupUsesVerifiedEncryptedProvider(t *testing.T) {
	f := nha157LocalSetup(t)
	before, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	id := f.backup(t)
	reader, err := openInstallationArchive(f.ctx, "", filepath.Join(f.data, "backups"), "local", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/v1\n")) || bytes.Contains(raw, []byte("HOMEWARD_SECRETS_KEY_V1")) || bytes.Contains(raw, []byte("postgres:")) {
		t.Fatal("provider did not retain only the encrypted age payload")
	}
	after, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present || before.Revision() != after.Revision() || before.ManifestSHA256() != after.ManifestSHA256() {
		t.Fatal("backup activated a configuration")
	}
}

func TestNHA157LocalOperatorRefusesMissingRecoveryDisabledAndForeignProvider(t *testing.T) {
	f := nha157LocalSetup(t)
	for _, tc := range []struct{ name, extra string }{
		{"missing-recipient", ""},
		{"disabled-local", fmt.Sprintf("backup_recovery_recipient: %q\nbackups:\n  instances: []\n", f.recipient)},
		{"wrong-instance", fmt.Sprintf("backup_recovery_recipient: %q\nbackups:\n  instances:\n    - id: another\n      provider: local\n", f.recipient)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(f.operator, "reject-"+tc.name+".yaml")
			raw := append(nha035CLIBootstrap(f.cluster.Port, f.data), []byte(tc.extra)...)
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out, diagnostics bytes.Buffer
			code := runHomed(f.ctx, []string{"installation", "backup", "--bootstrap", file, "--pg-bin-dir", f.pgBin}, &out, &diagnostics, homedDependencies{})
			if code != 1 || out.Len() != 0 || diagnostics.String() != "installation: backup unavailable\n" {
				t.Fatalf("rejection code=%d out=%q diagnostics=%q", code, out.String(), diagnostics.String())
			}
			if _, err := os.Stat(filepath.Join(f.data, "backups")); !os.IsNotExist(err) {
				t.Fatal("rejected backup opened a provider")
			}
		})
	}
}

func TestNHA157LocalOperatorRejectsLostServerKeyWithoutRegenerating(t *testing.T) {
	f := nha157LocalSetup(t)
	if err := os.Remove(filepath.Join(f.data, ".env")); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	code := runHomed(f.ctx, []string{"installation", "backup", "--bootstrap", f.bootstrap, "--pg-bin-dir", f.pgBin}, &out, &diagnostics, homedDependencies{})
	if code != 1 || out.Len() != 0 {
		t.Fatalf("lost-key backup code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(f.data, ".env")); !os.IsNotExist(err) {
		t.Fatal("backup regenerated the lost key")
	}
	if _, err := os.Stat(filepath.Join(f.data, "backups")); !os.IsNotExist(err) {
		t.Fatal("lost-key backup reached provider")
	}
}

func TestNHA157LocalOperatorRefusesIncompatibleSchemaWithoutMigration(t *testing.T) {
	f := nha157LocalSetup(t)
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET schema_version=schema_version+1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	code := runHomed(f.ctx, []string{"installation", "backup", "--bootstrap", f.bootstrap, "--pg-bin-dir", f.pgBin}, &out, &diagnostics, homedDependencies{})
	if code != 1 || out.Len() != 0 {
		t.Fatalf("incompatible backup code=%d", code)
	}
	var schema, migrations int
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT schema_version,(SELECT count(*) FROM public.homeward_schema_migrations) FROM public.homeward_schema_window`).Scan(&schema, &migrations)
	}); err != nil {
		t.Fatal(err)
	}
	if schema != int(storage.CurrentVersion().Number())+1 || migrations != int(storage.CurrentVersion().Number()) {
		t.Fatal("backup changed schema metadata")
	}
	if _, err := os.Stat(filepath.Join(f.data, "backups")); !os.IsNotExist(err) {
		t.Fatal("incompatible backup reached provider")
	}
}

func TestNHA157LocalSnapshotRechecksSchemaAfterSuccessfulPreflight(t *testing.T) {
	f := nha157LocalSetup(t)
	if err := f.store.ValidateBackupSchema(f.ctx); err != nil {
		t.Fatal(err)
	}
	secrets, err := f.store.BootstrapSecretsForStartup(f.ctx, f.data, storage.RequireEstablishedSecretStartup())
	if err != nil {
		t.Fatal(err)
	}
	public, err := backuparchive.ParseRecoveryPublic(f.recipient)
	if err != nil {
		t.Fatal(err)
	}
	service, err := installation.NewService(f.store, secrets, public, filepath.Join(f.data, "backup-archive-pending"))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := storage.ParseBackupPGTools(f.pgBin)
	if err != nil {
		t.Fatal(err)
	}
	// Exact cut between the preliminary admission and the real snapshot.
	// Without admission inside BeginBackupSnapshot, this invalid release
	// would be exported successfully despite its now-stale preliminary check.
	if err = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET schema_version=schema_version+1`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	id, _ := backups.NewArtifactID()
	sealed, err := service.Create(f.ctx, tools, id)
	if sealed != nil {
		_ = sealed.Close()
	}
	if err == nil || sealed != nil {
		t.Fatal("snapshot trusted stale schema preflight")
	}
}

type nha157SecondOutputFailure struct {
	bytes.Buffer
	calls      int
	panicWrite bool
}

func (w *nha157SecondOutputFailure) Write(raw []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return w.Buffer.Write(raw)
	}
	if w.panicWrite {
		panic("NHA157_OUTPUT_PRIVATE_CANARY")
	}
	return 0, io.ErrClosedPipe
}

func TestNHA157LocalOperatorPreservesVerifiedBackupWhenSuccessOutputFails(t *testing.T) {
	f := nha157LocalSetup(t)
	for _, panicWrite := range []bool{false, true} {
		name := "io-error"
		if panicWrite {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			out := &nha157SecondOutputFailure{panicWrite: panicWrite}
			var diagnostics bytes.Buffer
			code := runHomed(f.ctx, []string{"installation", "backup", "--bootstrap", f.bootstrap, "--pg-bin-dir", f.pgBin}, out, &diagnostics, homedDependencies{})
			if code != 4 || out.calls != 2 || strings.Contains(diagnostics.String(), "PRIVATE_CANARY") {
				t.Fatalf("durable backup output failure code=%d diagnostics=%q", code, diagnostics.String())
			}
			id := nha157ArtifactFromOutput(t, out.String())
			reader, err := openInstallationArchive(f.ctx, "", filepath.Join(f.data, "backups"), "local", id)
			if err != nil {
				t.Fatal("completed artifact missing", err)
			}
			_, readErr := io.Copy(io.Discard, reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal("completed artifact lost integrity", readErr, closeErr)
			}
		})
	}
}
