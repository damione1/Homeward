package storage

import (
	"context"
	"crypto/sha256"

	"github.com/damione1/Homeward/contracts"
)

// ReadActivationPolicyAccounts returns the explicit audited human cohort
// between the still-visible generation and the current SQL winner. Policies
// remain autonomous documents: none are inferred from a config manifest.
func (s *Store) ReadActivationPolicyAccounts(ctx context.Context, after, through ConfigGenerationRevision, manifest [sha256.Size]byte) ([]contracts.AccountID, error) {
	if s == nil || s.pool == nil || nilRegistryPort(ctx) || !after.Valid() || !through.Valid() || through.Number() < after.Number() || manifest == ([sha256.Size]byte{}) {
		return nil, &Error{kind: ConfigInvalid}
	}
	active, present, err := s.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != through || active.ManifestSHA256() != manifest {
		return nil, &Error{kind: ConfigConflict}
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT p.account_id
		FROM public.config_activation_policy_changes p
		JOIN public.config_activation_attempts a ON a.attempt_id=p.attempt_id
		WHERE a.target_generation>$1 AND a.target_generation<=$2
		ORDER BY p.account_id LIMIT 16385`, after.Number(), through.Number())
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	defer rows.Close()
	accounts := make([]contracts.AccountID, 0)
	for rows.Next() {
		if len(accounts) == 16384 {
			return nil, &Error{kind: ConfigInvalid}
		}
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, &Error{kind: Unavailable}
		}
		account, err := contracts.ParseAccountID(text)
		if err != nil {
			return nil, &Error{kind: ConfigIntegrity}
		}
		accounts = append(accounts, account)
	}
	if rows.Err() != nil {
		return nil, &Error{kind: Unavailable}
	}
	return accounts, nil
}
