package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ValidateBackupSchema is read-only admission for the offline operator path.
// It never upgrades an installation as a side effect of making a backup.
func (s *Store) ValidateBackupSchema(ctx context.Context) error {
	return s.InTransaction(ctx, func(tx pgx.Tx) error {
		return validateBackupSchemaTx(ctx, tx)
	})
}

// The exporter repeats admission in the very transaction it exports. A
// successful preliminary check is not authority for a later snapshot cut.
func validateBackupSchemaTx(ctx context.Context, tx pgx.Tx) error {
	current, err := versionInTx(ctx, tx)
	if err != nil {
		return err
	}
	if current != latestVersion || schemaAdmissionInTx(ctx, tx, current) != schemaAdmissionCurrent {
		return &Error{kind: InconsistentSchema}
	}
	return nil
}
