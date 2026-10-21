package archive

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func fixtureSettings(t *testing.T, cluster testpg.Cluster) storage.Settings {
	t.Helper()
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "NHA155_FIXTURE_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "NHA155_FIXTURE_PASSWORD" })
	if err != nil {
		t.Fatal("fixture database settings")
	}
	return settings
}
func pgFixtureCommand(ctx context.Context, command string, cluster testpg.Cluster, args ...string) *exec.Cmd {
	flags := []string{"--host=127.0.0.1", "--port=" + strconv.Itoa(int(cluster.Port)), "--username=homeward_test", "--dbname=postgres"}
	cmd := exec.CommandContext(ctx, command, append(flags, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PGPASSWORD=" + testpg.Password(), "PGPASSFILE=/dev/null", "PGCONNECT_TIMEOUT=5", "PGSSLMODE=disable", "LC_ALL=C"}
	return cmd
}

// This destroys the actual source fixture database files and key directory,
// then restores a real PostgreSQL dump to a different disposable cluster. It
// deliberately never starts a Homeward server, Host, scheduler or driver.
func TestNHA155RecoveryAfterDatabaseAndEnvLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sourceCluster := testpg.Start(t)
	source, err := storage.Open(ctx, fixtureSettings(t, sourceCluster))
	if err != nil {
		t.Fatal("open source fixture")
	}
	t.Cleanup(source.Close)
	if _, err := source.Migrate(ctx); err != nil {
		t.Fatal("migrate source fixture")
	}
	serverDir := filepath.Join(t.TempDir(), "server-data")
	secrets, err := source.BootstrapSecrets(ctx, serverDir)
	if err != nil {
		t.Fatal("bootstrap source fixture")
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha155-fixture")
	name, _ := storage.ParseSecretName("test-credential")
	handle, _ := storage.NewSecretHandle(instance, name)
	const canary = "NHA155_DATABASE_RECOVERY_FAKE_CREDENTIAL"
	value, _ := storage.NewSecretValue([]byte(canary))
	if err := secrets.RotateCredential(ctx, handle, value); err != nil {
		t.Fatal("store fixture credential")
	}
	var sourceDatabaseDirectory string
	_, err = source.InTransactionOutcome(ctx, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "SHOW data_directory").Scan(&sourceDatabaseDirectory) })
	if err != nil || filepath.Base(sourceDatabaseDirectory) != "data" || !strings.Contains(sourceDatabaseDirectory, "TestNHA155RecoveryAfterDatabaseAndEnvLoss") {
		t.Fatal("source fixture directory not safely identified")
	}
	operator := privateRecoveryDirFixture(t)
	privateFile, publicFile := filepath.Join(operator, "retained.key"), filepath.Join(serverDir, "recovery.pub")
	if _, err := ExportRecovery(privateFile, publicFile, serverDir); err != nil {
		t.Fatal("export independent fixture recovery")
	}
	public, err := LoadRecoveryPublic(publicFile)
	if err != nil {
		t.Fatal("load public fixture recovery")
	}
	dump := pgFixtureCommand(ctx, "pg_dump", sourceCluster, "--format=custom", "--no-owner", "--no-privileges")
	var dumpBytes, dumpDiagnostics bytes.Buffer
	dump.Stdout, dump.Stderr = &dumpBytes, &dumpDiagnostics
	if err := dump.Run(); err != nil {
		t.Fatal("real pg_dump fixture failed")
	}
	yaml := []byte("input_boolean:\n  recovered_helper:\n    initial: true\n")
	yamlSource, _ := NewYAMLSource("configuration.yaml", int64(len(yaml)), bytes.NewReader(yaml))
	key, err := secrets.BackupKey()
	if err != nil {
		t.Fatal("export admitted source key")
	}
	input, err := NewInput(bytes.NewReader(dumpBytes.Bytes()), int64(dumpBytes.Len()), []YAMLSource{yamlSource}, key)
	if err != nil {
		t.Fatal("prepare fixture archive")
	}
	image, err := Seal(ctx, filepath.Join(t.TempDir(), "cipher"), mustArtifactID(t), public, input, DefaultLimits())
	if err != nil {
		t.Fatal("seal real fixture database")
	}
	provider := &memoryProvider{}
	if _, err := image.Publish(ctx, provider); err != nil {
		_ = image.Close()
		t.Fatal("publish real fixture archive")
	}
	if err := image.Close(); err != nil {
		t.Fatal("close temporary cipher image")
	}
	input, key = Input{}, storage.BackupServerKey{}
	clear(dumpBytes.Bytes())
	secrets = nil
	source.Close()
	if err := sourceCluster.Stop(); err != nil {
		t.Fatal("stop lost source fixture database")
	}
	if err := os.RemoveAll(sourceDatabaseDirectory); err != nil {
		t.Fatal("destroy source fixture database")
	}
	if err := os.RemoveAll(serverDir); err != nil {
		t.Fatal("destroy source fixture .env")
	}
	for _, lost := range []string{sourceDatabaseDirectory, serverDir} {
		if _, err := os.Lstat(lost); !os.IsNotExist(err) {
			t.Fatal("source fixture was not lost")
		}
	}

	// No old key, old .env, original SQL connection or startup input is supplied.
	private, err := LoadRecoveryPrivate(privateFile)
	if err != nil {
		t.Fatal("read independently retained recovery")
	}
	recoveredParent := filepath.Join(t.TempDir(), "recovered")
	result, err := RecoverProvider(ctx, provider, mustArtifactID(t), private, recoveredParent, "installation", DefaultLimits())
	if err != nil || result.State() != Restored {
		t.Fatal("recover after source DB and .env loss")
	}
	recovered := filepath.Join(recoveredParent, "installation")
	targetCluster := testpg.Start(t)
	restore := pgFixtureCommand(ctx, "pg_restore", targetCluster, "--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges", filepath.Join(recovered, "database.dump"))
	var restoreDiagnostics bytes.Buffer
	restore.Stdout, restore.Stderr = &restoreDiagnostics, &restoreDiagnostics
	if err := restore.Run(); err != nil {
		t.Fatal("real pg_restore fixture failed")
	}
	target, err := storage.Open(ctx, fixtureSettings(t, targetCluster))
	if err != nil {
		t.Fatal("open restored fixture database")
	}
	defer target.Close()
	restoredSecrets, err := target.BootstrapSecrets(ctx, recovered)
	if err != nil {
		t.Fatal("restored key does not match restored database")
	}
	got, err := restoredSecrets.Read(ctx, handle)
	if err != nil {
		t.Fatal("read restored fixture credential")
	}
	plain, err := got.Bytes()
	defer clear(plain)
	if err != nil || string(plain) != canary {
		t.Fatal("fixture credential not recovered")
	}
	for _, stream := range [][]byte{provider.payload, dumpDiagnostics.Bytes(), restoreDiagnostics.Bytes()} {
		if bytes.Contains(stream, []byte(canary)) || bytes.Contains(stream, []byte(private.identity.String())) {
			t.Fatal("recovery leaked a fixture secret")
		}
	}
}
