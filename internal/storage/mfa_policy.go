package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
)

type MFAPolicySnapshot struct {
	source  authmfa.PolicySource
	version authmfa.ExpectedPolicyVersion
	updated time.Time
}

func (p MFAPolicySnapshot) Source() (authmfa.PolicySource, error) {
	if _, _, err := p.version.Values(); err != nil || p.updated.IsZero() {
		return authmfa.PolicySource{}, mfaError(authmfa.ErrorIntegrity)
	}
	if _, err := p.source.Required(); err != nil {
		return authmfa.PolicySource{}, err
	}
	return p.source, nil
}
func (p MFAPolicySnapshot) Version() (authmfa.ExpectedPolicyVersion, error) {
	if _, err := p.Source(); err != nil {
		return authmfa.ExpectedPolicyVersion{}, err
	}
	return p.version, nil
}
func (p MFAPolicySnapshot) UpdatedAt() (time.Time, error) {
	if _, err := p.Source(); err != nil {
		return time.Time{}, err
	}
	return p.updated, nil
}
func (p MFAPolicySnapshot) same(expected authmfa.ExpectedPolicyVersion) bool {
	_, _, err := expected.Values()
	return err == nil && p.version == expected && !p.updated.IsZero()
}

const mfaPolicyQuery = `SELECT p.require_admin_factor,p.version_revision,v.source_hash,v.yaml_source,v.accepted_at,d.kind::text,v.schema_version::text,v.syntax_profile::text
 FROM public.auth_mfa_policy p JOIN public.config_documents d ON d.id=p.document_id
 JOIN public.config_versions v ON v.document_id=p.document_id AND v.revision=p.version_revision
 WHERE p.singleton AND p.document_id='security:mfa-policy'`

func scanMFAPolicy(row pgx.Row) (MFAPolicySnapshot, error) {
	var required bool
	var revision int64
	var hash, data []byte
	var updated time.Time
	var kind, schema, profile string
	if err := row.Scan(&required, &revision, &hash, &data, &updated, &kind, &schema, &profile); err != nil {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorUnavailable)
	}
	source, err := authmfa.AdmitPolicySource(data)
	actual, se := source.Required()
	digest := sha256.Sum256(data)
	if err != nil || se != nil || actual != required || revision <= 0 || updated.IsZero() || kind != "mfa_policy" || schema != authmfa.PolicySchema || profile != authmfa.PolicySyntaxProfile || !bytes.Equal(hash, digest[:]) {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	version, err := authmfa.NewExpectedPolicyVersion(uint64(revision), hash)
	if err != nil {
		return MFAPolicySnapshot{}, err
	}
	return MFAPolicySnapshot{source: source, version: version, updated: updated}, nil
}
func lockMFAPolicy(ctx context.Context, tx pgx.Tx) (MFAPolicySnapshot, error) {
	return scanMFAPolicy(tx.QueryRow(ctx, mfaPolicyQuery+` FOR SHARE OF p`))
}
func mfaPolicyBinding() ConfigDocumentBinding {
	return ConfigDocumentBinding{id: ConfigDocumentID{document: "security:mfa-policy"}, path: "security/mfa-policy.yaml"}
}
func (c *ConfigTx) SaveMFAPolicy(source authmfa.PolicySource, metadata ConfigVersionMetadata) (ref ConfigVersionRef, err error) {
	defer func() { c.remember(err) }()
	data, err := source.Bytes()
	if err != nil {
		return ConfigVersionRef{}, err
	}
	hash, _ := source.SHA256()
	return c.saveAdmittedSource(mfaPolicyBinding(), configSourceMFA, data, hash, metadata)
}
func (c *ConfigTx) ActivateMFAPolicy(expected authmfa.ExpectedPolicyVersion, target ConfigVersionRef) (snapshot MFAPolicySnapshot, err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return MFAPolicySnapshot{}, err
	}
	if !target.Valid() || target.document != mfaPolicyBinding().id {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	current, err := scanMFAPolicy(c.tx.QueryRow(c.ctx, mfaPolicyQuery+` FOR UPDATE OF p`))
	if err != nil {
		return MFAPolicySnapshot{}, err
	}
	if !current.same(expected) {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorConflict)
	}
	var data []byte
	if err := c.tx.QueryRow(c.ctx, `SELECT yaml_source FROM public.config_versions WHERE document_id=$1 AND revision=$2 AND schema_version::text=$3 AND syntax_profile::text=$4`, target.document.document, target.revision.version, authmfa.PolicySchema, authmfa.PolicySyntaxProfile).Scan(&data); err != nil {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorIntegrity)
	}
	source, err := authmfa.AdmitPolicySource(data)
	if err != nil {
		return MFAPolicySnapshot{}, err
	}
	required, _ := source.Required()
	if _, err := c.tx.Exec(c.ctx, `UPDATE public.auth_mfa_policy SET version_revision=$1,require_admin_factor=$2 WHERE singleton`, target.revision.version, required); err != nil {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorUnavailable)
	}
	return scanMFAPolicy(c.tx.QueryRow(c.ctx, mfaPolicyQuery))
}

// BootstrapMFAPolicy is called once before admissions, independently of HTTPS
// MFA availability. An existing document without a pointer is corruption.
func (r *MFARepository) BootstrapMFAPolicy(ctx context.Context) (MFAPolicySnapshot, error) {
	if !r.valid() {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return r.store.BootstrapMFAPolicy(ctx)
}

// BootstrapMFAPolicy initializes only the versioned configuration. It needs
// neither HTTPS nor a key repository, and must precede account admissions.
func (s *Store) BootstrapMFAPolicy(ctx context.Context) (MFAPolicySnapshot, error) {
	return mfaStoreTransaction(s, ctx, func(tx pgx.Tx) (snapshot MFAPolicySnapshot, err error) {
		err = withConfigTx(ctx, tx, func(c *ConfigTx) error {
			if _, err := lockAuthBootstrapOnly(ctx, tx); err != nil {
				return mfaError(authmfa.ErrorUnavailable)
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_mfa_policy WHERE singleton)`).Scan(&exists); err != nil {
				return mfaError(authmfa.ErrorUnavailable)
			}
			if exists {
				var err error
				snapshot, err = lockMFAPolicy(ctx, tx)
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.config_documents WHERE id='security:mfa-policy')`).Scan(&exists); err != nil || exists {
				return mfaError(authmfa.ErrorIntegrity)
			}
			author, _ := NewConfigAuthor("homeward")
			source, _ := NewConfigSource("mfa-policy-initialization")
			metadata, _ := NewConfigVersionMetadata(author, source)
			ref, err := c.SaveMFAPolicy(authmfa.NewPolicySource(false), metadata)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO public.auth_mfa_policy(singleton,document_id,version_revision,require_admin_factor) VALUES(true,$1,$2,false)`, ref.document.document, ref.revision.version); err != nil {
				return mfaError(authmfa.ErrorUnavailable)
			}
			snapshot, err = lockMFAPolicy(ctx, tx)
			return err
		})
		return snapshot, err
	})
}
func (r *MFARepository) MFAPolicy(ctx context.Context) (MFAPolicySnapshot, error) {
	if !r.valid() || accountContextError(ctx) != nil {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return r.store.MFAPolicy(ctx)
}

func (s *Store) MFAPolicy(ctx context.Context) (MFAPolicySnapshot, error) {
	if s == nil || s.pool == nil || accountContextError(ctx) != nil {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return scanMFAPolicy(s.pool.QueryRow(ctx, mfaPolicyQuery))
}

type MFAPolicyWriteResult struct{ *mfaPolicyWriteValue }
type mfaPolicyWriteValue struct {
	policy   MFAPolicySnapshot
	affected []contracts.AccountID
	changed  bool
}

func (v MFAPolicyWriteResult) Result() (MFAPolicySnapshot, []contracts.AccountID, bool, error) {
	if v.mfaPolicyWriteValue == nil {
		return MFAPolicySnapshot{}, nil, false, mfaError(authmfa.ErrorInvalidInput)
	}
	if _, err := v.policy.Source(); err != nil {
		return MFAPolicySnapshot{}, nil, false, err
	}
	return v.policy, append([]contracts.AccountID(nil), v.affected...), v.changed, nil
}
func (r *MFARepository) SetMFAPolicy(ctx context.Context, actor contracts.AccountID, expected authmfa.ExpectedPolicyVersion, desired authmfa.PolicySource, now time.Time) (MFAPolicyWriteResult, error) {
	actorText, err := sqlAccountID(actor)
	required, de := desired.Required()
	_, _, ee := expected.Values()
	if accountContextError(ctx) != nil || err != nil || de != nil || ee != nil || now.IsZero() {
		return MFAPolicyWriteResult{}, mfaError(authmfa.ErrorInvalidInput)
	}
	limited, cancel := context.WithTimeout(ctx, authmfa.PolicyMutationTimeout)
	defer cancel()
	var attempted []contracts.AccountID
	result, writeErr := mfaTransaction(r, limited, func(tx pgx.Tx) (result MFAPolicyWriteResult, err error) {
		err = withConfigTx(limited, tx, func(c *ConfigTx) error {
			if _, err := lockAuthSingleton(limited, tx); err != nil {
				return mfaError(authmfa.ErrorUnavailable)
			}
			current, err := scanMFAPolicy(tx.QueryRow(limited, mfaPolicyQuery+` FOR UPDATE OF p`))
			if err != nil {
				return err
			}
			if err := requireAdmin(limited, tx, actor); err != nil {
				return mfaError(authmfa.ErrorInvalidProof)
			}
			if !current.same(expected) {
				return mfaError(authmfa.ErrorConflict)
			}
			old, _ := current.source.Required()
			value := &mfaPolicyWriteValue{policy: current}
			result = MFAPolicyWriteResult{value}
			if old == required {
				if err := insertSessionAudit(limited, tx, actor, authActionMFAPolicyReplace, authOutcomeAdmitted); err != nil {
					return mfaError(authmfa.ErrorUnavailable)
				}
				return nil
			}
			if required {
				var instance string
				if err := tx.QueryRow(limited, `SELECT instance_id FROM public.auth_mfa_instance WHERE singleton`).Scan(&instance); err != nil || instance == "" {
					return mfaError(authmfa.ErrorUnavailable)
				}
				rows, err := tx.Query(limited, `SELECT id FROM public.auth_accounts WHERE role=1 AND status=1 AND NOT second_factor_enabled ORDER BY id LIMIT $1 FOR UPDATE`, authmfa.PolicyAffectedAccountLimit+1)
				if err != nil {
					return mfaError(authmfa.ErrorUnavailable)
				}
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return mfaError(authmfa.ErrorUnavailable)
					}
					parsed, err := contracts.ParseAccountID(id)
					if err != nil {
						rows.Close()
						return mfaError(authmfa.ErrorIntegrity)
					}
					value.affected = append(value.affected, parsed)
				}
				rows.Close()
				if rows.Err() != nil {
					return mfaError(authmfa.ErrorUnavailable)
				}
				if len(value.affected) > authmfa.PolicyAffectedAccountLimit {
					return mfaError(authmfa.ErrorBusy)
				}
			}
			attempted = append([]contracts.AccountID(nil), value.affected...)
			author, _ := NewConfigAuthor(actorText)
			source, _ := NewConfigSource("mfa-policy-api")
			metadata, _ := NewConfigVersionMetadata(author, source)
			target, err := c.SaveMFAPolicy(desired, metadata)
			if err != nil {
				return err
			}
			value.policy, err = c.ActivateMFAPolicy(expected, target)
			if err != nil {
				return err
			}
			for start := 0; start < len(value.affected); start += authmfa.PolicyBatchLimit {
				end := min(start+authmfa.PolicyBatchLimit, len(value.affected))
				for _, account := range value.affected[start:end] {
					if err := revokeMFAAccountTx(limited, tx, account, now); err != nil {
						return err
					}
				}
			}
			if err := insertSessionAudit(limited, tx, actor, authActionMFAPolicyReplace, authOutcomeAdmitted); err != nil {
				return mfaError(authmfa.ErrorUnavailable)
			}
			value.changed = true
			return nil
		})
		return result, err
	})
	if authmfa.KindOf(writeErr) == authmfa.ErrorCommitUnknown {
		return MFAPolicyWriteResult{&mfaPolicyWriteValue{affected: attempted}}, writeErr
	}
	return result, writeErr
}
func (v MFAPolicyWriteResult) AttemptedAccounts() []contracts.AccountID {
	if v.mfaPolicyWriteValue == nil {
		return nil
	}
	return append([]contracts.AccountID(nil), v.affected...)
}

// ReconcileMFAPolicyPrimary only reads settled primary state, never retries Set.
// The process fence remains closed until its caller publishes every returned
// account and the policy. Missing accounts are returned as settled denials.
func (r *MFARepository) ReconcileMFAPolicyPrimary(ctx context.Context, known []contracts.AccountID) (MFAPolicySnapshot, error) {
	if !r.valid() {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return r.store.ReconcileMFAPolicyPrimary(ctx, known)
}

// Reconciliation is settled security metadata; it never needs a crypto key.
func (s *Store) ReconcileMFAPolicyPrimary(ctx context.Context, known []contracts.AccountID) (MFAPolicySnapshot, error) {
	if accountContextError(ctx) != nil || s == nil || s.pool == nil || len(known) > authmfa.PolicyAffectedAccountLimit {
		return MFAPolicySnapshot{}, mfaError(authmfa.ErrorInvalidInput)
	}
	limited, cancel := context.WithTimeout(ctx, authmfa.PolicyMutationTimeout)
	defer cancel()
	return mfaStoreTransaction(s, limited, func(tx pgx.Tx) (snapshot MFAPolicySnapshot, err error) {
		err = withConfigTx(limited, tx, func(_ *ConfigTx) error {
			if _, err := lockAuthSingleton(limited, tx); err != nil {
				return mfaError(authmfa.ErrorUnavailable)
			}
			snapshot, err = lockMFAPolicy(limited, tx)
			if err != nil {
				return err
			}
			// Inputs are already sorted/deduplicated by the closed fence cohort; sort
			// again at this boundary so reconciliation cannot invert account locks.
			ordered, err := sortedMFAAccounts(known)
			if err != nil {
				return err
			}
			for _, account := range ordered {
				id, _ := account.Value()
				var revision int64
				err := tx.QueryRow(limited, `SELECT revocation_revision FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&revision)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil || revision <= 0 {
					return mfaError(authmfa.ErrorIntegrity)
				}
				if err := readSettledMFAAccountTx(limited, tx, account); err != nil {
					return err
				}
			}
			return nil
		})
		return snapshot, err
	})
}
