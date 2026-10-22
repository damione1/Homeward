package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/installation"
	"github.com/damione1/Homeward/internal/backups/local"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

// This local operator path borrows the established database/key pair. It does
// not run migrations, reconcile configuration, start pilots or open a listener.
func runInstallationBackup(ctx context.Context, args []string, out, diagnostics io.Writer) (code int) {
	publicationStarted, published, completed := false, false, false
	defer func() {
		if recover() != nil {
			code = 1
			if publicationStarted {
				code = 3
			}
			if completed {
				code = 4
			}
			installationBackupDiagnostic(diagnostics, "installation: backup interrupted; inspect the prepared artifact before retrying")
		}
	}()
	flags := flag.NewFlagSet("installation backup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bootstrap := flags.String("bootstrap", "", "existing launch bootstrap YAML")
	instance := flags.String("instance", "local", "enabled local backup instance")
	pgBin := flags.String("pg-bin-dir", "", "trusted native PostgreSQL tools directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	id, err := backups.ParseInstanceID(*instance)
	if err != nil {
		return 2
	}
	tools, err := storage.ParseBackupPGTools(*pgBin)
	if err != nil {
		return 2
	}
	raw, err := readInstallationFile(*bootstrap, 1<<20)
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	defer clear(raw)
	launch, err := config.ParseBootstrapLaunchWithEnv(raw, os.LookupEnv)
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	configuration, err := launch.BackupProviders()
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	var selected backups.InstanceSettings
	for _, candidate := range configuration.Instances() {
		if candidate.ID() == id && candidate.Enabled() && candidate.Kind() == backups.Local {
			selected = candidate
		}
	}
	public, present, err := launch.BackupRecovery()
	if !selected.Valid() || err != nil || !present {
		return installationBackupFailure(diagnostics)
	}
	settings, err := launch.PostgresSettings()
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	defer store.Close()
	if err = store.ValidateBackupSchema(ctx); err != nil {
		return installationBackupFailure(diagnostics)
	}
	data, err := launch.DataDir()
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	secrets, err := store.BootstrapSecretsForStartup(ctx, data, storage.RequireEstablishedSecretStartup())
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	service, err := installation.NewService(store, secrets, public, filepath.Join(data, "backup-archive-pending"))
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	provider, err := (local.Factory{}).Open(ctx, selected)
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	defer provider.Close()
	artifact, err := backups.NewArtifactID()
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	sealed, err := service.Create(ctx, tools, artifact)
	if err != nil {
		return installationBackupFailure(diagnostics)
	}
	// Keep the exact ciphertext on an ambiguous publication. A subsequent
	// command generates a new identity and never reseals under this identity.
	defer func() {
		if !publicationStarted || published {
			_ = sealed.Close()
		}
	}()
	if _, err = fmt.Fprintf(out, "backup prepared; artifact id %s; instance %s\n", artifact.String(), id.String()); err != nil {
		return installationBackupFailure(diagnostics)
	}
	publicationStarted = true
	result, err := sealed.Publish(ctx, provider)
	if result.State() == backups.NotPublished {
		publicationStarted = false
	}
	if err != nil || (result.State() != backups.Published && result.State() != backups.AlreadyPresent) {
		if publicationStarted {
			installationBackupDiagnostic(diagnostics, "installation: backup publication uncertain; preserve the artifact and pending ciphertext before retrying")
			return 3
		}
		return installationBackupFailure(diagnostics)
	}
	published = true
	verified, err := provider.Verify(ctx, artifact)
	if err != nil || !verified.Valid() || verified.Metadata().Size() != sealed.Size() || verified.Metadata().SHA256() != sealed.SHA256() {
		installationBackupDiagnostic(diagnostics, "installation: published backup verification unavailable; preserve the artifact")
		return 3
	}
	completed = true
	if _, err = fmt.Fprintln(out, "backup verified; encrypted local archive; no runtime started"); err != nil {
		installationBackupDiagnostic(diagnostics, "installation: backup completed but output failed; preserve the prepared artifact")
		return 4
	}
	return 0
}

func installationBackupFailure(diagnostics io.Writer) int {
	installationBackupDiagnostic(diagnostics, "installation: backup unavailable")
	return 1
}

func installationBackupDiagnostic(out io.Writer, message string) {
	defer func() { _ = recover() }()
	_, _ = fmt.Fprintln(out, message)
}

// A provider reader checks the complete opaque payload through EOF. Restore155
// authenticates its contents before any SQL import or directory publication.
func openInstallationArchive(ctx context.Context, archive, root, instance string, id backups.ArtifactID) (io.ReadCloser, error) {
	if (archive == "") == (root == "") {
		return nil, backups.NewError(backups.InvalidInput)
	}
	if archive != "" {
		return os.Open(archive)
	}
	instanceID, err := backups.ParseInstanceID(instance)
	if err != nil {
		return nil, err
	}
	settings, err := backups.NewLocalSettings(instanceID, true, root, backups.DefaultLimits())
	if err != nil {
		return nil, err
	}
	provider, err := (local.Factory{}).Open(ctx, settings)
	if err != nil {
		return nil, err
	}
	reader, err := provider.Open(ctx, id)
	if err != nil {
		_ = provider.Close()
		return nil, err
	}
	return &installationArchiveReader{ReadCloser: reader, provider: provider}, nil
}

type installationArchiveReader struct {
	io.ReadCloser
	provider backups.ManagedProvider
}

func (r *installationArchiveReader) Close() error {
	err := r.ReadCloser.Close()
	if providerErr := r.provider.Close(); err == nil {
		err = providerErr
	}
	return err
}
