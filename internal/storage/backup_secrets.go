package storage

import (
	"bytes"
	"context"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Validate every archived credential using the existing 051 cipher and AAD,
// inside the same snapshot as sources and dump. Never materialize values in an
// archive witness, diagnostic, or a second credential store.
func validateBackupCredentials(ctx context.Context, tx pgx.Tx, secrets *SecretRepository) error {
	rows, err := tx.Query(ctx, `SELECT instance_id,secret_name,revision,key_id,suite,salt,CASE WHEN octet_length(ciphertext) BETWEEN 17 AND 1048592 THEN ciphertext END FROM public.integration_secrets ORDER BY instance_id,secret_name LIMIT 4097`)
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 4096 {
			return &Error{kind: SecretInvalid}
		}
		var instanceText, nameText, suite string
		var revision int64
		var keyID, salt, ciphertext []byte
		if rows.Scan(&instanceText, &nameText, &revision, &keyID, &suite, &salt, &ciphertext) != nil {
			return &Error{kind: SecretIntegrity}
		}
		instance, e := domain.ParseIntegrationInstanceID(instanceText)
		if e != nil {
			return &Error{kind: SecretIntegrity}
		}
		name, e := ParseSecretName(nameText)
		if e != nil {
			return &Error{kind: SecretIntegrity}
		}
		handle, e := NewSecretHandle(instance, name)
		if e != nil {
			return &Error{kind: SecretIntegrity}
		}
		parsed, e := parseSecretCipherSuite(suite)
		if e != nil || parsed != secretCipherSuiteAES256GCMHKDFSHA256V1 || revision < 1 || len(salt) != 32 || !bytes.Equal(keyID, secrets.key.id[:]) || len(ciphertext) < 17 {
			return &Error{kind: SecretIntegrity}
		}
		raw, e := secrets.open(handle, revision, salt, ciphertext)
		if e != nil {
			return &Error{kind: SecretIntegrity}
		}
		valid := len(raw) > 0 && len(raw) <= secretLimit
		clear(raw)
		if !valid {
			return &Error{kind: SecretIntegrity}
		}
	}
	if rows.Err() != nil {
		return &Error{kind: SecretUnavailable}
	}
	return nil
}
