package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

// ActivationAttemptID is chosen before entering SQL and survives a lost COMMIT
// acknowledgement. The zero value cannot identify an activation.
type ActivationAttemptID struct{ value [16]byte }

func NewActivationAttemptID(value [16]byte) (ActivationAttemptID, error) {
	if value == ([16]byte{}) {
		return ActivationAttemptID{}, &Error{kind: ConfigInvalid}
	}
	return ActivationAttemptID{value: value}, nil
}

func RandomActivationAttemptID() (ActivationAttemptID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ActivationAttemptID{}, &Error{kind: Unavailable}
	}
	return NewActivationAttemptID(value)
}

func (id ActivationAttemptID) Valid() bool    { return id.value != ([16]byte{}) }
func (id ActivationAttemptID) String() string { return hex.EncodeToString(id.value[:]) }

type configActivationReceipt struct {
	id            ActivationAttemptID
	expected      ExpectedConfigActive
	target        ConfigGenerationRevision
	manifest      [sha256.Size]byte
	auditRequired bool
	audited       bool
}

// RecordActivationAttempt belongs to the same ConfigTx that moved the active
// pointer. It cannot create a success record for an unrelated generation.
func (c *ConfigTx) RecordActivationAttempt(id ActivationAttemptID, metadata ConfigVersionMetadata) (err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return err
	}
	if !id.Valid() || !metadata.author.Valid() || !metadata.source.Valid() || c.activation == nil ||
		!c.activation.auditRequired || c.activation.audited {
		return &Error{kind: ConfigInvalid}
	}
	var expectedRevision any
	var expectedHash any
	switch c.activation.expected.kind {
	case configActiveExpectationAbsent:
	case configActiveExpectationRevision:
		expectedRevision = c.activation.expected.revision.generation
		expectedHash = c.activation.expected.manifest[:]
	default:
		return &Error{kind: ConfigInvalid}
	}
	_, err = c.tx.Exec(c.ctx, `INSERT INTO public.config_activation_attempts
		(attempt_id,expected_generation,expected_manifest_hash,target_generation,target_manifest_hash,author,source)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, id.value[:], expectedRevision, expectedHash,
		c.activation.target.generation, c.activation.manifest[:], metadata.author.author, metadata.source.source)
	if err != nil {
		return configWriteError(err)
	}
	c.activation.audited = true
	c.activation.id = id
	return nil
}

// RecordActivationPolicyChange audits only a policy explicitly included by
// the activation caller. ActivateAuthorizationPolicy independently checks the
// same expected version under SQL CAS; this method confirms its after-row.
func (c *ConfigTx) RecordActivationPolicyChange(id ActivationAttemptID, account contracts.AccountID, before authorization.ExpectedVersion, after ConfigVersionRef) (err error) {
	defer func() { c.remember(err) }()
	if err := c.valid(); err != nil {
		return err
	}
	if !id.Valid() || c.activation == nil || !c.activation.audited || c.activation.id != id || !after.Valid() {
		return &Error{kind: ConfigInvalid}
	}
	accountText, err := sqlAccountID(account)
	if err != nil {
		return err
	}
	binding, err := policyBinding(account)
	if err != nil || after.document != binding.id {
		return &Error{kind: ConfigInvalid}
	}
	absent, beforeVersion, err := before.Values()
	if err != nil {
		return &Error{kind: ConfigInvalid}
	}
	var beforeRevision any
	var beforeHash any
	if !absent {
		revision, hash, err := beforeVersion.Values()
		if err != nil || revision >= uint64(after.revision.version) {
			return &Error{kind: ConfigInvalid}
		}
		beforeRevision, beforeHash = int64(revision), hash[:]
	}
	var currentRevision int64
	var currentHash []byte
	err = c.tx.QueryRow(c.ctx, `SELECT p.version_revision,v.source_hash
		FROM public.authorization_policy_active p JOIN public.config_versions v
		ON v.document_id=p.document_id AND v.revision=p.version_revision
		WHERE p.account_id=$1 AND p.document_id=$2 FOR UPDATE OF p`, accountText, binding.id.document).
		Scan(&currentRevision, &currentHash)
	if err != nil || currentRevision != after.revision.version || len(currentHash) != sha256.Size {
		return &Error{kind: ConfigIntegrity}
	}
	_, err = c.tx.Exec(c.ctx, `INSERT INTO public.config_activation_policy_changes
		(attempt_id,account_id,before_revision,before_hash,after_revision,after_hash)
		VALUES($1,$2,$3,$4,$5,$6)`, id.value[:], accountText, beforeRevision, beforeHash,
		currentRevision, currentHash)
	if err != nil {
		return configWriteError(err)
	}
	return nil
}

type ConfigActivationAttempt struct {
	id        ActivationAttemptID
	expected  ExpectedConfigActive
	target    ConfigGenerationRevision
	manifest  [sha256.Size]byte
	author    ConfigAuthor
	source    ConfigSource
	committed time.Time
}

func (a ConfigActivationAttempt) ID() ActivationAttemptID           { return a.id }
func (a ConfigActivationAttempt) Expected() ExpectedConfigActive    { return a.expected }
func (a ConfigActivationAttempt) Target() ConfigGenerationRevision  { return a.target }
func (a ConfigActivationAttempt) ManifestSHA256() [sha256.Size]byte { return a.manifest }
func (a ConfigActivationAttempt) CommittedAt() time.Time            { return a.committed }
func (a ConfigActivationAttempt) Author() ConfigAuthor              { return a.author }
func (a ConfigActivationAttempt) Source() ConfigSource              { return a.source }

// ReadActivationAttempt leases an independent SQL connection after an
// indeterminate commit. The caller must still reconcile/publish the active
// generation; presence alone does not permit redispatch of device actions.
func (s *Store) ReadActivationAttempt(ctx context.Context, id ActivationAttemptID) (ConfigActivationAttempt, bool, error) {
	if ctx == nil || s == nil || s.pool == nil || !id.Valid() {
		return ConfigActivationAttempt{}, false, &Error{kind: ConfigInvalid}
	}
	connection, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
	if err != nil {
		return ConfigActivationAttempt{}, false, &Error{kind: Unavailable}
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = connection.Close(closeCtx)
	}()
	// A new connection alone can observe a missing attempt while the original
	// backend still owns the commit. Wait for that writer on the same SQL gate,
	// then take a new READ COMMITTED snapshot in the following statement.
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ConfigActivationAttempt{}, false, &Error{kind: Unavailable}
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316,151)`); err != nil {
		// A deadline while waiting is indeterminate, never a proven absence.
		return ConfigActivationAttempt{}, false, &Error{kind: Unavailable}
	}
	var expectedRevision *int64
	var expectedHash, manifest []byte
	var target int64
	var author, source string
	var committed time.Time
	err = tx.QueryRow(ctx, `SELECT expected_generation,expected_manifest_hash,target_generation,
		target_manifest_hash,author,source,committed_at FROM public.config_activation_attempts WHERE attempt_id=$1`,
		id.value[:]).Scan(&expectedRevision, &expectedHash, &target, &manifest, &author, &source, &committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigActivationAttempt{}, false, nil
	}
	if err != nil {
		return ConfigActivationAttempt{}, false, &Error{kind: Unavailable}
	}
	result := ConfigActivationAttempt{id: id, committed: committed}
	result.target, err = NewConfigGenerationRevision(target)
	if err != nil || len(manifest) != sha256.Size || committed.IsZero() {
		return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
	}
	copy(result.manifest[:], manifest)
	result.author, err = NewConfigAuthor(author)
	if err != nil {
		return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
	}
	result.source, err = NewConfigSource(source)
	if err != nil {
		return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
	}
	if expectedRevision == nil {
		if expectedHash != nil {
			return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
		}
		result.expected = ExpectNoConfigActive()
	} else {
		if len(expectedHash) != sha256.Size {
			return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
		}
		revision, err := NewConfigGenerationRevision(*expectedRevision)
		if err != nil {
			return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
		}
		var hash [sha256.Size]byte
		copy(hash[:], expectedHash)
		result.expected, err = ExpectConfigActive(revision, hash)
		if err != nil {
			return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
		}
	}
	var actualManifest []byte
	var sealed bool
	if err := tx.QueryRow(ctx, `SELECT manifest_hash,sealed FROM public.config_generations WHERE revision=$1`, target).
		Scan(&actualManifest, &sealed); err != nil || !sealed || !bytes.Equal(actualManifest, manifest) {
		return ConfigActivationAttempt{}, false, &Error{kind: ConfigIntegrity}
	}
	return result, true, nil
}
