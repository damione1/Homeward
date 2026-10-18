package storage

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA091InitialSessionEncryptedAndPreserved(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "private")
	repo, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha091-neviweb")
	name, _ := ParseSecretName("session")
	handle, _ := NewSecretHandle(instance, name)
	initial, _ := NewSecretValue([]byte(`{"version":1,"state":"absent"}`))
	var created atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			inserted, e := repo.InitializeCredentialIfAbsent(ctx, handle, initial)
			if e != nil {
				t.Error(e)
			}
			if inserted {
				created.Add(1)
			}
		}()
	}
	group.Wait()
	if created.Load() != 1 {
		t.Fatalf("conditional inserts=%d", created.Load())
	}
	value, revision, err := repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := value.Bytes()
	if string(raw) != `{"version":1,"state":"absent"}` {
		t.Fatal("unexpected initial state")
	}
	var persisted []byte
	if err := store.pool.QueryRow(ctx, `SELECT ciphertext FROM public.integration_secrets WHERE instance_id=$1 AND secret_name=$2`, "nha091-neviweb", "session").Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if string(persisted) == string(raw) {
		t.Fatal("session persisted in plaintext")
	}
	active, _ := NewSecretValue([]byte(`{"version":1,"state":"active","session_id":"local-test"}`))
	if err := repo.RotateCredentialIfRevision(ctx, handle, revision, active); err != nil {
		t.Fatal(err)
	}
	if inserted, err := repo.InitializeCredentialIfAbsent(ctx, handle, initial); err != nil || inserted {
		t.Fatalf("restart overwrote active session: %v %v", inserted, err)
	}
	stored, _, err := repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := stored.Bytes()
	if string(body) != `{"version":1,"state":"active","session_id":"local-test"}` {
		t.Fatal("active session changed")
	}
}

func TestNHA091QuotaConcurrentAndDurable(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	quota, err := NewCloudAttemptQuota(store, "fixture-account", 5, "America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	var admitted, denied atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			retry, e := quota.ReserveAttempt(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			if retry.IsZero() {
				admitted.Add(1)
			} else {
				denied.Add(1)
			}
		}()
	}
	group.Wait()
	if admitted.Load() != 5 || denied.Load() != 15 {
		t.Fatalf("admitted/denied=%d/%d", admitted.Load(), denied.Load())
	}
	count, err := quota.Today(ctx)
	if err != nil || count != 5 {
		t.Fatalf("today=%d %v", count, err)
	}
	store.Close()
	reopened, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := NewCloudAttemptQuota(reopened, "fixture-account", 5, "America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	if retry, e := again.ReserveAttempt(ctx); e != nil || retry.IsZero() {
		t.Fatalf("reopen admitted exhausted quota: %v %v", retry, e)
	}
	if _, err := NewCloudAttemptQuota(reopened, "fixture-account", 5, "Local"); err == nil {
		t.Fatal("host-dependent Local timezone admitted")
	}
	if _, err := NewCloudAttemptQuota(reopened, "fixture-account", 30001, "UTC"); err == nil {
		t.Fatal("over-cap budget admitted")
	}
	if _, err := again.ReserveAttempt(nil); err == nil {
		t.Fatal("nil context admitted")
	}
}
