package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNHA096VersionedSecretCASManualConflictAndReopen(t *testing.T) {
	ctx := context.Background()
	store, settings := nha051Store(t)
	dataDir := filepath.Join(t.TempDir(), "private-data")
	repo, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051Handle(t, "nha096-instance", "oauth-token")
	const firstCanary = "NHA096_first_private_token"
	const manualCanary = "NHA096_manual_private_token"
	if err := repo.RotateCredential(ctx, handle, nha051Value(t, firstCanary)); err != nil {
		t.Fatal(err)
	}
	first, version, err := repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := first.Bytes(); err != nil || string(raw) != firstCanary {
		t.Fatal("versioned read returned wrong credential")
	}
	if err := repo.RotateCredential(ctx, handle, nha051Value(t, manualCanary)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RotateCredentialIfRevision(ctx, handle, version, nha051Value(t, "NHA096_stale_private_token")); err == nil {
		t.Fatal("stale refresh overwrote manual rotation")
	} else {
		nha051Kind(t, err, SecretConflict)
	}
	if err := repo.RotateCredentialIfRevision(ctx, handle, SecretRevision{}, nha051Value(t, "invalid")); err == nil {
		t.Fatal("zero revision admitted")
	} else {
		nha051Kind(t, err, SecretInvalid)
	}
	other := nha051Handle(t, "nha096-instance", "other-oauth-token")
	if err := repo.RotateCredentialIfRevision(ctx, other, version, nha051Value(t, "invalid")); err == nil {
		t.Fatal("foreign revision admitted")
	} else {
		nha051Kind(t, err, SecretInvalid)
	}
	var revision int64
	var ciphertext []byte
	if err := store.pool.QueryRow(ctx, `SELECT revision,ciphertext FROM public.integration_secrets WHERE instance_id='nha096-instance' AND secret_name='oauth-token'`).Scan(&revision, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || bytes.Contains(ciphertext, []byte(manualCanary)) || bytes.Contains(ciphertext, []byte(firstCanary)) {
		t.Fatal("manual version or encryption changed after stale CAS")
	}
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	secondRepo, err := reopened.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	value, secondVersion, err := secondRepo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := value.Bytes()
	if err != nil || string(raw) != manualCanary || secondVersion == version {
		t.Fatal("reopened repository did not retain newer version")
	}
	if !secondVersion.NewerThan(version) || version.NewerThan(secondVersion) || (SecretRevision{}).NewerThan(version) {
		t.Fatal("opaque revision ordering failed")
	}
	if err := secondRepo.RotateCredential(ctx, other, nha051Value(t, "NHA096_other_handle")); err != nil {
		t.Fatal(err)
	}
	_, foreignVersion, err := secondRepo.ReadCredentialVersioned(ctx, other)
	if err != nil || foreignVersion.NewerThan(version) || secondVersion.NewerThan(foreignVersion) {
		t.Fatal("opaque revision ordering crossed handles")
	}
	if err := secondRepo.RotateCredentialIfRevision(ctx, handle, secondVersion, nha051Value(t, "NHA096_final_private_token")); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT revision FROM public.integration_secrets WHERE instance_id='nha096-instance' AND secret_name='oauth-token'`).Scan(&revision); err != nil || revision != 3 {
		t.Fatalf("conditional rotation did not advance revision: %d %v", revision, err)
	}
}

func TestNHA096ConcurrentCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	store, _ := nha051Store(t)
	repo, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051Handle(t, "nha096-instance", "singleflight-token")
	if err := repo.RotateCredential(ctx, handle, nha051Value(t, "NHA096_initial")); err != nil {
		t.Fatal(err)
	}
	_, version, err := repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, value := range []string{"NHA096_winner_a", "NHA096_winner_b"} {
		wg.Add(1)
		go func(v string) {
			defer wg.Done()
			secret, _ := NewSecretValue([]byte(v))
			results <- repo.RotateCredentialIfRevision(ctx, handle, version, secret)
		}(value)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var storageErr *Error
		if errors.As(err, &storageErr) && storageErr.Kind() == SecretConflict {
			conflicts++
		} else {
			t.Fatalf("unexpected CAS outcome: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS outcomes: success=%d conflict=%d", successes, conflicts)
	}
}

func TestNHA096CASCommitUnknownAndRedaction(t *testing.T) {
	ctx := context.Background()
	store, _ := nha051Store(t)
	repo, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051Handle(t, "nha096-instance", "commit-token")
	const canary = "NHA096_commit_private_canary"
	if err := repo.RotateCredential(ctx, handle, nha051Value(t, canary)); err != nil {
		t.Fatal(err)
	}
	_, version, err := repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE FUNCTION public.nha096_refuse_secret_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'deferred synthetic failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha096_refuse_secret_commit AFTER UPDATE ON public.integration_secrets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha096_refuse_secret_commit()`); err != nil {
		t.Fatal(err)
	}
	commitErr := repo.RotateCredentialIfRevision(ctx, handle, version, nha051Value(t, "NHA096_unconfirmed_private_canary"))
	nha051Kind(t, commitErr, SecretCommitUnknown)
	value, latest, err := repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := value.Bytes()
	if err != nil || string(raw) != canary || latest != version {
		t.Fatal("failed deferred commit was reported as a durable rotation")
	}
	for _, thing := range []any{version, commitErr, value} {
		encoded, marshalErr := json.Marshal(thing)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, rendering := range []string{fmt.Sprintf("%v", thing), fmt.Sprintf("%+v", thing), fmt.Sprintf("%#v", thing), string(encoded)} {
			if strings.Contains(rendering, canary) || strings.Contains(rendering, "NHA096_unconfirmed_private_canary") {
				t.Fatal("secret leaked through a diagnostic representation")
			}
		}
	}
}
