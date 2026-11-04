package storage

import (
	"bytes"
	"context"

	"github.com/damione1/Homeward/internal/domain"
)

// EnsureIntegrationResourceIdentities runs inside the native configuration
// transaction with identities from its admitted operational source. A removed
// instance retains its identity mapping; that mapping alone never proves current
// existence or grants permission to read an integration.
func (c *ConfigTx) EnsureIntegrationResourceIdentities(ids []domain.IntegrationInstanceID) (err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		key, err := id.Value()
		if err != nil || seen[key] {
			return &Error{kind: ConfigInvalid}
		}
		seen[key] = true
		if choice, present := c.ctx.Value(requestedIntegrationResourceIDKey{}).(requestedIntegrationResourceID); present && choice.target.instance == id {
			if _, err := InsertAPIResourceIdentityTx(c.ctx, c.tx, APIIntegrationInstances, key, choice.id); err != nil {
				return err
			}
		} else if _, err := EnsureAPIResourceIdentityTx(c.ctx, c.tx, APIIntegrationInstances, key); err != nil {
			return err
		}
	}
	if target, present := configurationIntegrationTargetFromContext(c.ctx); present {
		key, _ := target.instance.Value()
		switch target.method {
		case ConfigurationCreateIntegration, ConfigurationUpdateIntegration:
			if !seen[key] {
				return &Error{kind: ConfigIntegrity}
			}
		case ConfigurationDeleteIntegration:
			if seen[key] {
				return &Error{kind: ConfigIntegrity}
			}
		}
	}
	return nil
}

// ReconcileIntegrationResourceIdentities initializes mappings for an existing
// installation before public runtime composition. It fences the exact native
// generation inspected by startup; it cannot activate a new generation or use
// a redacted API projection as source authority.
func (s *Store) ReconcileIntegrationResourceIdentities(ctx context.Context, revision ConfigGenerationRevision, manifest [32]byte, ids []domain.IntegrationInstanceID) error {
	if ctx == nil || s == nil || !revision.Valid() || manifest == ([32]byte{}) {
		return &Error{kind: ConfigInvalid}
	}
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var current *int64
		if err := tx.tx.QueryRow(ctx, `SELECT generation_revision FROM public.config_active WHERE singleton FOR UPDATE`).Scan(&current); err != nil {
			return err
		}
		if current == nil || *current != revision.Number() {
			return &Error{kind: ConfigConflict}
		}
		var actual []byte
		if err := tx.tx.QueryRow(ctx, `SELECT manifest_hash FROM public.config_generations WHERE revision=$1 AND sealed`, revision.Number()).Scan(&actual); err != nil {
			return err
		}
		if !bytes.Equal(actual, manifest[:]) {
			return &Error{kind: ConfigConflict}
		}
		return tx.EnsureIntegrationResourceIdentities(ids)
	})
	if err != nil {
		return err
	}
	if outcome != TransactionCommitted {
		return &Error{kind: Unavailable}
	}
	return nil
}
