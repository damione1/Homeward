package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

func nha051Store(t *testing.T) (*Store, Settings) {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := ParseSettings(ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store, settings
}

func nha051Handle(t *testing.T, instance, name string) SecretHandle {
	t.Helper()
	i, err := domain.ParseIntegrationInstanceID(instance)
	if err != nil {
		t.Fatal(err)
	}
	n, err := ParseSecretName(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewSecretHandle(i, n)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func nha051Value(t *testing.T, value string) SecretValue {
	t.Helper()
	v, err := NewSecretValue([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func nha051Kind(t *testing.T, err error, kind Kind) {
	t.Helper()
	var storageErr *Error
	if !errors.As(err, &storageErr) || storageErr.Kind() != kind {
		t.Fatalf("kind: got %v want %v", err, kind)
	}
}

func TestNHA051ExistingPrivateDataDirectory(t *testing.T) {
	store, _ := nha051Store(t)
	if _, err := store.BootstrapSecrets(context.Background(), "/tmp/\x00data"); err == nil {
		t.Fatal("NUL data path admitted")
	} else {
		nha051Kind(t, err, SecretInvalid)
	}
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(filepath.Dir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapSecrets(context.Background(), dataDir); err != nil {
		t.Fatalf("existing data directory %q mode %o parent %q mode %o: %v", dataDir, info.Mode().Perm(), filepath.Dir(dataDir), parentInfo.Mode().Perm(), err)
	}
	if _, err := store.BootstrapSecrets(context.Background(), dataDir); err != nil {
		t.Fatalf("reopen existing directory: %v", err)
	}
}

func TestNHA051BootstrapRotationRestartAndEmptyStoreIdentity(t *testing.T) {
	ctx := context.Background()
	store, settings := nha051Store(t)
	dataDir := filepath.Join(t.TempDir(), "persistent-data")
	missingParent := filepath.Join(t.TempDir(), "missing", "data")
	if _, err := store.BootstrapSecrets(ctx, missingParent); err == nil {
		t.Fatal("missing parent silently created")
	}
	repo, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dataDir, secretFile)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("key file mode: %v %v", info, err)
	}
	dirInfo, err := os.Lstat(dataDir)
	if err != nil || dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("data dir mode: %v %v", dirInfo, err)
	}
	first := nha051Handle(t, "fake-instance-a", "api-token")
	second := nha051Handle(t, "fake-instance-b", "password")
	const canary = "NHA051_canary_token_abc123"
	if err := repo.RotateCredential(ctx, first, nha051Value(t, canary)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RotateCredential(ctx, second, nha051Value(t, "NHA051_canary_password_xyz")); err != nil {
		t.Fatal(err)
	}
	if err := repo.RotateCredential(ctx, first, nha051Value(t, "NHA051_rotated_456")); err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	var revision int64
	if err := store.pool.QueryRow(ctx, `SELECT ciphertext,revision FROM public.integration_secrets WHERE instance_id='fake-instance-a'`).Scan(&ciphertext, &revision); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || bytes.Contains(ciphertext, []byte(canary)) || bytes.Contains(ciphertext, []byte("NHA051_rotated_456")) {
		t.Fatal("plaintext or revision failure in SQL")
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repo, err = reopened.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := repo.Read(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := v.Bytes()
	if err != nil || string(plain) != "NHA051_rotated_456" {
		t.Fatalf("restart value invalid: %v", err)
	}
	plain[0] = 'X'
	again, _ := v.Bytes()
	if string(again) != "NHA051_rotated_456" {
		t.Fatal("SecretValue allowed caller mutation")
	}
	if _, err := reopened.pool.Exec(ctx, `DELETE FROM public.integration_secrets`); err != nil {
		t.Fatal(err)
	}
	var salts int
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM public.secret_salts`).Scan(&salts); err != nil || salts != 3 {
		t.Fatalf("historical salts lost: %d %v", salts, err)
	}
	if _, err := reopened.BootstrapSecrets(ctx, dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	_, err = reopened.BootstrapSecrets(ctx, dataDir)
	nha051Kind(t, err, SecretKeyMismatch)
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing key silently regenerated")
	}
}

func TestNHA051ConcurrentBootstrapAndDurableFileInterruption(t *testing.T) {
	ctx := context.Background()
	store, _ := nha051Store(t)
	dataDir := filepath.Join(t.TempDir(), "persistent-data")
	rootBefore, err := openSecretRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	const orphan = ".env.pending-0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dataDir, orphan), []byte("NHA051_unused_prepublication_key"), 0600); err != nil {
		t.Fatal(err)
	}
	rootBefore.Close() // State injection: private temp exists without a published key or SQL identity.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := store.BootstrapSecrets(ctx, dataDir); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dataDir, orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned private temporary key was retained")
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.secret_bootstrap`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bootstrap rows: %d %v", count, err)
	}
	repo, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := nha051Handle(t, "fake-instance-a", "guard-token")
	if err := repo.RotateCredential(ctx, h, nha051Value(t, "NHA051_guard_canary")); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := store.pool.QueryRow(ctx, `SELECT ciphertext FROM public.integration_secrets`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`TRUNCATE public.secret_bootstrap CASCADE`, `DELETE FROM public.secret_bootstrap`} {
		_, err := store.pool.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
			t.Fatalf("immutability guard missing: %s: %v", sql, err)
		}
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.secret_bootstrap`).Scan(&count); err != nil || count != 1 {
		t.Fatal("bootstrap identity changed after refused mutation")
	}
	var after []byte
	if err := store.pool.QueryRow(ctx, `SELECT ciphertext FROM public.integration_secrets`).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("guard failure changed ciphertext")
	}
	dataDir2 := filepath.Join(t.TempDir(), "interrupted-data")
	// A second disposable database models a restart from the durable-file state.
	other, _ := nha051Store(t)
	root, err := openSecretRoot(dataDir2)
	if err != nil {
		t.Fatal(err)
	}
	key, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := publishMasterFile(root, key); err != nil {
		t.Fatal(err)
	}
	root.Close() // State injection: durable link exists, SQL singleton has not been inserted.
	repo, err = other.BootstrapSecrets(ctx, dataDir2)
	if err != nil {
		t.Fatal(err)
	}
	if repo.key.id != key.id {
		t.Fatal("recovery generated another key")
	}
	var id []byte
	if err := other.pool.QueryRow(ctx, `SELECT key_id FROM public.secret_bootstrap`).Scan(&id); err != nil || !equalKeyID(id, key.id[:]) {
		t.Fatal("file adoption did not commit matching SQL identity")
	}
}

func TestNHA051WrongKeyPermissionsSymlinkAndTamper(t *testing.T) {
	ctx := context.Background()
	store, _ := nha051Store(t)
	dataDir := filepath.Join(t.TempDir(), "persistent-data")
	repo, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := nha051Handle(t, "fake-instance-a", "signed-url")
	const canary = "https://fake.invalid/?signature=NHA051_canary_signature"
	if err := repo.RotateCredential(ctx, h, nha051Value(t, canary)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dataDir, secretFile)
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := store.pool.QueryRow(ctx, `SELECT ciphertext FROM public.integration_secrets`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = store.BootstrapSecrets(ctx, dataDir)
	nha051Kind(t, err, SecretKeyMismatch)
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapSecrets(ctx, dataDir); err == nil {
		t.Fatal("public data directory admitted")
	}
	if err := os.Chmod(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0000); err != nil {
		t.Fatal(err)
	}
	_, err = store.BootstrapSecrets(ctx, dataDir)
	nha051Kind(t, err, SecretKeyMismatch)
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), file); err != nil {
		t.Fatal(err)
	}
	_, err = store.BootstrapSecrets(ctx, dataDir)
	nha051Kind(t, err, SecretKeyMismatch)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(file, 0600); err != nil {
		t.Fatal(err)
	}
	fifoDone := make(chan error, 1)
	go func() { _, err := store.BootstrapSecrets(ctx, dataDir); fifoDone <- err }()
	select {
	case err := <-fifoDone:
		nha051Kind(t, err, SecretKeyMismatch)
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO blocked key-file inspection")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	root, err := openSecretRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := publishMasterFile(root, wrong); err != nil {
		t.Fatal(err)
	}
	root.Close()
	_, err = store.BootstrapSecrets(ctx, dataDir)
	nha051Kind(t, err, SecretKeyMismatch)
	var after []byte
	if err := store.pool.QueryRow(ctx, `SELECT ciphertext FROM public.integration_secrets`).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed bootstrap changed ciphertext")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.integration_secrets SET ciphertext=set_byte(ciphertext, 13, get_byte(ciphertext,13) # 1)`); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Read(ctx, h)
	nha051Kind(t, err, SecretIntegrity)
	if err := repo.RotateCredential(ctx, h, nha051Value(t, "replacement")); err == nil {
		t.Fatal("rotation concealed corrupted old ciphertext")
	} else {
		nha051Kind(t, err, SecretIntegrity)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.integration_secrets SET ciphertext=$1,secret_name='other-name'`, before); err != nil {
		t.Fatal(err)
	}
	other := nha051Handle(t, "fake-instance-a", "other-name")
	_, err = repo.Read(ctx, other)
	nha051Kind(t, err, SecretIntegrity)
	for _, e := range []error{err, (&Error{kind: SecretKeyMismatch})} {
		if strings.Contains(fmt.Sprintf("%+v %#v", e, e), canary) {
			t.Fatal("error leaked secret")
		}
	}
	parked := dataDir + "-parked"
	if err := os.Rename(dataDir, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parked, dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapSecrets(ctx, dataDir); err == nil {
		t.Fatal("symlinked data directory admitted")
	}
}

func TestNHA051RedactionZeroAndSchemaDrift(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	var suite, reason string
	if err := store.pool.QueryRow(ctx, `SELECT string_agg(enumlabel,',' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid='public.secret_cipher_suite'::regtype`).Scan(&suite); err != nil {
		t.Fatal(err)
	}
	if suite != secretSuiteSQL {
		t.Fatalf("SQL suite drift: %s", suite)
	}
	if err := store.pool.QueryRow(ctx, `SELECT string_agg(enumlabel,',' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid='public.secret_audit_reason'::regtype`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	foreign, _ := SecretDenialForeignHandle.sqlValue()
	inactive, _ := SecretDenialInactiveSession.sqlValue()
	if reason != foreign+","+inactive {
		t.Fatalf("SQL denial drift: %s", reason)
	}
	if _, err := (SecretName{}).Value(); err == nil {
		t.Fatal("zero name")
	}
	if _, _, err := (SecretHandle{}).Components(); err == nil {
		t.Fatal("zero handle")
	}
	var zero SecretValue
	if _, err := zero.Bytes(); err == nil {
		t.Fatal("zero value")
	}
	var typedNil *SecretValue
	if _, err := typedNil.Bytes(); err == nil {
		t.Fatal("typed nil value")
	}
	if _, err := NewSecretValue(nil); err == nil {
		t.Fatal("nil value")
	}
	if (&SecretRepository{}).Valid() || (*SecretRepository)(nil).Valid() {
		t.Fatal("zero/typed nil repository accepted")
	}
	var nilKey *masterKey
	var zeroKey masterKey
	if zeroKey.valid() || nilKey.valid() {
		t.Fatal("zero/typed nil key accepted")
	}
	var nilErr *Error
	if nilErr.Kind() != 0 || nilErr.Unwrap() != nil || nilErr.Error() == "" {
		t.Fatal("typed nil error is not safe")
	}
	if _, err := (secretCipherSuiteUnspecified).sqlValue(); err == nil {
		t.Fatal("zero suite accepted")
	}
	if _, err := parseSecretCipherSuite("future_suite"); err == nil {
		t.Fatal("future suite accepted")
	}
	const canary = "NHA051_private_password_and_token"
	v := nha051Value(t, canary)
	h := nha051Handle(t, "fake-instance-a", canary)
	n, _ := ParseSecretName(canary)
	for _, thing := range []any{v, h, n} {
		encoded, err := json.Marshal(thing)
		if err != nil {
			t.Fatal(err)
		}
		for _, rendered := range []string{fmt.Sprintf("%v", thing), fmt.Sprintf("%+v", thing), fmt.Sprintf("%#v", thing), string(encoded)} {
			if strings.Contains(rendered, canary) {
				t.Fatalf("rendered canary: %s", rendered)
			}
		}
	}
}
