package storage

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/jackc/pgx/v5"
)

// This is the real storage CAS boundary. Owner.Request cannot contain an
// independently supplied expected hash; this test does not add that seam.
func TestNHA029ActiveCASRejectsNonzeroWrongManifestBeforeWrites(t *testing.T) {
	f := nha029HistoryFixture(t, 96, nil)
	store, ctx := f.store, f.ctx
	observer, err := pgx.ConnectConfig(ctx, store.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != f.active.generation || active.ManifestSHA256() != f.active.manifest {
		t.Fatalf("nonzero CAS fixture lacks exact SQL authority: present=%t err=%v", present, err)
	}
	// C is a real complete, sealed candidate newer than active B. A later
	// positive control activates this very target with the correct B pair.
	raw := nha029HistoryRaw(96, 'C')
	defer clear(raw)
	var target ConfigGenerationRevision
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		ref, err := tx.SaveProtectedOperationalSource(f.replay, f.binding, raw, sha256.Sum256(raw), nha151Metadata(t))
		if err != nil {
			return err
		}
		target, err = tx.RegisterCompiledGeneration(f.binding.Path(), sha256.Sum256(raw), []ConfigVersionRef{ref})
		return err
	})
	if err != nil || outcome != TransactionCommitted || target.Number() <= active.Revision().Number() {
		t.Fatalf("nonzero CAS target was not durably prepared: outcome=%v err=%v", outcome, err)
	}
	wanted, err := store.ReadConfigGeneration(ctx, target)
	if err != nil || wanted.Availability() != ConfigGenerationComplete || wanted.ManifestFormat() != ConfigManifestCompiledV2 {
		t.Fatalf("nonzero CAS target is not admissible: %v", err)
	}
	// This fixture-only sequence is nontransactional: even a pointer UPDATE
	// followed by rollback would leave is_called=true. The trigger changes no
	// product decision and is installed only in this owned disposable cluster.
	if _, err := observer.Exec(ctx, `CREATE SEQUENCE public.nha029_cas_pointer_updates START WITH 1;
CREATE FUNCTION public.nha029_count_cas_pointer_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM nextval('public.nha029_cas_pointer_updates'); RETURN NEW; END $$;
CREATE TRIGGER nha029_count_cas_pointer_update BEFORE UPDATE ON public.config_active
FOR EACH ROW EXECUTE FUNCTION public.nha029_count_cas_pointer_update()`); err != nil {
		t.Fatal(err)
	}
	fingerprint := func() [sha256.Size]byte {
		t.Helper()
		var digest []byte
		if err := observer.QueryRow(ctx, `SELECT sha256(convert_to(jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY x.revision) FROM public.config_generations x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY x.generation_revision,x.document_id) FROM public.config_generation_entries x),
 (SELECT jsonb_agg(to_jsonb(x)) FROM public.config_active x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY x.attempt_id) FROM public.config_activation_attempts x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY x.attempt_id,x.account_id) FROM public.config_activation_policy_changes x)
 )::text,'UTF8'))`).Scan(&digest); err != nil || len(digest) != sha256.Size {
			t.Fatalf("independent CAS fingerprint: %v", err)
		}
		return [sha256.Size]byte(digest)
	}
	before := fingerprint()
	wrong := [sha256.Size]byte{1}
	if wrong == active.ManifestSHA256() {
		wrong[0] = 2
	}
	expected, err := ExpectConfigActive(active.Revision(), wrong)
	if err != nil || wrong == ([sha256.Size]byte{}) || wrong == active.ManifestSHA256() {
		t.Fatalf("wrong nonzero hash was not a valid independent expectation: %v", err)
	}
	attempt, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	metadata := nha151Metadata(t)
	outcome, err = store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		if err := tx.ActivateGeneration(expected, target); err != nil {
			return err
		}
		return tx.RecordActivationAttempt(attempt, metadata)
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("right revision/wrong nonzero manifest did not conflict: outcome=%v err=%v", outcome, err)
	}
	// The transaction boundary preserves the specific CAS refusal as the
	// cause of its redacted TransactionFailed envelope. Check both levels.
	nha151Kind(t, err, TransactionFailed)
	nha151Kind(t, err, ConfigConflict)
	var called bool
	var updates int64
	if err := observer.QueryRow(ctx, `SELECT last_value,is_called FROM public.nha029_cas_pointer_updates`).Scan(&updates, &called); err != nil || called {
		t.Fatalf("hash conflict attempted a pointer write even though transaction rolled back: updates=%d called=%t err=%v", updates, called, err)
	}
	after, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || after.Revision() != active.Revision() || after.ManifestSHA256() != active.ManifestSHA256() || fingerprint() != before {
		t.Fatalf("hash conflict changed SQL authority/manifest/audits: present=%t err=%v", present, err)
	}
	if _, present, err := store.ReadActivationAttempt(ctx, attempt); err != nil || present {
		t.Fatalf("hash conflict fabricated a success audit: present=%t err=%v", present, err)
	}
	correct, err := ExpectConfigActive(active.Revision(), active.ManifestSHA256())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		if err := tx.ActivateGeneration(correct, target); err != nil {
			return err
		}
		return tx.RecordActivationAttempt(attempt, metadata)
	})
	if err != nil || outcome != TransactionCommitted {
		t.Fatalf("same target with correct hash was not activatable: outcome=%v err=%v", outcome, err)
	}
	if err := observer.QueryRow(ctx, `SELECT last_value,is_called FROM public.nha029_cas_pointer_updates`).Scan(&updates, &called); err != nil || !called || updates != 1 {
		t.Fatalf("positive control did not observe exactly one real pointer write: updates=%d called=%t err=%v", updates, called, err)
	}
	control, present, err := store.ReadActivationAttempt(ctx, attempt)
	if err != nil || !present || control.Target() != target || control.ManifestSHA256() != wanted.ManifestSHA256() {
		t.Fatalf("positive control did not commit the real target audit: present=%t err=%v", present, err)
	}
}
