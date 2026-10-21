package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/storage"
)

// Restore performs an explicit offline import into an empty isolated database.
// The independently supplied target bootstrap selects that database and final
// data directory. Archived launch addresses are never used as a target.
func Restore(ctx context.Context, source io.Reader, id backups.ArtifactID, private backuparchive.RecoveryPrivate, parent, destination string, tools storage.BackupPGTools, targetBootstrap []byte, lookup config.EnvLookup) (RestoreResult, error) {
	return restoreWithIO(ctx, source, id, private, parent, destination, tools, targetBootstrap, lookup, restoreIO{renameExclusive, syncDirectory})
}

type restoreIO struct {
	rename func(*os.Root, string, string) error
	sync   func(*os.Root, string) error
}

func restoreWithIO(ctx context.Context, source io.Reader, id backups.ArtifactID, private backuparchive.RecoveryPrivate, parent, destination string, tools storage.BackupPGTools, targetBootstrap []byte, lookup config.EnvLookup, operations restoreIO) (result RestoreResult, returned error) {
	result.state = NotPublished
	intervention := time.Now()
	if !contextValid(ctx) || backups.IsNil(source) || !id.Valid() || !private.Valid() || !tools.Valid() || !safeDestination(destination) || !filepath.IsAbs(parent) || operations.rename == nil || operations.sync == nil {
		return result, failure(Invalid)
	}
	launch, err := config.ParseBootstrapLaunchWithEnv(targetBootstrap, lookup)
	if err != nil {
		return result, failure(Invalid)
	}
	final := filepath.Join(parent, destination)
	dataDir, err := launch.DataDir()
	if err != nil || dataDir != final {
		return result, failure(Invalid)
	}
	root, err := storage.OpenPrivateDataDir(parent)
	if err != nil {
		return result, failure(Unavailable)
	}
	defer root.Close()
	if _, err = root.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return result, failure(Conflict)
	}
	stage, err := privateName("installation-pending-")
	if err != nil {
		return result, err
	}
	// Recover155 authenticates all bytes before publishing this private stage.
	extracted, err := backuparchive.Recover(ctx, source, id, private, parent, stage, backuparchive.DefaultLimits())
	if extracted.State() == backuparchive.RestoreIndeterminate {
		result.state = Indeterminate
		result.staged = filepath.Join(parent, stage)
		return result, failure(Unavailable)
	}
	if err != nil || extracted.State() != backuparchive.Restored {
		return result, failure(Integrity)
	}
	defer func() {
		if result.state != Indeterminate {
			_ = root.RemoveAll(stage)
		} else if result.staged == "" {
			result.staged = filepath.Join(parent, stage)
			if _, e := root.Lstat(stage); errors.Is(e, os.ErrNotExist) {
				if _, e = root.Lstat(destination); e == nil {
					result.staged = final
				}
			}
		}
	}()
	output, err := root.OpenRoot(stage)
	if err != nil {
		return result, failure(Unavailable)
	}
	defer output.Close()
	raw, err := readBounded(output, "yaml/"+manifestPath, maxManifest)
	if err != nil {
		return result, err
	}
	m, err := decodeManifest(raw)
	if err != nil {
		return result, err
	}
	if m.CutUTC.After(intervention) {
		return result, failure(Integrity)
	}
	for _, witness := range m.Sources {
		raw, err := readBounded(output, "yaml/"+witness.Archive, 1<<20)
		if err != nil {
			return result, err
		}
		hash := sha256.Sum256(raw)
		if hex.EncodeToString(hash[:]) != witness.SHA256 {
			return result, failure(Integrity)
		}
	}
	settings, err := launch.PostgresSettings()
	if err != nil {
		return result, failure(Invalid)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return result, failure(Unavailable)
	}
	defer store.Close()
	outcome, err := store.ImportBackupIntoEmpty(ctx, tools, filepath.Join(parent, stage, "database.dump"))
	if outcome != storage.TransactionNotCommitted {
		result.state = Indeterminate
	}
	if err != nil || outcome != storage.TransactionCommitted {
		return result, failure(Unavailable)
	}
	secrets, err := store.BootstrapSecretsForStartup(ctx, filepath.Join(parent, stage), storage.RequireEstablishedSecretStartup())
	if err != nil {
		return result, failure(Integrity)
	}
	// The restored database, source bytes, server key, and inventory must agree
	// before the fence mutates anything. Missing references never become empty.
	snapshot, err := store.BeginBackupSnapshot(ctx, secrets)
	if err != nil {
		return result, failure(Integrity)
	}
	sourceErr := m.matchSources(snapshot.Sources())
	closeErr := snapshot.Close()
	if sourceErr != nil || closeErr != nil {
		return result, failure(Integrity)
	}
	revision, _ := storage.NewConfigGenerationRevision(m.Generation)
	manifestHash, _ := digest(m.ManifestSHA256)
	expectation, err := storage.NewBackupRestoreExpectation(revision, manifestHash, m.Tables)
	if err != nil {
		return result, failure(Integrity)
	}
	generation, outcome, err := store.RestoreBackupGeneration(ctx, expectation, jobs.FenceRestoredJobs)
	if outcome == storage.TransactionCommitted {
		result.generation = generation
	}
	if err != nil || outcome != storage.TransactionCommitted {
		return result, failure(Unavailable)
	}
	for _, witness := range m.Sources {
		if witness.Bootstrap {
			continue
		}
		raw, err := readBounded(output, "yaml/"+witness.Archive, 1<<20)
		if err != nil {
			return result, err
		}
		if err = writePrivate(output, witness.Path, raw); err != nil {
			return result, err
		}
	}
	if err = writePrivate(output, "bootstrap.launch.yaml", targetBootstrap); err != nil {
		return result, err
	}
	receipt := struct {
		Version  int    `json:"version"`
		Original int64  `json:"archived_generation"`
		Restored int64  `json:"restored_generation"`
		Manifest string `json:"archived_manifest_sha256"`
		Fenced   bool   `json:"old_work_fenced"`
	}{1, m.Generation, generation.Number(), m.ManifestSHA256, true}
	raw, err = json.Marshal(receipt)
	if err != nil {
		return result, failure(Unavailable)
	}
	if err = writePrivate(output, "installation-restored.json", raw); err != nil {
		return result, err
	}
	if err = operations.sync(output, "."); err != nil {
		return result, failure(Unavailable)
	}
	if err = ctx.Err(); err != nil {
		return result, failure(Cancelled)
	}
	if err = operations.rename(root, stage, destination); err != nil {
		return result, failure(Unavailable)
	}
	// A successful rename is already an effect; failed parent fsync is never
	// reported as a rollback. Keep both the DB and directory for inspection.
	result.staged = final
	if err = operations.sync(root, "."); err != nil {
		return result, failure(Unavailable)
	}
	completed := time.Now()
	age := intervention.Sub(m.CutUTC)
	if age < 0 {
		return result, failure(Integrity)
	}
	result.state = Restored
	result.staged = ""
	result.generation = generation
	result.measurement = Measurement{m.CutUTC, intervention.UTC(), completed.UTC(), age, completed.Sub(intervention)}
	return result, nil
}
func safeDestination(name string) bool {
	if len(name) < 1 || len(name) > 80 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func readBounded(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 0 || info.Size() > limit {
		return nil, failure(Integrity)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, failure(Unavailable)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(raw)) != info.Size() {
		return nil, failure(Integrity)
	}
	return raw, nil
}
func writePrivate(root *os.Root, name string, raw []byte) error {
	if !safePath(name) || name == ".env" || name == "database.dump" || strings.HasPrefix(name, "yaml/") {
		return failure(Integrity)
	}
	dir := path.Dir(name)
	if dir != "." {
		if err := root.MkdirAll(dir, 0700); err != nil {
			return failure(Unavailable)
		}
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return failure(Conflict)
	}
	n, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || n != len(raw) {
		return failure(Unavailable)
	}
	// Sync the complete ancestor chain before publishing the directory tree.
	for current := dir; ; current = path.Dir(current) {
		if err := syncDirectory(root, current); err != nil {
			return err
		}
		if current == "." {
			break
		}
	}
	return nil
}
func syncDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return failure(Unavailable)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return failure(Unavailable)
	}
	return nil
}
