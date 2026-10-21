package main

import (
	"bytes"
	"context"
	"fmt"
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
)

func nha035CLIBootstrap(port int, data string) []byte {
	return []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", data, port))
}
func TestNHA035HomedInstallationRestoreBeforeServerStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Setenv("HOMEWARD_TEST_PASSWORD", testpg.Password())
	sourceCluster := testpg.Start(t)
	settings, e := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: sourceCluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, os.LookupEnv)
	if e != nil {
		t.Fatal(e)
	}
	source, e := storage.Open(ctx, settings)
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	if _, e = source.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	data := filepath.Join(t.TempDir(), "source")
	startup, e := config.NewDetachedStartup(nha035CLIBootstrap(sourceCluster.Port, data), "configuration.yaml", nil, os.LookupEnv)
	if e != nil {
		t.Fatal(e)
	}
	admitted, _, e := configactivation.ReconcileStartup(ctx, source, startup)
	if e != nil {
		t.Fatal(e)
	}
	secrets, e := admitted.Secrets()
	if e != nil {
		t.Fatal(e)
	}
	operator := nha155PrivateRecoveryDir(t)
	privatePath := filepath.Join(operator, "retained.key")
	publicPath := filepath.Join(data, "recovery.pub")
	if _, e = backuparchive.ExportRecovery(privatePath, publicPath, data); e != nil {
		t.Fatal(e)
	}
	public, e := backuparchive.LoadRecoveryPublic(publicPath)
	if e != nil {
		t.Fatal(e)
	}
	service, e := installation.NewService(source, secrets, public, filepath.Join(data, "archive-pending"))
	if e != nil {
		t.Fatal(e)
	}
	bin, e := exec.LookPath("pg_dump")
	if e != nil {
		t.Fatal(e)
	}
	bin, e = filepath.Abs(bin)
	if e != nil {
		t.Fatal(e)
	}
	tools, e := storage.ParseBackupPGTools(filepath.Dir(bin))
	if e != nil {
		t.Fatal(e)
	}
	id, _ := backups.NewArtifactID()
	sealed, e := service.Create(ctx, tools, id)
	if e != nil {
		t.Fatal(e)
	}
	defer sealed.Close()
	archivePath := filepath.Join(operator, "installation.age")
	file, e := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = sealed.WriteTo(file); e != nil {
		t.Fatal(e)
	}
	if e = file.Close(); e != nil {
		t.Fatal(e)
	}
	target := testpg.Start(t)
	parent := nha155PrivateRecoveryDir(t)
	destination := filepath.Join(parent, "restored")
	targetFile := filepath.Join(operator, "target.yaml")
	if e = os.WriteFile(targetFile, nha035CLIBootstrap(target.Port, destination), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"installation", "restore", "--archive", archivePath, "--artifact-id", id.String(), "--private-file", privatePath, "--output", destination, "--target-bootstrap", targetFile, "--pg-bin-dir", filepath.Dir(bin)}
	var out, diagnostics bytes.Buffer
	// Invalid server dependencies are deliberate: actual installation dispatch
	// must happen before config admission, listener construction, and Run.
	code := runHomed(ctx, args, &out, &diagnostics, homedDependencies{})
	if code != 0 || !strings.Contains(out.String(), "server remains stopped") || diagnostics.Len() != 0 {
		t.Fatalf("offline installation dispatch code=%d output=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	if _, e = os.Stat(filepath.Join(destination, "installation-restored.json")); e != nil {
		t.Fatal(e)
	}
	for _, stream := range []string{out.String(), diagnostics.String()} {
		if strings.Contains(stream, testpg.Password()) || strings.Contains(stream, "HOMEWARD_SECRETS_KEY_V1=") {
			t.Fatal("operator output leaked credentials")
		}
	}
	out.Reset()
	diagnostics.Reset()
	if code = runHomed(ctx, args, &out, &diagnostics, homedDependencies{}); code != 1 {
		t.Fatalf("retry overwrote restored target: %d", code)
	}
}
func TestNHA035InstallationCLIBoundsAndRedacts(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"restore", "--archive", "CANARY_PRIVATE_PATH", "--bogus"}, {"restore", "--artifact-id", "invalid"}} {
		var out, diagnostics bytes.Buffer
		code := runInstallation(context.Background(), args, &out, &diagnostics)
		if code != 2 {
			t.Fatalf("invalid CLI code=%d", code)
		}
		if strings.Contains(diagnostics.String(), "CANARY_PRIVATE_PATH") {
			t.Fatal("raw argument in diagnostic")
		}
	}
}
