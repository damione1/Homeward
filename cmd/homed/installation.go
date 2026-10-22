package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/backups/installation"
	"github.com/damione1/Homeward/internal/storage"
)

// Installation restore is dispatched before startup admission. It never calls
// Run, opens a public listener, starts pilots, or contacts archived addresses.
func runInstallation(ctx context.Context, args []string, out, diagnostics io.Writer) (code int) {
	result := installation.RestoreResult{}
	mayHaveEffect := false
	defer func() {
		if recover() != nil {
			if mayHaveEffect || result.State() == installation.Restored || result.State() == installation.Indeterminate {
				code = 3
			} else {
				code = 1
			}
			if !homedMissingWriter(diagnostics) {
				_, _ = fmt.Fprintln(diagnostics, "installation: interrupted; inspect the isolated target before retrying")
			}
		}
	}()
	if ctx == nil || homedMissingWriter(out) || homedMissingWriter(diagnostics) || len(args) == 0 {
		return 2
	}
	if args[0] == "backup" {
		return runInstallationBackup(ctx, args[1:], out, diagnostics)
	}
	if args[0] != "restore" {
		_, _ = fmt.Fprintln(diagnostics, "installation: unsupported operation")
		return 2
	}
	flags := flag.NewFlagSet("installation restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archive := flags.String("archive", "", "exported encrypted archive")
	localRoot := flags.String("local-root", "", "local provider root, instead of an exported archive")
	instance := flags.String("instance", "local", "local provider instance")
	artifact := flags.String("artifact-id", "", "archive identity")
	key := flags.String("private-file", "", "independent recovery private key")
	output := flags.String("output", "", "new isolated data directory")
	bootstrap := flags.String("target-bootstrap", "", "explicit target launch YAML (empty isolated PostgreSQL)")
	pgBin := flags.String("pg-bin-dir", "", "trusted native PostgreSQL tools directory")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(diagnostics, "installation: invalid options")
		return 2
	}
	id, err := backups.ParseArtifactID(*artifact)
	if err != nil {
		return 2
	}
	tools, err := storage.ParseBackupPGTools(*pgBin)
	if err != nil {
		return 2
	}
	if !filepath.IsAbs(*output) || filepath.Clean(*output) != *output {
		return 2
	}
	private, err := backuparchive.LoadRecoveryPrivate(*key)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, "installation: recovery key unavailable")
		return 1
	}
	launch, err := readInstallationFile(*bootstrap, 1<<20)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, "installation: target configuration unavailable")
		return 1
	}
	file, err := openInstallationArchive(ctx, *archive, *localRoot, *instance, id)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, "installation: archive unavailable")
		return 1
	}
	defer file.Close()
	mayHaveEffect = true
	result, err = installation.Restore(ctx, file, id, private, filepath.Dir(*output), filepath.Base(*output), tools, launch, os.LookupEnv)
	if err != nil {
		if result.State() == installation.Indeterminate {
			_, _ = fmt.Fprintln(diagnostics, "installation: recovery is indeterminate; preserve the isolated database and pending directory; do not retry against that target")
			return 3
		}
		_, _ = fmt.Fprintln(diagnostics, "installation: recovery failed before database publication")
		return 1
	}
	if result.State() != installation.Restored {
		return 3
	}
	m := result.Measurement()
	if _, err = fmt.Fprintf(out, "installation restored; restored generation %d; recovery duration %s; snapshot age %s; server remains stopped\n", result.Generation().Number(), m.RestoreDuration, m.DataAge); err != nil {
		_, _ = fmt.Fprintln(diagnostics, "installation: restoration completed but output failed; inspect the target before retrying")
		return 4
	}
	return 0
}
func readInstallationFile(name string, limit int64) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		_ = file.Close()
		return nil, fmt.Errorf("invalid installation input")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || int64(len(raw)) != info.Size() {
		return nil, fmt.Errorf("invalid installation input")
	}
	return raw, nil
}
