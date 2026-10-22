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

// AuthorizationPolicyBinding exposes the existing canonical binding; it does
// not introduce another path, document identity or policy writer.
func AuthorizationPolicyBinding(account contracts.AccountID) (ConfigDocumentBinding, error) {
	return policyBinding(account)
}

// ReadActiveAuthorizationVersion follows the independent policy pointer even
// when another retained version is the document's latest metadata row.
// It is a trusted composition read; public routes admit current admin rights.
func (s *Store) ReadActiveAuthorizationVersion(ctx context.Context, id ConfigDocumentID) (authorization.Version, bool, error) {
	if !id.Valid() {
		return authorization.Version{}, false, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return authorization.Version{}, false, err
	}
	defer cancel()
	defer rollback(tx)
	var accountText, schema, profile string
	var revision int64
	var hash []byte
	err = tx.QueryRow(ctx, `SELECT p.account_id,p.version_revision,v.source_hash,v.schema_version::text,v.syntax_profile::text
		FROM public.authorization_policy_active p LEFT JOIN public.config_versions v ON v.document_id=p.document_id AND v.revision=p.version_revision WHERE p.document_id=$1`, id.String()).Scan(&accountText, &revision, &hash, &schema, &profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return authorization.Version{}, false, nil
	}
	if err != nil {
		return authorization.Version{}, false, &Error{kind: Unavailable}
	}
	account, err := contracts.ParseAccountID(accountText)
	if err != nil {
		return authorization.Version{}, false, &Error{kind: ConfigIntegrity}
	}
	binding, err := policyBinding(account)
	if err != nil || binding.ID() != id || revision <= 0 || len(hash) != 32 || schema != authorization.Schema || profile != authorization.SyntaxProfile {
		return authorization.Version{}, false, &Error{kind: ConfigIntegrity}
	}
	var source [32]byte
	copy(source[:], hash)
	version, err := authorization.NewVersion(uint64(revision), source)
	if err != nil {
		return authorization.Version{}, false, &Error{kind: ConfigIntegrity}
	}
	if err := tx.Commit(ctx); err != nil {
		return authorization.Version{}, false, &Error{kind: Unavailable}
	}
	return version, true, nil
}

// ReadActivationAuthorizationSource reads the exact policy after-version
// explicitly audited by the selected activation. A policy is never inferred
// from an unrelated generation member or a latest retained version.
// Generations inheriting a policy without their own delta remain unavailable;
// this conservative reader does not claim complete historical policy mapping.
func (s *Store) ReadActivationAuthorizationSource(ctx context.Context, account contracts.AccountID, generation ConfigGenerationRevision, manifest [32]byte) (authorization.PolicySource, error) {
	binding, err := policyBinding(account)
	if err != nil || !generation.Valid() || manifest == ([32]byte{}) {
		return authorization.PolicySource{}, &Error{kind: ConfigInvalid}
	}
	accountText, err := account.Value()
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: ConfigInvalid}
	}
	ctx, cancel, tx, err := publicConfigRead(ctx, s)
	if err != nil {
		return authorization.PolicySource{}, err
	}
	defer cancel()
	defer rollback(tx)
	historical, err := readConfigGenerationTx(ctx, tx, generation)
	if err != nil {
		return authorization.PolicySource{}, err
	}
	if historical.ManifestSHA256() != manifest {
		return authorization.PolicySource{}, &Error{kind: ConfigConflict}
	}
	if historical.Availability() != ConfigGenerationComplete {
		return authorization.PolicySource{}, &Error{kind: ConfigIncomplete}
	}
	var revision int64
	var auditedHash, raw, sourceHash []byte
	var schema, profile *string
	err = tx.QueryRow(ctx, `SELECT p.after_revision,p.after_hash,v.yaml_source,v.source_hash,v.schema_version::text,v.syntax_profile::text
		FROM public.config_activation_policy_changes p JOIN public.config_activation_attempts a ON a.attempt_id=p.attempt_id
		LEFT JOIN public.config_versions v ON v.document_id=$1 AND v.revision=p.after_revision
		WHERE p.account_id=$2 AND a.target_generation=$3 AND a.target_manifest_hash=$4`, binding.ID().String(), accountText, generation.Number(), manifest[:]).Scan(&revision, &auditedHash, &raw, &sourceHash, &schema, &profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return authorization.PolicySource{}, &Error{kind: ConfigNotFound}
	}
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: Unavailable}
	}
	if revision <= 0 || len(raw) == 0 || schema == nil || profile == nil {
		return authorization.PolicySource{}, &Error{kind: ConfigIncomplete}
	}
	if *schema != authorization.Schema || *profile != authorization.SyntaxProfile || len(auditedHash) != sha256.Size || len(sourceHash) != sha256.Size || !bytes.Equal(auditedHash, sourceHash) {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	source, err := authorization.AdmitPolicySource(account, raw)
	if err != nil {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	hash, _ := source.SHA256()
	if !bytes.Equal(hash[:], sourceHash) {
		return authorization.PolicySource{}, &Error{kind: ConfigIntegrity}
	}
	if err := tx.Commit(ctx); err != nil {
		return authorization.PolicySource{}, &Error{kind: Unavailable}
	}
	return source, nil
}
