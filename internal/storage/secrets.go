package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

const secretLimit = 1 << 20
const secretSuiteSQL = "aes256gcm_hkdf_sha256_v1"
const secretFile = ".env"
const secretFilePrefix = "HOMEWARD_SECRETS_KEY_V1="

type secretCipherSuite uint8

const (
	secretCipherSuiteUnspecified secretCipherSuite = iota
	secretCipherSuiteAES256GCMHKDFSHA256V1
)

func (s secretCipherSuite) sqlValue() (string, error) {
	switch s {
	case secretCipherSuiteAES256GCMHKDFSHA256V1:
		return secretSuiteSQL, nil
	default:
		return "", &Error{kind: SecretInvalid}
	}
}

func parseSecretCipherSuite(raw string) (secretCipherSuite, error) {
	switch raw {
	case secretSuiteSQL:
		return secretCipherSuiteAES256GCMHKDFSHA256V1, nil
	default:
		return secretCipherSuiteUnspecified, &Error{kind: SecretIntegrity}
	}
}

// SecretName is an open, instance-local identifier; it is not a credential.
type SecretName struct{ name string }

func ParseSecretName(raw string) (SecretName, error) {
	if raw == "" || len(raw) > 256 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\x00\r\n") {
		return SecretName{}, &Error{kind: SecretInvalid}
	}
	return SecretName{name: raw}, nil
}

func (n SecretName) Value() (string, error) {
	if n.name == "" {
		return "", &Error{kind: SecretInvalid}
	}
	return n.name, nil
}

func (n SecretName) String() string               { return "[REDACTED SECRET NAME]" }
func (n SecretName) GoString() string             { return n.String() }
func (n SecretName) MarshalJSON() ([]byte, error) { return json.Marshal(n.String()) }

// SecretHandle carries the target identity, but does not grant access to it.
type SecretHandle struct {
	instance domain.IntegrationInstanceID
	name     SecretName
}

// SecretRevision is an opaque comparison token for one encrypted handle. It
// cannot be constructed by a caller from a SQL revision or another handle.
type SecretRevision struct {
	handle SecretHandle
	number int64
}

func (v SecretRevision) validFor(handle SecretHandle) bool {
	return v.number > 0 && v.handle == handle
}

// NewerThan compares revisions only within the same authenticated handle.
// It reveals ordering without exposing the SQL revision or allowing callers
// to construct a comparison token.
func (v SecretRevision) NewerThan(previous SecretRevision) bool {
	return v.validFor(previous.handle) && previous.validFor(v.handle) && v.number > previous.number
}

func (v SecretRevision) String() string               { return "[REDACTED SECRET REVISION]" }
func (v SecretRevision) GoString() string             { return v.String() }
func (v SecretRevision) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func NewSecretHandle(instance domain.IntegrationInstanceID, name SecretName) (SecretHandle, error) {
	if _, err := instance.Value(); err != nil {
		return SecretHandle{}, &Error{kind: SecretInvalid}
	}
	if _, err := name.Value(); err != nil {
		return SecretHandle{}, &Error{kind: SecretInvalid}
	}
	return SecretHandle{instance: instance, name: name}, nil
}

func (h SecretHandle) Components() (domain.IntegrationInstanceID, SecretName, error) {
	if _, err := h.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, SecretName{}, &Error{kind: SecretInvalid}
	}
	if _, err := h.name.Value(); err != nil {
		return domain.IntegrationInstanceID{}, SecretName{}, &Error{kind: SecretInvalid}
	}
	return h.instance, h.name, nil
}

func (h SecretHandle) String() string               { return "[REDACTED SECRET HANDLE]" }
func (h SecretHandle) GoString() string             { return h.String() }
func (h SecretHandle) MarshalJSON() ([]byte, error) { return json.Marshal(h.String()) }

// SecretValue never renders its contents through the standard diagnostic paths.
type SecretValue struct{ value []byte }

func NewSecretValue(raw []byte) (SecretValue, error) {
	if len(raw) == 0 || len(raw) > secretLimit {
		return SecretValue{}, &Error{kind: SecretInvalid}
	}
	return SecretValue{value: append([]byte(nil), raw...)}, nil
}

func (v *SecretValue) Bytes() ([]byte, error) {
	if v == nil || len(v.value) == 0 {
		return nil, &Error{kind: SecretInvalid}
	}
	return append([]byte(nil), v.value...), nil
}

func (v SecretValue) String() string               { return "[REDACTED]" }
func (v SecretValue) GoString() string             { return v.String() }
func (v SecretValue) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

type masterKey struct {
	bytes [32]byte
	id    [32]byte
}

func (k *masterKey) valid() bool {
	return k != nil && k.bytes != [32]byte{} && k.id == sha256.Sum256(append([]byte("homeward/secret-master/v1\x00"), k.bytes[:]...))
}
func (k masterKey) String() string   { return "[REDACTED]" }
func (k masterKey) GoString() string { return k.String() }

func newMasterKey() (masterKey, error) {
	var k masterKey
	if _, err := rand.Read(k.bytes[:]); err != nil {
		return masterKey{}, &Error{kind: SecretUnavailable}
	}
	k.id = sha256.Sum256(append([]byte("homeward/secret-master/v1\x00"), k.bytes[:]...))
	if !k.valid() {
		return masterKey{}, &Error{kind: SecretKeyMismatch}
	}
	return k, nil
}

func parseMasterKey(data []byte) (masterKey, error) {
	if len(data) != len(secretFilePrefix)+44+1 || !strings.HasPrefix(string(data), secretFilePrefix) || data[len(data)-1] != '\n' {
		return masterKey{}, &Error{kind: SecretKeyMismatch}
	}
	raw, err := base64.StdEncoding.DecodeString(string(data[len(secretFilePrefix) : len(data)-1]))
	if err != nil || len(raw) != 32 {
		return masterKey{}, &Error{kind: SecretKeyMismatch}
	}
	var k masterKey
	copy(k.bytes[:], raw)
	k.id = sha256.Sum256(append([]byte("homeward/secret-master/v1\x00"), k.bytes[:]...))
	if !k.valid() {
		return masterKey{}, &Error{kind: SecretKeyMismatch}
	}
	return k, nil
}

// SecretRepository is available only after file and database identity agree.
type SecretRepository struct {
	store *Store
	key   masterKey
}

func (r *SecretRepository) String() string               { return "secrets: repository [REDACTED]" }
func (r *SecretRepository) GoString() string             { return r.String() }
func (r *SecretRepository) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

func (r *SecretRepository) Valid() bool {
	return r != nil && r.store != nil && r.store.pool != nil && r.key.valid()
}

// BootstrapSecrets serializes DB/file reconciliation across server processes.
// The configured directory's parent is an operator-controlled persistent mount.
func (s *Store) BootstrapSecrets(ctx context.Context, dataDir string) (*SecretRepository, error) {
	return s.bootstrapSecrets(ctx, dataDir, nil)
}

// BootstrapSecretsForStartup keeps the NHA-027 file/key format but refuses an
// orphan file, an orphan database key or established activation history without
// a key. The admission decision is repeated under the NHA-027 SQL lock.
func (s *Store) BootstrapSecretsForStartup(ctx context.Context, dataDir string, admission SecretStartupAdmission) (*SecretRepository, error) {
	if !admission.valid() {
		return nil, &Error{kind: SecretInvalid}
	}
	return s.bootstrapSecrets(ctx, dataDir, &admission)
}

func (s *Store) bootstrapSecrets(ctx context.Context, dataDir string, admission *SecretStartupAdmission) (*SecretRepository, error) {
	if ctx == nil || s == nil || s.pool == nil || !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir || strings.ContainsRune(dataDir, '\x00') {
		return nil, &Error{kind: SecretInvalid}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := openSecretRoot(dataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var key masterKey
	var callbackErr error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 51)`); e != nil {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		var dbID []byte
		e := tx.QueryRow(ctx, `SELECT key_id FROM public.secret_bootstrap WHERE singleton = true`).Scan(&dbID)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		initialized := e == nil
		key, e = readMasterFile(root)
		if admission != nil {
			state, stateErr := classifyStartupSecretState(ctx, tx, initialized, dbID, key, e)
			if stateErr != nil {
				callbackErr = stateErr
				return stateErr
			}
			if state != SecretStartupEstablishedPair &&
				!(state == SecretStartupFreshPair && admission.kind == secretStartupFreshEligible) {
				callbackErr = &Error{kind: SecretKeyMismatch}
				return callbackErr
			}
		}
		switch {
		case e == nil:
		case errors.Is(e, os.ErrNotExist) && !initialized:
			if e = cleanupPendingFiles(root); e != nil {
				callbackErr = e
				return e
			}
			key, e = newMasterKey()
			if e == nil {
				e = publishMasterFile(root, key)
			}
			if e != nil {
				callbackErr = e
				return e
			}
		default:
			callbackErr = &Error{kind: SecretKeyMismatch}
			return callbackErr
		}
		if initialized {
			if len(dbID) != 32 || !equalKeyID(dbID, key.id[:]) {
				callbackErr = &Error{kind: SecretKeyMismatch}
				return callbackErr
			}
			return nil
		}
		if _, e = tx.Exec(ctx, `INSERT INTO public.secret_bootstrap(singleton, key_id) VALUES (true, $1)`, key.id[:]); e != nil {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		return nil
	})
	if outcome == TransactionIndeterminate {
		return nil, &Error{kind: SecretCommitUnknown}
	}
	if err != nil {
		if callbackErr != nil {
			return nil, callbackErr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{kind: SecretUnavailable}
	}
	if !key.valid() {
		return nil, &Error{kind: SecretKeyMismatch}
	}
	return &SecretRepository{store: s, key: key}, nil
}

func equalKeyID(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// OpenPrivateDataDir verifies the persistent operator-owned directory and
// returns a rooted handle for sibling private resources such as the Unix
// account recovery socket. The caller owns Close.
func OpenPrivateDataDir(path string) (*os.Root, error) { return openSecretRoot(path) }

func openSecretRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsRune(path, '\x00') {
		return nil, &Error{kind: SecretInvalid}
	}
	parentPath, base := filepath.Split(path)
	parentPath = filepath.Clean(parentPath)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0022 != 0 || !trustedParentOwner(parentInfo) {
		return nil, &Error{kind: SecretUnavailable}
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, &Error{kind: SecretUnavailable}
	}
	defer parent.Close()
	parentFD, err := parent.Open(".")
	if err != nil {
		return nil, &Error{kind: SecretUnavailable}
	}
	openedParent, err := parentFD.Stat()
	if err != nil || !os.SameFile(parentInfo, openedParent) {
		parentFD.Close()
		return nil, &Error{kind: SecretUnavailable}
	}
	if err := parent.Mkdir(base, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		parentFD.Close()
		return nil, &Error{kind: SecretUnavailable}
	}
	// Even a concurrent creator may not have synced the parent yet.
	if err := parentFD.Sync(); err != nil {
		parentFD.Close()
		return nil, &Error{kind: SecretUnavailable}
	}
	_ = parentFD.Close()
	info, err := parent.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return nil, &Error{kind: SecretUnavailable}
	}
	root, err := parent.OpenRoot(base)
	if err != nil {
		return nil, &Error{kind: SecretUnavailable}
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, &Error{kind: SecretUnavailable}
	}
	opened, err := dir.Stat()
	_ = dir.Close()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0700 || !ownedByCurrentUser(opened) {
		root.Close()
		return nil, &Error{kind: SecretUnavailable}
	}
	return root, nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func trustedParentOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == uint32(os.Geteuid()) || stat.Uid == 0)
}

func cleanupPendingFiles(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".env.pending-") {
			continue
		}
		token := strings.TrimPrefix(name, ".env.pending-")
		if len(token) != 32 {
			continue
		}
		if _, err := hex.DecodeString(token); err != nil {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info) {
			return &Error{kind: SecretUnavailable}
		}
		if err := root.Remove(name); err != nil {
			return &Error{kind: SecretUnavailable}
		}
		removed = true
	}
	if removed {
		if err := d.Sync(); err != nil {
			return &Error{kind: SecretUnavailable}
		}
	}
	return nil
}

func readMasterFile(root *os.Root) (masterKey, error) {
	f, err := root.OpenFile(secretFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return masterKey{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0400 == 0 || info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info) || info.Size() != int64(len(secretFilePrefix)+44+1) {
		return masterKey{}, &Error{kind: SecretKeyMismatch}
	}
	data, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return masterKey{}, &Error{kind: SecretKeyMismatch}
	}
	return parseMasterKey(data)
}

func publishMasterFile(root *os.Root, key masterKey) error {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return &Error{kind: SecretUnavailable}
	}
	tmp := ".env.pending-" + hex.EncodeToString(token[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	defer root.Remove(tmp)
	data := []byte(secretFilePrefix + base64.StdEncoding.EncodeToString(key.bytes[:]) + "\n")
	written, writeErr := f.Write(data)
	if written != len(data) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return &Error{kind: SecretUnavailable}
	}
	if err := root.Link(tmp, secretFile); err != nil {
		return &Error{kind: SecretUnavailable}
	}
	d, err := root.Open(".")
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	return nil
}

// RotateCredential replaces only one handle and advances its authenticated revision.
func (r *SecretRepository) RotateCredential(ctx context.Context, handle SecretHandle, value SecretValue) error {
	return r.rotateCredential(ctx, handle, value, nil, nil)
}

// InitializeCredentialIfAbsent is reserved for trusted server composition.
// It creates one encrypted revision under the same advisory lock as rotation,
// and never overwrites an existing credential during restart.
func (r *SecretRepository) InitializeCredentialIfAbsent(ctx context.Context, handle SecretHandle, value SecretValue) (bool, error) {
	inserted := false
	err := r.rotateCredential(ctx, handle, value, nil, &inserted)
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// RotateCredentialIfRevision refuses a stale refresh without overwriting a
// newer manual rotation. A commit whose outcome is unknown is never reported
// as a successful replacement.
func (r *SecretRepository) RotateCredentialIfRevision(ctx context.Context, handle SecretHandle, expected SecretRevision, value SecretValue) error {
	if !expected.validFor(handle) {
		return &Error{kind: SecretInvalid}
	}
	return r.rotateCredential(ctx, handle, value, &expected, nil)
}

func (r *SecretRepository) rotateCredential(ctx context.Context, handle SecretHandle, value SecretValue, expected *SecretRevision, initialized *bool) error {
	if ctx == nil || r == nil || r.store == nil || !r.key.valid() {
		return &Error{kind: SecretInvalid}
	}
	instance, name, err := handle.Components()
	if err != nil {
		return err
	}
	plain, err := value.Bytes()
	if err != nil {
		return err
	}
	i, _ := instance.Value()
	n, _ := name.Value()
	var callbackErr error
	outcome, err := r.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		// Advisory key includes the handle, so absent-row inserts serialize too.
		digest := sha256.Sum256([]byte(i + "\x00" + n))
		lock := int64(binary.BigEndian.Uint64(digest[:8]))
		if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lock); e != nil {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		var revision int64
		var oldKeyID, oldSalt, oldCiphertext []byte
		var oldSuite string
		e := tx.QueryRow(ctx, `SELECT revision,key_id,suite,salt,ciphertext FROM public.integration_secrets WHERE instance_id=$1 AND secret_name=$2 FOR UPDATE`, i, n).Scan(&revision, &oldKeyID, &oldSuite, &oldSalt, &oldCiphertext)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		if errors.Is(e, pgx.ErrNoRows) {
			revision = 0
		} else if parsed, parseErr := parseSecretCipherSuite(oldSuite); revision < 1 || parseErr != nil || parsed != secretCipherSuiteAES256GCMHKDFSHA256V1 || len(oldSalt) != 32 || !equalKeyID(oldKeyID, r.key.id[:]) {
			callbackErr = &Error{kind: SecretIntegrity}
			return callbackErr
		} else if _, e = r.open(handle, revision, oldSalt, oldCiphertext); e != nil {
			callbackErr = e
			return e
		}
		if initialized != nil && revision > 0 {
			return nil
		}
		if expected != nil && revision != expected.number {
			callbackErr = &Error{kind: SecretConflict}
			return callbackErr
		}
		if revision == int64(^uint64(0)>>1) {
			callbackErr = &Error{kind: SecretInvalid}
			return callbackErr
		}
		revision++
		var salt [32]byte
		if _, e = rand.Read(salt[:]); e != nil {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		ciphertext, e := r.seal(handle, revision, salt[:], plain)
		if e != nil {
			callbackErr = e
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO public.secret_salts(salt) VALUES ($1)`, salt[:]); e != nil {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		suiteSQL, _ := secretCipherSuiteAES256GCMHKDFSHA256V1.sqlValue()
		_, e = tx.Exec(ctx, `INSERT INTO public.integration_secrets(instance_id,secret_name,revision,key_id,suite,salt,ciphertext) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (instance_id,secret_name) DO UPDATE SET revision=EXCLUDED.revision,key_id=EXCLUDED.key_id,suite=EXCLUDED.suite,salt=EXCLUDED.salt,ciphertext=EXCLUDED.ciphertext,updated_at=clock_timestamp()`, i, n, revision, r.key.id[:], suiteSQL, salt[:], ciphertext)
		if e != nil {
			callbackErr = &Error{kind: SecretUnavailable}
			return callbackErr
		}
		if initialized != nil {
			*initialized = true
		}
		return nil
	})
	if outcome == TransactionIndeterminate {
		return &Error{kind: SecretCommitUnknown}
	}
	if err != nil {
		if callbackErr != nil {
			return callbackErr
		}
		return &Error{kind: SecretUnavailable}
	}
	return nil
}

func (r *SecretRepository) Read(ctx context.Context, handle SecretHandle) (SecretValue, error) {
	value, _, err := r.ReadCredentialVersioned(ctx, handle)
	return value, err
}

// ReadCredentialVersioned returns a secret and the authenticated revision
// observed in the same SQL row. The version is valid only for this handle.
func (r *SecretRepository) ReadCredentialVersioned(ctx context.Context, handle SecretHandle) (SecretValue, SecretRevision, error) {
	if ctx == nil || r == nil || r.store == nil || !r.key.valid() {
		return SecretValue{}, SecretRevision{}, &Error{kind: SecretInvalid}
	}
	iID, nID, err := handle.Components()
	if err != nil {
		return SecretValue{}, SecretRevision{}, err
	}
	i, _ := iID.Value()
	n, _ := nID.Value()
	var revision int64
	var keyID, salt, ciphertext []byte
	var suite string
	err = r.store.pool.QueryRow(ctx, `SELECT revision,key_id,suite,salt,ciphertext FROM public.integration_secrets WHERE instance_id=$1 AND secret_name=$2`, i, n).Scan(&revision, &keyID, &suite, &salt, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretValue{}, SecretRevision{}, &Error{kind: SecretNotFound}
	}
	if err != nil {
		return SecretValue{}, SecretRevision{}, &Error{kind: SecretUnavailable}
	}
	parsedSuite, parseErr := parseSecretCipherSuite(suite)
	if revision < 1 || parseErr != nil || parsedSuite != secretCipherSuiteAES256GCMHKDFSHA256V1 || len(salt) != 32 || !equalKeyID(keyID, r.key.id[:]) {
		return SecretValue{}, SecretRevision{}, &Error{kind: SecretIntegrity}
	}
	plain, err := r.open(handle, revision, salt, ciphertext)
	if err != nil {
		return SecretValue{}, SecretRevision{}, err
	}
	value, err := NewSecretValue(plain)
	if err != nil {
		return SecretValue{}, SecretRevision{}, err
	}
	return value, SecretRevision{handle: handle, number: revision}, nil
}

// protectedPurpose selects a fixed domain; suite and AEAD implementation remain unique.
type protectedPurpose uint8

const (
	protectedUnspecified protectedPurpose = iota
	protectedIntegrationCredential
	protectedMFACredential
	protectedMFACeremony
	protectedConfigSource
)

func (p protectedPurpose) domain() (string, error) {
	switch p {
	case protectedIntegrationCredential:
		return "homeward/integration-secret/aes256gcm/v1", nil
	case protectedMFACredential:
		return "homeward/mfa-credential/aes256gcm/v1", nil
	case protectedMFACeremony:
		return "homeward/mfa-ceremony/aes256gcm/v1", nil
	case protectedConfigSource:
		return "homeward/config-source/aes256gcm/v1", nil
	default:
		return "", &Error{kind: SecretInvalid}
	}
}
func protectedAEAD(key *masterKey, purpose protectedPurpose, salt []byte) (cipher.AEAD, error) {
	domain, err := purpose.domain()
	if err != nil || !key.valid() || len(salt) != 32 {
		return nil, &Error{kind: SecretInvalid}
	}
	derived, err := hkdf.Key(sha256.New, key.bytes[:], salt, domain, 32)
	if err != nil {
		return nil, &Error{kind: SecretUnavailable}
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, &Error{kind: SecretUnavailable}
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, &Error{kind: SecretUnavailable}
	}
	return aead, nil
}
func sealProtectedRecord(key *masterKey, purpose protectedPurpose, aad, salt, plain []byte) ([]byte, error) {
	aead, err := protectedAEAD(key, purpose, salt)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, plain, aad), nil
}
func openProtectedRecord(key *masterKey, purpose protectedPurpose, aad, salt, ciphertext []byte) ([]byte, error) {
	aead, err := protectedAEAD(key, purpose, salt)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nil, ciphertext, aad)
	if err != nil {
		return nil, &Error{kind: SecretIntegrity}
	}
	return plain, nil
}
func (r *SecretRepository) seal(handle SecretHandle, revision int64, salt, plain []byte) ([]byte, error) {
	return sealProtectedRecord(&r.key, protectedIntegrationCredential, secretAAD(handle, revision, r.key.id[:], salt), salt, plain)
}
func (r *SecretRepository) open(handle SecretHandle, revision int64, salt, ciphertext []byte) ([]byte, error) {
	return openProtectedRecord(&r.key, protectedIntegrationCredential, secretAAD(handle, revision, r.key.id[:], salt), salt, ciphertext)
}
func (r *SecretRepository) aead(salt []byte) (cipher.AEAD, error) {
	return protectedAEAD(&r.key, protectedIntegrationCredential, salt)
}

func secretAAD(handle SecretHandle, revision int64, keyID, salt []byte) []byte {
	i, n, _ := handle.Components()
	is, _ := i.Value()
	ns, _ := n.Value()
	var result []byte
	for _, part := range [][]byte{[]byte("homeward-secret-aad-v1"), []byte(secretSuiteSQL), keyID, []byte(is), []byte(ns), salt} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		result = append(result, size[:]...)
		result = append(result, part...)
	}
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], uint64(revision))
	return append(result, rev[:]...)
}

// AuditDenied persists a refusal; callers must fail closed if persistence fails.
type SecretDenial uint8

const (
	SecretDenialUnspecified SecretDenial = iota
	SecretDenialForeignHandle
	SecretDenialInactiveSession
)

func (reason SecretDenial) sqlValue() (string, error) {
	switch reason {
	case SecretDenialForeignHandle:
		return "foreign_handle", nil
	case SecretDenialInactiveSession:
		return "inactive_session", nil
	default:
		return "", &Error{kind: SecretInvalid}
	}
}

func (r *SecretRepository) AuditDenied(ctx context.Context, requester domain.IntegrationInstanceID, handle SecretHandle, reason SecretDenial) error {
	if ctx == nil || r == nil || r.store == nil {
		return &Error{kind: SecretInvalid}
	}
	req, err := requester.Value()
	if err != nil {
		return &Error{kind: SecretInvalid}
	}
	i, n, err := handle.Components()
	if err != nil {
		return err
	}
	target, _ := i.Value()
	name, _ := n.Value()
	sqlReason, err := reason.sqlValue()
	if err != nil {
		return err
	}
	_ = name // names are never persisted in denial audits.
	_, err = r.store.pool.Exec(ctx, `INSERT INTO public.secret_access_audit(requesting_instance,target_instance,reason) VALUES ($1,$2,$3)`, req, target, sqlReason)
	if err != nil {
		return &Error{kind: SecretUnavailable}
	}
	return nil
}

var _ = fmt.Stringer(SecretValue{})
