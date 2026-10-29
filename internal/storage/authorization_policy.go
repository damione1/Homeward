package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

var ErrPolicyNotConfigured = errors.New("authorization: policy not configured")

type AuthorizationPolicySnapshot struct {
	source         authorization.PolicySource
	version        authorization.Version
	rightsRevision uint64
	role           authv1.AccountRole
	status         authv1.AccountStatus
	revocation     uint64
	updatedAt      time.Time
}

func (s AuthorizationPolicySnapshot) State() (authorization.AccountState, error) {
	if s.updatedAt.IsZero() {
		return authorization.AccountState{}, ErrAccountUnavailable
	}
	state, err := authorization.NewAccountState(s.source, s.role, s.status, s.revocation, s.version, s.rightsRevision)
	if err != nil {
		return authorization.AccountState{}, ErrAccountUnavailable
	}
	return state, nil
}
func (s AuthorizationPolicySnapshot) Source() (authorization.PolicySource, error) {
	if _, err := s.State(); err != nil {
		return authorization.PolicySource{}, err
	}
	return s.source, nil
}
func (s AuthorizationPolicySnapshot) Version() (authorization.Version, error) {
	if _, err := s.State(); err != nil {
		return authorization.Version{}, err
	}
	return s.version, nil
}
func (s AuthorizationPolicySnapshot) UpdatedAt() (time.Time, error) {
	if _, err := s.State(); err != nil {
		return time.Time{}, err
	}
	return s.updatedAt, nil
}

type AuthorizationPolicyWriteOutcome struct {
	snapshot      AuthorizationPolicySnapshot
	rightsChanged bool
	valid         bool
}

func (o AuthorizationPolicyWriteOutcome) Result() (AuthorizationPolicySnapshot, bool, error) {
	if !o.valid {
		return AuthorizationPolicySnapshot{}, false, ErrAccountUnavailable
	}
	if _, err := o.snapshot.State(); err != nil {
		return AuthorizationPolicySnapshot{}, false, err
	}
	return o.snapshot, o.rightsChanged, nil
}

func policyBinding(account contracts.AccountID) (ConfigDocumentBinding, error) {
	text, err := sqlAccountID(account)
	if err != nil {
		return ConfigDocumentBinding{}, err
	}
	digest := sha256.Sum256([]byte(text))
	suffix := hex.EncodeToString(digest[:])
	id, err := NewConfigDocumentID("authorization:" + suffix)
	if err != nil {
		return ConfigDocumentBinding{}, err
	}
	return NewConfigDocumentBinding(id, "authorization/"+suffix+"/policy.yaml")
}

func (c *ConfigTx) SaveAuthorizationPolicy(source authorization.PolicySource, metadata ConfigVersionMetadata) (ref ConfigVersionRef, err error) {
	defer func() { c.remember(err) }()
	account, err := source.Account()
	if err != nil {
		return ConfigVersionRef{}, authorization.ErrInvalid
	}
	binding, err := policyBinding(account)
	if err != nil {
		return ConfigVersionRef{}, err
	}
	data, err := source.Bytes()
	if err != nil {
		return ConfigVersionRef{}, authorization.ErrInvalid
	}
	hash, err := source.SHA256()
	if err != nil {
		return ConfigVersionRef{}, authorization.ErrInvalid
	}
	return c.saveAdmittedSource(binding, configSourceAuthorization, data, hash, metadata)
}

// ActivateAuthorizationPolicy performs CAS against the active exact source
// version. It readmits the referenced source for this account before exposing
// it, even for callers composing a transaction directly through ConfigTx.
func (c *ConfigTx) ActivateAuthorizationPolicy(account contracts.AccountID, expected authorization.ExpectedVersion, ref ConfigVersionRef) (changed bool, err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return false, err
	}
	accountText, err := sqlAccountID(account)
	if err != nil {
		return false, err
	}
	absent, version, err := expected.Values()
	if err != nil || !ref.Valid() {
		return false, authorization.ErrInvalid
	}
	binding, err := policyBinding(account)
	if err != nil || binding.id != ref.document {
		return false, authorization.ErrInvalid
	}
	var data, hash []byte
	var schema, profile string
	if err := c.tx.QueryRow(c.ctx, `SELECT yaml_source,source_hash,schema_version::text,syntax_profile::text
		FROM public.config_versions WHERE document_id=$1 AND revision=$2`, ref.document.document, ref.revision.version).Scan(&data, &hash, &schema, &profile); err != nil {
		return false, err
	}
	if schema != authorization.Schema || profile != authorization.SyntaxProfile || len(hash) != sha256.Size {
		return false, authorization.ErrInvalid
	}
	source, err := authorization.AdmitPolicySource(account, data)
	if err != nil {
		return false, err
	}
	actualHash, _ := source.SHA256()
	if string(actualHash[:]) != string(hash) {
		return false, authorization.ErrInvalid
	}
	semantic, _ := source.SemanticSHA256()
	var currentRevision, currentRights int64
	var currentHash, currentSemantic []byte
	queryErr := c.tx.QueryRow(c.ctx, `SELECT p.version_revision,p.rights_revision,p.semantic_sha256,v.source_hash
		FROM public.authorization_policy_active p JOIN public.config_versions v
		ON v.document_id=p.document_id AND v.revision=p.version_revision WHERE p.account_id=$1 FOR UPDATE OF p`, accountText).Scan(&currentRevision, &currentRights, &currentSemantic, &currentHash)
	found := queryErr == nil
	if queryErr != nil && !errors.Is(queryErr, pgx.ErrNoRows) {
		return false, queryErr
	}
	if absent {
		if found {
			return false, authorization.ErrConflict
		}
	} else {
		expectedRevision, expectedHash, versionErr := version.Values()
		if versionErr != nil || !found || uint64(currentRevision) != expectedRevision || string(currentHash) != string(expectedHash[:]) {
			return false, authorization.ErrConflict
		}
	}
	if found && (ref.revision.version <= currentRevision || len(currentSemantic) != sha256.Size || currentRights <= 0) {
		return false, authorization.ErrConflict
	}
	changed = !found || string(currentSemantic) != string(semantic[:])
	rights := ref.revision.version
	if !changed {
		rights = currentRights
	}
	if found {
		_, err = c.tx.Exec(c.ctx, `UPDATE public.authorization_policy_active SET document_id=$2,version_revision=$3,
			rights_revision=$4,semantic_sha256=$5 WHERE account_id=$1`, accountText, ref.document.document, ref.revision.version, rights, semantic[:])
	} else {
		_, err = c.tx.Exec(c.ctx, `INSERT INTO public.authorization_policy_active(account_id,document_id,version_revision,rights_revision,semantic_sha256)
			VALUES($1,$2,$3,$4,$5)`, accountText, ref.document.document, ref.revision.version, rights, semantic[:])
	}
	return changed, err
}

func accountConfigTransaction[T any](s *Store, ctx context.Context, event accountAudit, fn func(pgx.Tx, *ConfigTx) (T, error)) (T, error) {
	var zero T
	if fn == nil {
		return zero, ErrAccountInvalid
	}
	return accountTransaction(s, ctx, event, func(tx pgx.Tx) (T, error) {
		var result T
		err := withConfigTx(ctx, tx, func(config *ConfigTx) error { var err error; result, err = fn(tx, config); return err })
		return result, err
	})
}

const authorizationPolicyColumns = `a.id,a.role,a.status,a.revocation_revision,p.document_id,p.version_revision,
    p.rights_revision,p.semantic_sha256,v.source_hash,v.schema_version::text,v.syntax_profile::text,v.yaml_source,v.accepted_at`
const authorizationPolicyJoin = ` FROM public.auth_accounts a LEFT JOIN public.authorization_policy_active p ON p.account_id=a.id
    LEFT JOIN public.config_versions v ON v.document_id=p.document_id AND v.revision=p.version_revision `

func scanAuthorizationPolicy(row pgx.Row) (AuthorizationPolicySnapshot, error) {
	var id string
	var role, status int16
	var revocation int64
	var document *string
	var revision, rights *int64
	var semantic, hash, data []byte
	var schema, profile *string
	var updated *time.Time
	err := row.Scan(&id, &role, &status, &revocation, &document, &revision, &rights, &semantic, &hash, &schema, &profile, &data, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizationPolicySnapshot{}, ErrAccountNotFound
	}
	if err != nil {
		return AuthorizationPolicySnapshot{}, err
	}
	if document == nil {
		return AuthorizationPolicySnapshot{}, ErrPolicyNotConfigured
	}
	account, err := contracts.ParseAccountID(id)
	if err != nil || revision == nil || rights == nil || *revision <= 0 || *rights <= 0 || revocation <= 0 ||
		len(hash) != sha256.Size || len(semantic) != sha256.Size || schema == nil || *schema != authorization.Schema ||
		profile == nil || *profile != authorization.SyntaxProfile || updated == nil || updated.IsZero() {
		return AuthorizationPolicySnapshot{}, ErrAccountUnavailable
	}
	binding, err := policyBinding(account)
	if err != nil || binding.id.document != *document {
		return AuthorizationPolicySnapshot{}, ErrAccountUnavailable
	}
	source, err := authorization.AdmitPolicySource(account, data)
	if err != nil {
		return AuthorizationPolicySnapshot{}, ErrAccountUnavailable
	}
	var sourceHash [sha256.Size]byte
	copy(sourceHash[:], hash)
	semanticHash, _ := source.SemanticSHA256()
	if string(semanticHash[:]) != string(semantic) {
		return AuthorizationPolicySnapshot{}, ErrAccountUnavailable
	}
	version, err := authorization.NewVersion(uint64(*revision), sourceHash)
	if err != nil {
		return AuthorizationPolicySnapshot{}, ErrAccountUnavailable
	}
	admittedRole, err := admittedSQLRole(role)
	if err != nil {
		return AuthorizationPolicySnapshot{}, err
	}
	admittedStatus, err := admittedSQLStatus(status)
	if err != nil {
		return AuthorizationPolicySnapshot{}, err
	}
	snapshot := AuthorizationPolicySnapshot{source, version, uint64(*rights), admittedRole, admittedStatus, uint64(revocation), updated.UTC()}
	if _, err := snapshot.State(); err != nil {
		return AuthorizationPolicySnapshot{}, err
	}
	return snapshot, nil
}

// AuthorizationPolicy is a trusted hydration port. Public callers must use the
// authenticated service, which also rechecks administrative rights in SQL.
func (s *Store) AuthorizationPolicy(ctx context.Context, target contracts.AccountID) (AuthorizationPolicySnapshot, error) {
	text, err := sqlAccountID(target)
	if err != nil || s == nil || s.pool == nil {
		return AuthorizationPolicySnapshot{}, ErrAccountInvalid
	}
	if err := accountContextError(ctx); err != nil {
		return AuthorizationPolicySnapshot{}, err
	}
	return scanAuthorizationPolicy(s.pool.QueryRow(ctx, `SELECT `+authorizationPolicyColumns+authorizationPolicyJoin+` WHERE a.id=$1`, text))
}

func (s *Store) GetAuthorizationPolicy(ctx context.Context, actor, target contracts.AccountID) (AuthorizationPolicySnapshot, error) {
	text, err := sqlAccountID(target)
	if err != nil {
		return AuthorizationPolicySnapshot{}, ErrAccountInvalid
	}
	return accountTransaction(s, ctx, accountAudit{actor: &actor, target: &target, action: authActionPolicyRead}, func(tx pgx.Tx) (AuthorizationPolicySnapshot, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AuthorizationPolicySnapshot{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return AuthorizationPolicySnapshot{}, err
		}
		return scanAuthorizationPolicy(tx.QueryRow(ctx, `SELECT `+authorizationPolicyColumns+authorizationPolicyJoin+` WHERE a.id=$1`, text))
	})
}

func (s *Store) ReplaceAuthorizationPolicy(ctx context.Context, actor, target contracts.AccountID, source authorization.PolicySource, expected authorization.ExpectedVersion) (AuthorizationPolicyWriteOutcome, error) {
	account, err := source.Account()
	if err != nil || account != target {
		return AuthorizationPolicyWriteOutcome{}, authorization.ErrInvalid
	}
	text, err := sqlAccountID(target)
	if err != nil {
		return AuthorizationPolicyWriteOutcome{}, ErrAccountInvalid
	}
	if _, _, err := expected.Values(); err != nil {
		return AuthorizationPolicyWriteOutcome{}, err
	}
	return accountConfigTransaction(s, ctx, accountAudit{actor: &actor, target: &target, action: authActionPolicyReplace}, func(tx pgx.Tx, config *ConfigTx) (AuthorizationPolicyWriteOutcome, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		if _, err := accountByID(ctx, tx, target); err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		author, _ := NewConfigAuthor(mustAccountText(actor))
		origin, _ := NewConfigSource("authorization-api")
		metadata, err := NewConfigVersionMetadata(author, origin)
		if err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		ref, err := config.SaveAuthorizationPolicy(source, metadata)
		if err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		changed, err := config.ActivateAuthorizationPolicy(target, expected, ref)
		if err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		snapshot, err := scanAuthorizationPolicy(tx.QueryRow(ctx, `SELECT `+authorizationPolicyColumns+authorizationPolicyJoin+` WHERE a.id=$1`, text))
		if err != nil {
			return AuthorizationPolicyWriteOutcome{}, err
		}
		return AuthorizationPolicyWriteOutcome{snapshot, changed, true}, nil
	})
}

func mustAccountText(id contracts.AccountID) string { text, _ := id.Value(); return text }
