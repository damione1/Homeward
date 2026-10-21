package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
)

type SecretStartupState uint8

const (
	SecretStartupUnknown SecretStartupState = iota
	SecretStartupFreshPair
	SecretStartupEstablishedPair
	SecretStartupOrphanFile
	SecretStartupOrphanDB
	SecretStartupHistoryWithoutKey
)

type SecretStartupSourceOrigin uint8

const (
	SecretStartupOriginUnknown SecretStartupSourceOrigin = iota
	SecretStartupLegacyCombined
	SecretStartupExplicitBootstrap
)

type secretStartupAdmissionKind uint8

const (
	secretStartupAdmissionUnknown secretStartupAdmissionKind = iota
	secretStartupEstablishedOnly
	secretStartupFreshEligible
)

// A fresh admission is created only after the caller has admitted exact raw
// operational bytes. It is inapplicable to an automatically discovered sidecar.
type SecretStartupAdmission struct {
	kind       secretStartupAdmissionKind
	origin     SecretStartupSourceOrigin
	sourceHash [sha256.Size]byte
}

func RequireEstablishedSecretStartup() SecretStartupAdmission {
	return SecretStartupAdmission{kind: secretStartupEstablishedOnly}
}

func FreshSecretStartup(sourceHash [sha256.Size]byte, origin SecretStartupSourceOrigin) (SecretStartupAdmission, error) {
	if sourceHash == ([sha256.Size]byte{}) ||
		(origin != SecretStartupLegacyCombined && origin != SecretStartupExplicitBootstrap) {
		return SecretStartupAdmission{}, &Error{kind: SecretInvalid}
	}
	return SecretStartupAdmission{kind: secretStartupFreshEligible, sourceHash: sourceHash, origin: origin}, nil
}

func (a SecretStartupAdmission) valid() bool {
	switch a.kind {
	case secretStartupEstablishedOnly:
		return a.origin == SecretStartupOriginUnknown && a.sourceHash == ([sha256.Size]byte{})
	case secretStartupFreshEligible:
		return a.sourceHash != ([sha256.Size]byte{}) &&
			(a.origin == SecretStartupLegacyCombined || a.origin == SecretStartupExplicitBootstrap)
	default:
		return false
	}
}

// ProbeStartupSecretState is diagnostic only. BootstrapSecretsForStartup
// rechecks the state under the same 027 lock before any key publication.
func (s *Store) ProbeStartupSecretState(ctx context.Context, dataDir string) (SecretStartupState, error) {
	if ctx == nil || s == nil || s.pool == nil || !filepath.IsAbs(dataDir) ||
		filepath.Clean(dataDir) != dataDir || strings.ContainsRune(dataDir, '\x00') {
		return SecretStartupUnknown, &Error{kind: SecretInvalid}
	}
	root, err := openSecretRoot(dataDir)
	if err != nil {
		return SecretStartupUnknown, err
	}
	defer root.Close()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SecretStartupUnknown, &Error{kind: SecretUnavailable}
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 51)`); err != nil {
		return SecretStartupUnknown, &Error{kind: SecretUnavailable}
	}
	var dbID []byte
	err = tx.QueryRow(ctx, `SELECT key_id FROM public.secret_bootstrap WHERE singleton=true`).Scan(&dbID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SecretStartupUnknown, &Error{kind: SecretUnavailable}
	}
	initialized := err == nil
	key, fileErr := readMasterFile(root)
	state, err := classifyStartupSecretState(ctx, tx, initialized, dbID, key, fileErr)
	if err != nil {
		return SecretStartupUnknown, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretStartupUnknown, &Error{kind: SecretUnavailable}
	}
	return state, nil
}

func classifyStartupSecretState(ctx context.Context, tx pgx.Tx, initialized bool, dbID []byte, key masterKey, fileErr error) (SecretStartupState, error) {
	if initialized {
		if len(dbID) != 32 {
			return SecretStartupUnknown, nil
		}
		if fileErr != nil || !key.valid() || !equalKeyID(dbID, key.id[:]) {
			return SecretStartupOrphanDB, nil
		}
		return SecretStartupEstablishedPair, nil
	}
	var hasHistory bool
	err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM public.config_active WHERE singleton AND generation_revision IS NOT NULL) OR
		EXISTS(SELECT 1 FROM public.config_bootstrap_active WHERE singleton AND version_revision IS NOT NULL) OR
		EXISTS(SELECT 1 FROM public.config_activation_attempts) OR
		EXISTS(SELECT 1 FROM public.secret_salts)`).Scan(&hasHistory)
	if err != nil {
		return SecretStartupUnknown, &Error{kind: SecretUnavailable}
	}
	if hasHistory {
		return SecretStartupHistoryWithoutKey, nil
	}
	if fileErr == nil {
		return SecretStartupOrphanFile, nil
	}
	if errors.Is(fileErr, os.ErrNotExist) {
		return SecretStartupFreshPair, nil
	}
	return SecretStartupUnknown, nil
}
