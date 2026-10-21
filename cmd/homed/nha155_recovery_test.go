package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/storage"
)

func nha155PrivateRecoveryDir(t *testing.T) string {
	t.Helper()
	// testing.TempDir creates its numbered leaf with mode0777 before umask;
	// recovery exports deliberately require an existing owner-private0700 root.
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal("create private recovery fixture directory")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("recovery fixture directory is not private")
	}
	return directory
}

func TestNHA155OfflineRecoveryOperatorCommands(t *testing.T) {
	operator, server := nha155PrivateRecoveryDir(t), nha155PrivateRecoveryDir(t)
	private, public := filepath.Join(operator, "recovery.key"), filepath.Join(server, "recovery.pub")
	var out, diagnostic bytes.Buffer
	commands := [][]string{
		{"export", "--private-output", private, "--recipient-output", public, "--server-data-dir", server},
		{"verify", "--private-file", private, "--recipient-file", public, "--server-data-dir", server},
		{"copy", "--private-file", private, "--private-output", filepath.Join(operator, "copy.key"), "--server-data-dir", server},
	}
	for _, command := range commands {
		if code := runRecovery(context.Background(), command, &out, &diagnostic); code != 0 {
			t.Fatal("offline command failed")
		}
	}
	key, _ := os.ReadFile(private)
	if strings.Contains(out.String()+diagnostic.String(), strings.TrimSpace(string(key))) || strings.Contains(out.String()+diagnostic.String(), "AGE-SECRET-KEY-") {
		t.Fatal("operator output leaked recovery")
	}
	for _, command := range [][]string{{"verify", "--private-file", filepath.Join(operator, "missing.key"), "--recipient-file", public, "--server-data-dir", server}, {"verify", "--private-file", "NHA155_PRIVATE_ARGUMENT_CANARY"}, {"unknown", "NHA155_PRIVATE_ARGUMENT_CANARY"}} {
		out.Reset()
		diagnostic.Reset()
		if code := runRecovery(context.Background(), command, &out, &diagnostic); code == 0 {
			t.Fatal("invalid offline command succeeded")
		}
		if strings.Contains(out.String()+diagnostic.String(), "CANARY") {
			t.Fatal("argument appeared in diagnostics")
		}
	}
}

func TestNHA155OfflineRestorePreservesPublishedIndeterminateOutcome(t *testing.T) {
	operator, serverDir := nha155PrivateRecoveryDir(t), nha155PrivateRecoveryDir(t)
	privateFile, publicFile := filepath.Join(operator, "recovery.key"), filepath.Join(serverDir, "recovery.pub")
	if _, err := backuparchive.ExportRecovery(privateFile, publicFile, serverDir); err != nil {
		t.Fatal("export recovery fixture")
	}
	public, err := backuparchive.LoadRecoveryPublic(publicFile)
	if err != nil {
		t.Fatal("load public fixture")
	}
	keyRaw := []byte("HOMEWARD_SECRETS_KEY_V1=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32)) + "\n")
	key, err := storage.ParseBackupServerKey(keyRaw)
	if err != nil {
		t.Fatal("admit server key fixture")
	}
	source, _ := backuparchive.NewYAMLSource("configuration.yaml", 4, strings.NewReader("yaml"))
	input, err := backuparchive.NewInput(strings.NewReader("database"), 8, []backuparchive.YAMLSource{source}, key)
	if err != nil {
		t.Fatal("prepare fixture archive")
	}
	id, _ := backups.ParseArtifactID("15500000000000000000000000000004")
	image, err := backuparchive.Seal(context.Background(), filepath.Join(t.TempDir(), "cipher"), id, public, input, backuparchive.DefaultLimits())
	if err != nil {
		t.Fatal("seal fixture archive")
	}
	defer image.Close()
	archivePath := filepath.Join(operator, "archive.age")
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("create raw archive export")
	}
	_, writeErr := image.WriteTo(file)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("export raw archive")
	}
	parent := filepath.Join(t.TempDir(), "parent")
	destination := filepath.Join(parent, "restored")
	published := false
	restore := func(ctx context.Context, source io.Reader, artifact backups.ArtifactID, identity backuparchive.RecoveryPrivate, parent, name string, limits backuparchive.Limits) (backuparchive.RestoreState, error) {
		result, err := backuparchive.Recover(ctx, source, artifact, identity, parent, name, limits)
		if err != nil {
			return result.State(), err
		}
		published = true
		// The native operation really published complete files before this
		// boundary reports an IO failure. Core tests inject at rename/fsync.
		return backuparchive.RestoreIndeterminate, syscall.EIO
	}
	var out, diagnostics bytes.Buffer
	args := []string{"restore", "--archive", archivePath, "--artifact-id", id.String(), "--private-file", privateFile, "--output", destination}
	code := runRecoveryWithRestore(context.Background(), args, &out, &diagnostics, restore)
	if code != 3 || !published || out.Len() != 0 || !strings.Contains(diagnostics.String(), "recovery indeterminate") || strings.Contains(diagnostics.String(), "recovery failed") {
		t.Fatal("operator lost the ambiguous publication outcome")
	}
	private, _ := os.ReadFile(privateFile)
	for _, forbidden := range []string{strings.TrimSpace(string(private)), privateFile, archivePath, destination} {
		if strings.Contains(diagnostics.String(), forbidden) {
			t.Fatal("indeterminate diagnostic leaked private input")
		}
	}
	for _, name := range []string{".env", "database.dump", "yaml/configuration.yaml"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Fatal("CLI removed an ambiguously published complete destination")
		}
	}
	raw, err := os.ReadFile(filepath.Join(destination, ".env"))
	if err != nil || !bytes.Equal(raw, keyRaw) {
		t.Fatal("CLI publication was partial")
	}
	out.Reset()
	diagnostics.Reset()
	if code := runRecovery(context.Background(), args, &out, &diagnostics); code == 0 {
		t.Fatal("retry overwrote the ambiguous destination")
	}
	raw, err = os.ReadFile(filepath.Join(destination, "database.dump"))
	if err != nil || string(raw) != "database" {
		t.Fatal("retry changed the complete destination")
	}
}

type nha155OutputFailure struct {
	panicWrite bool
	message    string
	calls      int
}

func (w *nha155OutputFailure) Write([]byte) (int, error) {
	w.calls++
	err := fmt.Errorf("%s: %w", w.message, syscall.EIO)
	if w.panicWrite {
		panic(err)
	}
	return 0, err
}

type nha155CLIArchive struct {
	args                                  []string
	privateFile, archivePath, destination string
	key                                   []byte
}

func nha155CLIArchiveFixture(t *testing.T) nha155CLIArchive {
	t.Helper()
	operator, serverDir := nha155PrivateRecoveryDir(t), nha155PrivateRecoveryDir(t)
	privateFile, publicFile := filepath.Join(operator, "recovery.key"), filepath.Join(serverDir, "recovery.pub")
	if _, err := backuparchive.ExportRecovery(privateFile, publicFile, serverDir); err != nil {
		t.Fatal("export output-failure fixture recovery")
	}
	public, err := backuparchive.LoadRecoveryPublic(publicFile)
	if err != nil {
		t.Fatal("load output-failure fixture public")
	}
	keyRaw := []byte("HOMEWARD_SECRETS_KEY_V1=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32)) + "\n")
	key, err := storage.ParseBackupServerKey(keyRaw)
	if err != nil {
		t.Fatal("admit output-failure fixture server key")
	}
	source, _ := backuparchive.NewYAMLSource("configuration.yaml", 4, strings.NewReader("yaml"))
	input, err := backuparchive.NewInput(strings.NewReader("database"), 8, []backuparchive.YAMLSource{source}, key)
	if err != nil {
		t.Fatal("prepare output-failure fixture archive")
	}
	id, _ := backups.ParseArtifactID("15500000000000000000000000000005")
	image, err := backuparchive.Seal(context.Background(), filepath.Join(t.TempDir(), "cipher"), id, public, input, backuparchive.DefaultLimits())
	if err != nil {
		t.Fatal("seal output-failure fixture")
	}
	t.Cleanup(func() { _ = image.Close() })
	archivePath := filepath.Join(operator, "archive.age")
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("create output-failure raw archive")
	}
	_, writeErr := image.WriteTo(file)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("export output-failure raw archive")
	}
	destination := filepath.Join(t.TempDir(), "parent", "restored")
	return nha155CLIArchive{args: []string{"restore", "--archive", archivePath, "--artifact-id", id.String(), "--private-file", privateFile, "--output", destination}, privateFile: privateFile, archivePath: archivePath, destination: destination, key: keyRaw}
}
func TestNHA155OfflineRestoreKeepsDurableOutcomeWhenOutputFails(t *testing.T) {
	for _, mode := range []string{"EIO", "panic"} {
		t.Run(mode, func(t *testing.T) {
			fixture := nha155CLIArchiveFixture(t)
			private, err := os.ReadFile(fixture.privateFile)
			if err != nil {
				t.Fatal("read private fixture for redaction oracle")
			}
			out := &nha155OutputFailure{panicWrite: mode == "panic", message: "NHA155_PRIVATE_OUTPUT_CANARY " + strings.TrimSpace(string(private)) + " " + fixture.destination}
			var diagnostics bytes.Buffer
			// Use the native Recover path, including exclusive publication and
			// parent fsync. Only the later stdout writer is faulty.
			code := runRecovery(context.Background(), fixture.args, out, &diagnostics)
			if code != 4 || out.calls != 1 || !strings.Contains(diagnostics.String(), "recovery completed durably") || !strings.Contains(diagnostics.String(), "preserve the complete destination") || !strings.Contains(diagnostics.String(), "output unavailable") {
				t.Fatal("stdout failure erased the durable restoration outcome")
			}
			if strings.Contains(diagnostics.String(), "indeterminate") || strings.Contains(diagnostics.String(), "recovery failed") {
				t.Fatal("stdout failure reclassified a durable restoration")
			}
			for _, value := range []string{"NHA155_PRIVATE_OUTPUT_CANARY", strings.TrimSpace(string(private)), fixture.privateFile, fixture.archivePath, fixture.destination} {
				if strings.Contains(diagnostics.String(), value) {
					t.Fatal("completed diagnostic leaked a private value or path")
				}
			}
			for _, name := range []string{".env", "database.dump", "yaml/configuration.yaml"} {
				if _, err := os.Stat(filepath.Join(fixture.destination, name)); err != nil {
					t.Fatal("stdout failure removed a complete restoration")
				}
			}
			key, err := os.ReadFile(filepath.Join(fixture.destination, ".env"))
			if err != nil || !bytes.Equal(key, fixture.key) {
				t.Fatal("durable restoration did not preserve the server key")
			}
			var retryOut, retryDiagnostics bytes.Buffer
			if code := runRecovery(context.Background(), fixture.args, &retryOut, &retryDiagnostics); code != 1 {
				t.Fatal("retry did not refuse the completed destination")
			}
			got, err := os.ReadFile(filepath.Join(fixture.destination, "database.dump"))
			if err != nil || string(got) != "database" {
				t.Fatal("retry changed a completed restoration")
			}
		})
	}
}

func TestNHA155OfflineExportCopyKeepCompletedEffectWhenOutputFails(t *testing.T) {
	for _, operation := range []string{"export", "copy"} {
		for _, mode := range []string{"EIO", "panic"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				operator, serverDir := nha155PrivateRecoveryDir(t), nha155PrivateRecoveryDir(t)
				privateFile, publicFile := filepath.Join(operator, "exported.key"), filepath.Join(serverDir, "recovery.pub")
				args := []string{"export", "--private-output", privateFile, "--recipient-output", publicFile, "--server-data-dir", serverDir}
				if operation == "copy" {
					source := filepath.Join(operator, "original.key")
					if _, err := backuparchive.ExportRecovery(source, publicFile, serverDir); err != nil {
						t.Fatal("prepare output-failure copy source")
					}
					args = []string{"copy", "--private-file", source, "--private-output", privateFile, "--server-data-dir", serverDir}
				}
				out := &nha155OutputFailure{panicWrite: mode == "panic", message: "NHA155_PRIVATE_OUTPUT_CANARY " + privateFile}
				var diagnostics bytes.Buffer
				if code := runRecovery(context.Background(), args, out, &diagnostics); code != 4 || out.calls != 1 || !strings.Contains(diagnostics.String(), "recovery export completed") || !strings.Contains(diagnostics.String(), "preserve the exported files") {
					t.Fatal("stdout failure erased a completed export/copy")
				}
				if strings.Contains(diagnostics.String(), "recovery failed") || strings.Contains(diagnostics.String(), "indeterminate") {
					t.Fatal("completed export/copy was reclassified")
				}
				private, err := os.ReadFile(privateFile)
				if err != nil {
					t.Fatal("completed export/copy was removed")
				}
				for _, value := range []string{"NHA155_PRIVATE_OUTPUT_CANARY", strings.TrimSpace(string(private)), privateFile, publicFile} {
					if strings.Contains(diagnostics.String(), value) {
						t.Fatal("completed export diagnostic leaked input")
					}
				}
				if _, err := backuparchive.VerifyRetainedRecovery(privateFile, publicFile, serverDir); err != nil {
					t.Fatal("completed export/copy did not preserve a valid recovery")
				}
				var retryOut, retryDiagnostics bytes.Buffer
				if code := runRecovery(context.Background(), args, &retryOut, &retryDiagnostics); code != 1 {
					t.Fatal("retry overwrote completed export/copy")
				}
				after, err := os.ReadFile(privateFile)
				if err != nil || !bytes.Equal(after, private) {
					t.Fatal("retry changed completed export/copy")
				}
			})
		}
	}
}
