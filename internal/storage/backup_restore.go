package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// BackupRestoreExpectation is admitted metadata from an authenticated archive.
// Its table witnesses cover every restored public table before any fence write.
type BackupRestoreExpectation struct {
	revision ConfigGenerationRevision
	manifest [32]byte
	tables   []BackupTable
}

func NewBackupRestoreExpectation(revision ConfigGenerationRevision, manifest [32]byte, tables []BackupTable) (BackupRestoreExpectation, error) {
	if !revision.Valid() || manifest == ([32]byte{}) || len(tables) == 0 || len(tables) > 512 {
		return BackupRestoreExpectation{}, &Error{kind: ConfigInvalid}
	}
	for i, t := range tables {
		digest, err := hex.DecodeString(t.SHA256)
		if t.Name == "" || len(t.Name) > 63 || t.Rows < 0 || err != nil || len(digest) != sha256.Size || i > 0 && tables[i-1].Name >= t.Name {
			return BackupRestoreExpectation{}, &Error{kind: ConfigInvalid}
		}
	}
	return BackupRestoreExpectation{revision, manifest, append([]BackupTable(nil), tables...)}, nil
}

// RestoreBackupGeneration has one transactional authority: existing ConfigTx.
// The trusted jobs callback only closes old occurrences in this same tx. No
// Host, permission principal, replay loop or parallel effect journal is made.
func (s *Store) RestoreBackupGeneration(ctx context.Context, expected BackupRestoreExpectation, fenceJobs func(context.Context, pgx.Tx) error) (restored ConfigGenerationRevision, outcome TransactionOutcome, returned error) {
	if nilRegistryPort(ctx) || s == nil || s.pool == nil || !expected.revision.Valid() || len(expected.tables) == 0 || fenceJobs == nil {
		return ConfigGenerationRevision{}, TransactionNotCommitted, &Error{kind: ConfigInvalid}
	}
	runtime, err := s.AcquireCommandRuntime(ctx)
	if err != nil {
		return ConfigGenerationRevision{}, TransactionNotCommitted, err
	}
	defer func() {
		if closeErr := runtime.Close(); closeErr != nil {
			returned = errors.Join(returned, closeErr)
		}
	}()
	outcome, err = s.InConfigTransactionOutcome(ctx, func(c *ConfigTx) error {
		if _, e := c.tx.Exec(ctx, `SELECT set_config('TimeZone','UTC',true),set_config('DateStyle','ISO, YMD',true),set_config('IntervalStyle','postgres',true),set_config('extra_float_digits','3',true),set_config('bytea_output','hex',true)`); e != nil {
			return e
		}
		tables, e := backupTables(ctx, c.tx)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(tables, expected.tables) {
			return &Error{kind: ConfigIntegrity}
		}
		var revision int64
		if e = c.tx.QueryRow(ctx, `SELECT generation_revision FROM public.config_active WHERE singleton FOR UPDATE`).Scan(&revision); e != nil {
			return e
		}
		if revision != expected.revision.Number() {
			return &Error{kind: ConfigIntegrity}
		}
		generation, e := readConfigGenerationTx(ctx, c.tx, expected.revision)
		if e != nil {
			return e
		}
		if generation.ManifestSHA256() != expected.manifest {
			return &Error{kind: ConfigIntegrity}
		}
		if e = fenceJobs(ctx, c.tx); e != nil {
			return e
		}
		response, e := proto.Marshal(&actions.CommandResponse{Status: actions.ActionStatus_ACTION_STATUS_INDETERMINATE})
		if e != nil {
			return &Error{kind: ConfigInvalid}
		}
		// Accepted is fenced too: an explicit old command ID must not become a new
		// effect merely because the archive predates its eventual dispatch.
		if _, e = c.tx.Exec(ctx, `UPDATE public.command_intents SET state='indeterminate',response=$1,result_error='indeterminate',updated_at=clock_timestamp() WHERE state IN ('accepted','dispatching')`, response); e != nil {
			return e
		}
		refs := make([]ConfigVersionRef, 0, len(generation.versions))
		for _, v := range generation.versions {
			refs = append(refs, v.ref)
		}
		switch generation.manifestFormat {
		case ConfigManifestCompiledV2:
			compilation, readErr := readOptionalGenerationCompilation(ctx, c.tx, generation)
			if readErr != nil {
				return readErr
			}
			restored, e = c.RegisterCompiledGenerationWithWorkspace(generation.entry, generation.candidateHash, refs, generation.DeclarativeWorkspace(), compilation)
		case ConfigManifestLegacyV1:
			candidate, restoreErr := generation.Restore()
			if restoreErr != nil {
				return restoreErr
			}
			restored, e = c.RegisterGeneration(candidate, refs)
		default:
			return &Error{kind: ConfigIntegrity}
		}
		if e != nil {
			return e
		}
		active, e := ExpectConfigActive(expected.revision, expected.manifest)
		if e != nil {
			return e
		}
		if e = c.ActivateGeneration(active, restored); e != nil {
			return e
		}
		if generation.manifestFormat == ConfigManifestCompiledV2 {
			id, idErr := RandomActivationAttemptID()
			if idErr != nil {
				return idErr
			}
			author, _ := NewConfigAuthor("operator")
			source, _ := NewConfigSource("backup.restore")
			metadata, _ := NewConfigVersionMetadata(author, source)
			if e = c.RecordActivationAttempt(id, metadata); e != nil {
				return e
			}
		}
		return nil
	})
	if outcome != TransactionCommitted || err != nil {
		return ConfigGenerationRevision{}, outcome, err
	}
	return restored, outcome, nil
}
