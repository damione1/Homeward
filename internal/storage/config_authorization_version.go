package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

// ReadAuthorizationPolicyVersion reads a documentary exact version through the
// existing account-owned policy binding. It needs neither a generation nor an
// activation delta, and never substitutes a latest or active policy source.
// Public callers must independently admit current administrative permission.
func (s *Store) ReadAuthorizationPolicyVersion(ctx context.Context, account contracts.AccountID, version authorization.Version) (authorization.PolicySource, error) {
	binding, err := policyBinding(account)
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: ConfigInvalid}
	}
	revision, expectedHash, err := version.Values()
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return authorization.PolicySource{}, err
	}
	defer cancel()
	defer rollback(tx)
	var path, kind string
	var representation, schema, profile *string
	var raw, storedHash, ciphertext []byte
	err = tx.QueryRow(ctx, `SELECT d.logical_path,d.kind::text,v.source_representation::text,v.schema_version::text,v.syntax_profile::text,v.yaml_source,v.source_hash,v.ciphertext
		FROM public.config_documents d LEFT JOIN public.config_versions v ON v.document_id=d.id AND v.revision=$2
		WHERE d.id=$1`, binding.ID().String(), int64(revision)).Scan(&path, &kind, &representation, &schema, &profile, &raw, &storedHash, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return authorization.PolicySource{}, &Error{kind: ConfigNotFound}
	}
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: Unavailable}
	}
	if path != binding.Path() || kind != ConfigDocumentKindAuthorizationPolicy.String() {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	if representation == nil || schema == nil || profile == nil || storedHash == nil {
		return authorization.PolicySource{}, &Error{kind: ConfigNotFound}
	}
	if *representation != configPlainSQL || *schema != authorization.Schema || *profile != authorization.SyntaxProfile || len(ciphertext) != 0 || len(storedHash) != sha256.Size {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	if len(raw) == 0 || len(raw) > authorization.MaxSourceBytes {
		return authorization.PolicySource{}, &Error{kind: ConfigIncomplete}
	}
	if !bytes.Equal(storedHash, expectedHash[:]) {
		return authorization.PolicySource{}, &Error{kind: ConfigConflict}
	}
	recomputed := sha256.Sum256(raw)
	if !bytes.Equal(storedHash, recomputed[:]) {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	source, err := authorization.AdmitPolicySource(account, raw)
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	actualAccount, err := source.Account()
	actualHash, hashErr := source.SHA256()
	if err != nil || hashErr != nil || actualAccount != account || actualHash != expectedHash {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	if err := tx.Commit(ctx); err != nil {
		return authorization.PolicySource{}, &Error{kind: Unavailable}
	}
	return source, nil
}
