package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// An active generation is identified by both its revision and manifest hash.
// A revision-only expectation must not be able to publish another generation.
func TestNHA029ActiveCASRejectsMissingManifestHash(t *testing.T) {
	store, ctx := nha151Store(t)
	binding := nha151Binding(t, "home.config", "configuration.yaml")
	first, _ := nha151Candidate(t, binding.Path(), "first")
	_, activeRevision := nha151Write(t, store, ctx, first, binding, ExpectNoConfigActive())
	second, _ := nha151Candidate(t, binding.Path(), "second")
	_, nextRevision := nha151Write(t, store, ctx, second, binding, ExpectedConfigActive{})

	// This constructs the currently admitted revision-only expectation. Once
	// NHA-029 adds the manifest hash, its absent zero value must be rejected.
	revisionOnly := ExpectedConfigActive{
		kind:     configActiveExpectationRevision,
		revision: activeRevision,
	}
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		return tx.ActivateGeneration(revisionOnly, nextRevision)
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("revision-only activation was accepted: outcome=%v err=%v", outcome, err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != activeRevision {
		t.Fatalf("rejected CAS changed active generation: present=%t revision=%v err=%v", present, active.Revision(), err)
	}
}

func TestNHA029ProtectedOperationalExactSourceAndRedactedReplay(t *testing.T) {
	store, ctx := nha151Store(t)
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	binding := nha151Binding(t, "operational.main", "configuration.yaml")
	raw := []byte("# NHA029 canary: exact comment and quoted args\nexternal_workers:\n  - name: work\n    args: [\"--opaque=canary\"]\n")
	hash := sha256.Sum256(raw)
	attemptID, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	var ref ConfigVersionRef
	var generation ConfigGenerationRevision
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var e error
		ref, e = tx.SaveProtectedOperationalSource(replay, binding, raw, hash, nha151Metadata(t))
		if e != nil {
			return e
		}
		generation, e = tx.RegisterCompiledGeneration(binding.Path(), sha256.Sum256([]byte("compiled-program")), []ConfigVersionRef{ref})
		if e != nil {
			return e
		}
		if e = tx.ActivateGeneration(ExpectNoConfigActive(), generation); e != nil {
			return e
		}
		return tx.RecordActivationAttempt(attemptID, nha151Metadata(t))
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("protected activation outcome=%v err=%v", outcome, err)
	}
	committedAttempt, found, err := store.ReadActivationAttempt(ctx, attemptID)
	if err != nil || !found || committedAttempt.Target() != generation || committedAttempt.Expected().kind != configActiveExpectationAbsent {
		t.Fatalf("activation attempt missing on independent connection: found=%t err=%v", found, err)
	}
	unknownID, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ReadActivationAttempt(ctx, unknownID); err != nil || found {
		t.Fatalf("absent attempt did not reconcile as absent: found=%t err=%v", found, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.config_activation_attempts SET author='forged' WHERE attempt_id=$1`, attemptID.value[:]); err == nil {
		t.Fatal("committed activation audit was mutable")
	} else {
		nha151SQLState(t, err, "23514")
	}
	var plain, ciphertext []byte
	if err := store.pool.QueryRow(ctx, `SELECT yaml_source,ciphertext FROM public.config_versions WHERE document_id=$1 AND revision=$2`,
		ref.Document().String(), ref.Revision().Number()).Scan(&plain, &ciphertext); err != nil || plain != nil ||
		len(ciphertext) != len(raw)+28 || bytes.Contains(ciphertext, []byte("canary")) {
		t.Fatalf("protected SQL source leaked or malformed: %v", err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != generation || len(active.Versions()) != 1 {
		t.Fatalf("generic active: %t %v", present, err)
	}
	version := active.Versions()[0]
	if version.SHA256() != hash || !version.Present() {
		t.Fatal("protected metadata missing")
	}
	if rawFromGeneric, ok := version.Bytes(); ok || rawFromGeneric != nil {
		t.Fatal("generic reader exposed protected bytes")
	}
	if _, err := active.Restore(); err == nil {
		t.Fatal("legacy restore admitted a protected generation")
	} else {
		nha151Kind(t, err, ConfigProtected)
	}
	opened, err := replay.ReadActiveProtectedSource(ctx, generation, active.ManifestSHA256(), ref)
	if err != nil || !bytes.Equal(opened.Bytes(), raw) || opened.SHA256() != hash {
		t.Fatalf("protected replay failed: %v", err)
	}
	openedBytes := opened.Bytes()
	openedBytes[0] ^= 1
	if !bytes.Equal(opened.Bytes(), raw) || bytes.Contains([]byte(fmt.Sprintf("%+v", opened)), raw) {
		t.Fatal("protected replay alias or diagnostic leak")
	}
	wrongManifest := sha256.Sum256([]byte("wrong manifest"))
	if _, err := replay.ReadActiveProtectedSource(ctx, generation, wrongManifest, ref); err == nil {
		t.Fatal("wrong active manifest opened protected source")
	}
	if _, err := replay.ReadActiveProtectedSource(ctx, generation, active.ManifestSHA256(), ConfigVersionRef{}); err == nil {
		t.Fatal("zero source reference opened")
	}
}

func TestNHA029ProtectedOperationalAADRejectsCrossBindingAndTamper(t *testing.T) {
	key, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	other, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("# exact source with comment and --opaque=canary\n")
	hash := sha256.Sum256(raw)
	salt := bytes.Repeat([]byte{7}, 32)
	aad := configSourceAAD("operational.main", "configuration.yaml", 1, hash, key.id[:], salt)
	ciphertext, err := sealProtectedRecord(&key, protectedConfigSource, aad, salt, raw)
	if err != nil || len(ciphertext) != len(raw)+28 {
		t.Fatalf("config source seal: %v", err)
	}
	opened, err := openProtectedRecord(&key, protectedConfigSource, aad, salt, ciphertext)
	if err != nil || !bytes.Equal(opened, raw) {
		t.Fatalf("config source open: %v", err)
	}
	otherHash := sha256.Sum256([]byte("different"))
	for _, changedAAD := range [][]byte{
		configSourceAAD("operational.other", "configuration.yaml", 1, hash, key.id[:], salt),
		configSourceAAD("operational.main", "other.yaml", 1, hash, key.id[:], salt),
		configSourceAAD("operational.main", "configuration.yaml", 2, hash, key.id[:], salt),
		configSourceAAD("operational.main", "configuration.yaml", 1, otherHash, key.id[:], salt),
		configSourceAAD("operational.main", "configuration.yaml", 1, hash, other.id[:], salt),
		configSourceAAD("operational.main", "configuration.yaml", 1, hash, key.id[:], bytes.Repeat([]byte{8}, 32)),
	} {
		if _, err := openProtectedRecord(&key, protectedConfigSource, changedAAD, salt, ciphertext); err == nil {
			t.Fatal("cross-binding source opened")
		}
	}
	if _, err := openProtectedRecord(&other, protectedConfigSource, aad, salt, ciphertext); err == nil {
		t.Fatal("wrong 027 master key opened source")
	}
	if _, err := openProtectedRecord(&key, protectedIntegrationCredential, aad, salt, ciphertext); err == nil {
		t.Fatal("integration-secret AEAD domain opened configuration source")
	}
	ciphertext[15] ^= 1
	if _, err := openProtectedRecord(&key, protectedConfigSource, aad, salt, ciphertext); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
}

func TestNHA029LegacyActiveUpgradesWithSameLogicalPath(t *testing.T) {
	store, ctx := nha151Store(t)
	legacyBinding := nha151Binding(t, "legacy.configuration", "configuration.yaml")
	candidate, legacyBytes := nha151Candidate(t, legacyBinding.Path(), "before-v2")
	legacyRef, legacyGeneration := nha151Write(t, store, ctx, candidate, legacyBinding, ExpectNoConfigActive())
	legacyBefore, err := store.ReadConfigGeneration(ctx, legacyGeneration)
	if err != nil || legacyBefore.ManifestFormat() != ConfigManifestLegacyV1 {
		t.Fatalf("legacy generation before upgrade: %v", err)
	}
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	operationalBinding := nha151Binding(t, "operational.configuration", legacyBinding.Path())
	raw := []byte("# retained exact operational source\nexternal_workers: []\n")
	hash := sha256.Sum256(raw)
	var operationalRef ConfigVersionRef
	var upgraded ConfigGenerationRevision
	attemptID, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectConfigActive(legacyGeneration, legacyBefore.ManifestSHA256())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var e error
		operationalRef, e = tx.SaveProtectedOperationalSource(replay, operationalBinding, raw, hash, nha151Metadata(t))
		if e != nil {
			return e
		}
		upgraded, e = tx.RegisterCompiledGeneration(legacyBinding.Path(), candidate.SHA256(), []ConfigVersionRef{legacyRef, operationalRef})
		if e != nil {
			return e
		}
		if e = tx.ActivateGeneration(expected, upgraded); e != nil {
			return e
		}
		return tx.RecordActivationAttempt(attemptID, nha151Metadata(t))
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("same-path v1→v2 transaction: outcome=%v err=%v", outcome, err)
	}
	historical, err := store.ReadConfigGeneration(ctx, legacyGeneration)
	if err != nil || historical.ManifestSHA256() != legacyBefore.ManifestSHA256() {
		t.Fatalf("legacy manifest changed after upgrade: %v", err)
	}
	restored, err := historical.Restore()
	if err != nil || restored.SHA256() != candidate.SHA256() ||
		len(historical.Versions()) != 1 {
		t.Fatalf("legacy source no longer restores: %v", err)
	}
	historicalBytes, present := historical.Versions()[0].Bytes()
	if !present || !bytes.Equal(historicalBytes, legacyBytes) {
		t.Fatal("legacy source bytes changed")
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != upgraded || active.ManifestFormat() != ConfigManifestCompiledV2 || len(active.Versions()) != 2 {
		t.Fatalf("upgraded active manifest: present=%t err=%v", present, err)
	}
	var foundLegacy, foundOperational bool
	for _, version := range active.Versions() {
		if version.Path() != "configuration.yaml" {
			t.Fatal("same-path source was silently reassigned")
		}
		switch version.Ref().Document() {
		case legacyRef.Document():
			foundLegacy = true
			if data, ok := version.Bytes(); !ok || !bytes.Equal(data, legacyBytes) {
				t.Fatal("v1 document in v2 manifest lost exact source")
			}
		case operationalRef.Document():
			foundOperational = true
			if data, ok := version.Bytes(); ok || data != nil {
				t.Fatal("operational document leaked through generic reader")
			}
		default:
			t.Fatal("unexpected manifest document")
		}
	}
	if !foundLegacy || !foundOperational {
		t.Fatal("manifest conflated two document identities at the same logical path")
	}
	opened, err := replay.ReadActiveProtectedSource(ctx, upgraded, active.ManifestSHA256(), operationalRef)
	if err != nil || !bytes.Equal(opened.Bytes(), raw) {
		t.Fatalf("protected source after same-path upgrade: %v", err)
	}
}

func TestNHA029BootstrapSourceCASAndTwentyVersionRetention(t *testing.T) {
	store, ctx := nha151Store(t)
	binding := nha151Binding(t, "bootstrap.launch", "configuration.yaml.bootstrap.yaml")
	metadata := nha151Metadata(t)
	firstRaw := []byte("listen: 127.0.0.1:8123\n# exact launch sidecar\n")
	firstHash := sha256.Sum256(firstRaw)
	var first ConfigVersionRef
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var e error
		first, e = tx.SaveBootstrapLaunchSource(binding, firstRaw, firstHash, metadata)
		if e != nil {
			return e
		}
		return tx.ActivateBootstrapSource(ExpectNoBootstrapActive(), first)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("initial bootstrap pointer: outcome=%v err=%v", outcome, err)
	}
	initial, present, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !present || initial.Ref() != first || initial.SHA256() != firstHash || !bytes.Equal(initial.Bytes(), firstRaw) {
		t.Fatalf("bootstrap exact read: present=%t err=%v", present, err)
	}
	wanted, err := ExpectBootstrapActive(first, firstHash)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := ExpectBootstrapActive(first, sha256.Sum256([]byte("wrong")))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		raw := []byte("listen: 127.0.0.1:8124\n")
		ref, e := tx.SaveBootstrapLaunchSource(binding, raw, sha256.Sum256(raw), metadata)
		if e != nil {
			return e
		}
		return tx.ActivateBootstrapSource(wrong, ref)
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("wrong bootstrap hash accepted: outcome=%v err=%v", outcome, err)
	}
	still, present, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !present || still.Ref() != first {
		t.Fatalf("failed CAS changed bootstrap: present=%t err=%v", present, err)
	}
	var newest ConfigVersionRef
	for i := 2; i <= 21; i++ {
		raw := []byte(fmt.Sprintf("listen: 127.0.0.1:%d\n", 8100+i))
		hash := sha256.Sum256(raw)
		outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
			var e error
			newest, e = tx.SaveBootstrapLaunchSource(binding, raw, hash, metadata)
			return e
		})
		if outcome != TransactionCommitted || err != nil {
			t.Fatalf("bootstrap version %d: outcome=%v err=%v", i, outcome, err)
		}
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_versions WHERE document_id=$1`, binding.ID().String()).Scan(&count); err != nil || count != configVersionLimit {
		t.Fatalf("bootstrap retention: count=%d err=%v", count, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_versions WHERE document_id=$1 AND revision=1`, binding.ID().String()).Scan(&count); err != nil || count != 1 {
		t.Fatal("active bootstrap version was collected")
	}
	outcome, err = store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		return tx.ActivateBootstrapSource(wanted, newest)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("bootstrap CAS to newest: outcome=%v err=%v", outcome, err)
	}
	after, present, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !present || after.Ref() != newest || after.SHA256() == firstHash {
		t.Fatalf("bootstrap pointer did not advance: present=%t err=%v", present, err)
	}
}

func TestNHA029CompiledActivationWithoutAttemptRollsBack(t *testing.T) {
	store, ctx := nha151Store(t)
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	binding := nha151Binding(t, "operational.main", "configuration.yaml")
	raw := []byte("# no success audit means no active generation\n")
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		ref, e := tx.SaveProtectedOperationalSource(replay, binding, raw, sha256.Sum256(raw), nha151Metadata(t))
		if e != nil {
			return e
		}
		generation, e := tx.RegisterCompiledGeneration(binding.Path(), sha256.Sum256([]byte("compiled")), []ConfigVersionRef{ref})
		if e != nil {
			return e
		}
		return tx.ActivateGeneration(ExpectNoConfigActive(), generation)
	})
	if outcome != TransactionNotCommitted || err == nil {
		t.Fatalf("v2 activation without audit committed: outcome=%v err=%v", outcome, err)
	}
	if active, present, err := store.ReadActiveConfig(ctx); err != nil || present || active.Revision().Valid() {
		t.Fatalf("auditless activation changed SQL active: present=%t err=%v", present, err)
	}
}

func TestNHA029InternalCommandOriginCannotCollideWithExternalActor(t *testing.T) {
	store, ctx := nha151Store(t)
	commandID, _ := domain.ParseCommandID("nha029-internal-command")
	actor, _ := domain.ParseActorID("nha029-external-user")
	entity, _ := domain.ParseEntityID("nha029-test-entity")
	slot, err := authorization.NewCapabilitySlot("core.switch.v1", "main")
	if err != nil {
		t.Fatal(err)
	}
	target, err := authorization.NewCommandTarget(entity, slot)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewInternalAutomationOriginKey("automation.kitchen/run-1/generation-7/ordinal-2")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := NewInternalAutomationFingerprint(key, []byte("canonical-target-and-parameters"))
	if err != nil {
		t.Fatal(err)
	}
	intent, created, outcome, err := store.AcceptInternalAutomationIntent(ctx, commandID, key, "nha029-epoch", fingerprint, target)
	if err != nil || !created || outcome != TransactionCommitted || intent.OriginKind() != IntentOriginInternalAutomation {
		t.Fatalf("internal origin admission: created=%t outcome=%v err=%v", created, outcome, err)
	}
	if exact, present := intent.InternalOriginKey(); !present || exact != key {
		t.Fatal("internal occurrence identity lost")
	}
	var actorID, originKey *string
	var originKind string
	if err := store.pool.QueryRow(ctx, `SELECT actor_id,origin_key,origin_kind::text FROM public.command_intents WHERE command_id=$1`,
		"nha029-internal-command").Scan(&actorID, &originKey, &originKind); err != nil || actorID != nil || originKey == nil || *originKey != key.Value() || originKind != IntentOriginInternalAutomation.String() {
		t.Fatalf("SQL origin did not enforce actor NULL: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.command_intents SET origin_kind='external_actor',actor_id='forged',origin_key=NULL WHERE command_id=$1`,
		"nha029-internal-command"); err == nil {
		t.Fatal("internal ledger provenance was mutable")
	} else {
		nha151SQLState(t, err, "23514")
	}
	read, found, err := store.CommandIntent(ctx, commandID)
	if err != nil || !found || read.OriginKind() != IntentOriginInternalAutomation || !bytes.Equal(read.Fingerprint(), fingerprint.digest[:]) {
		t.Fatalf("internal origin read: found=%t err=%v", found, err)
	}
	if _, created, outcome, err := store.AcceptCommandIntent(ctx, commandID, actor, "nha029-epoch", []byte("external-canonical")); err == nil || created || outcome != TransactionNotCommitted {
		t.Fatalf("external actor crossed internal command ID: created=%t outcome=%v err=%v", created, outcome, err)
	}
	if _, created, outcome, err := store.AcceptInternalAutomationIntent(ctx, commandID, key, "nha029-epoch", fingerprint, target); err != nil || created || outcome != TransactionCommitted {
		t.Fatalf("same internal occurrence did not dedupe: created=%t outcome=%v err=%v", created, outcome, err)
	}
	otherKey, err := NewInternalAutomationOriginKey("automation.kitchen/run-2/generation-7/ordinal-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, created, outcome, err := store.AcceptInternalAutomationIntent(ctx, commandID, otherKey, "nha029-epoch", fingerprint, target); err == nil || created || outcome != TransactionNotCommitted {
		t.Fatalf("different occurrence reused internal command ID: created=%t outcome=%v err=%v", created, outcome, err)
	}
	if _, err := NewInternalAutomationOriginKey(""); err == nil {
		t.Fatal("zero internal occurrence admitted")
	}
	if _, err := NewInternalAutomationFingerprint(InternalAutomationOriginKey{}, []byte("canonical")); err == nil {
		t.Fatal("zero internal origin fingerprint admitted")
	}
}

func TestNHA029StartupSecretAdmissionRejectsOrphansAndHistory(t *testing.T) {
	store, ctx := nha151Store(t)
	dataDir := filepath.Join(t.TempDir(), "private-data")
	if state, err := store.ProbeStartupSecretState(ctx, dataDir); err != nil || state != SecretStartupFreshPair {
		t.Fatalf("fresh SQL/file pair: state=%v err=%v", state, err)
	}
	if _, err := store.BootstrapSecretsForStartup(ctx, dataDir, RequireEstablishedSecretStartup()); err == nil {
		t.Fatal("established-only admission initialized a fresh key")
	}
	if _, err := os.Stat(filepath.Join(dataDir, secretFile)); !os.IsNotExist(err) {
		t.Fatalf("failed established-only admission published key: %v", err)
	}
	admission, err := FreshSecretStartup(sha256.Sum256([]byte("admitted-exact-operational-raw")), SecretStartupExplicitBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapSecretsForStartup(ctx, dataDir, admission); err != nil {
		t.Fatalf("fresh explicit bootstrap failed: %v", err)
	}
	if state, err := store.ProbeStartupSecretState(ctx, dataDir); err != nil || state != SecretStartupEstablishedPair {
		t.Fatalf("established SQL/file pair: state=%v err=%v", state, err)
	}
	if _, err := store.BootstrapSecretsForStartup(ctx, dataDir, RequireEstablishedSecretStartup()); err != nil {
		t.Fatalf("established restart refused: %v", err)
	}
	wrongStore, wrongCtx := nha151Store(t)
	if state, err := wrongStore.ProbeStartupSecretState(wrongCtx, dataDir); err != nil || state != SecretStartupOrphanFile {
		t.Fatalf("wrong empty DB with old key: state=%v err=%v", state, err)
	}
	if _, err := wrongStore.BootstrapSecretsForStartup(wrongCtx, dataDir, admission); err == nil {
		t.Fatal("orphan file adopted into wrong empty database")
	}
	historicalStore, historicalCtx := nha151Store(t)
	binding := nha151Binding(t, "legacy.configuration", "configuration.yaml")
	candidate, _ := nha151Candidate(t, binding.Path(), "preexisting-active")
	nha151Write(t, historicalStore, historicalCtx, candidate, binding, ExpectNoConfigActive())
	historicalDir := filepath.Join(t.TempDir(), "historical-data")
	if state, err := historicalStore.ProbeStartupSecretState(historicalCtx, historicalDir); err != nil || state != SecretStartupHistoryWithoutKey {
		t.Fatalf("SQL active without key: state=%v err=%v", state, err)
	}
	if _, err := historicalStore.BootstrapSecretsForStartup(historicalCtx, historicalDir, admission); err == nil {
		t.Fatal("history without key reinitialized master key")
	}
	if _, err := FreshSecretStartup([sha256.Size]byte{}, SecretStartupLegacyCombined); err == nil {
		t.Fatal("zero raw-source digest admitted as fresh")
	}
}

func TestNHA029ActivationAuditIncludesTouchedPolicyOutsideManifest(t *testing.T) {
	store, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "Activation Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	seed, err := store.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := nha050Expected(t, seed)
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := RandomActivationAttemptID()
	if err != nil {
		t.Fatal(err)
	}
	binding := nha151Binding(t, "operational.main", "configuration.yaml")
	raw := []byte("# operational source; account policies are autonomous\n")
	policySource := nha050Source(t, admin.ID, true, "touched cohort")
	var policyRef ConfigVersionRef
	var generation ConfigGenerationRevision
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var e error
		policyRef, e = tx.SaveAuthorizationPolicy(policySource, nha151Metadata(t))
		if e != nil {
			return e
		}
		if _, e = tx.ActivateAuthorizationPolicy(admin.ID, before, policyRef); e != nil {
			return e
		}
		operational, e := tx.SaveProtectedOperationalSource(replay, binding, raw, sha256.Sum256(raw), nha151Metadata(t))
		if e != nil {
			return e
		}
		generation, e = tx.RegisterCompiledGeneration(binding.Path(), sha256.Sum256([]byte("compiled")), []ConfigVersionRef{operational})
		if e != nil {
			return e
		}
		if e = tx.ActivateGeneration(ExpectNoConfigActive(), generation); e != nil {
			return e
		}
		if e = tx.RecordActivationAttempt(attemptID, nha151Metadata(t)); e != nil {
			return e
		}
		return tx.RecordActivationPolicyChange(attemptID, admin.ID, before, policyRef)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("atomic touched-policy activation: outcome=%v err=%v", outcome, err)
	}
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != generation || len(active.Versions()) != 1 {
		t.Fatalf("policy leaked into bounded manifest: present=%t err=%v", present, err)
	}
	var accountID string
	var afterRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT account_id,after_revision FROM public.config_activation_policy_changes WHERE attempt_id=$1`,
		attemptID.value[:]).Scan(&accountID, &afterRevision); err != nil || afterRevision != policyRef.Revision().Number() {
		t.Fatalf("touched policy audit absent: %v", err)
	}
	accountText, _ := admin.ID.Value()
	if accountID != accountText {
		t.Fatal("touched policy audit bound to wrong account")
	}
}

func TestNHA029UpgradeFrom21To22AndReopen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	port := testCluster(t)
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(migrations) < 22 || migrations[19].version.Number() != 20 || migrations[19].sql != executionHistorySchemaSQL || migrations[20].version.Number() != 21 || migrations[20].sql != recorderAggregateSchemaSQL || migrations[21].version.Number() != 22 || migrations[21].sql != configActivationSchemaSQL {
		t.Fatal("activation migration 22 must follow the unchanged 1–21 history")
	}
	if version, err := store.migrate(ctx, migrations[:21]); err != nil || version.Number() != 21 {
		t.Fatalf("historical migration chain: %v %v", version, err)
	}
	if version, err := store.Migrate(ctx); err != nil || version.Number() != 22 {
		t.Fatalf("upgrade to migration 22: %v %v", version, err)
	}
	if version, err := store.Migrate(ctx); err != nil || version.Number() != 22 {
		t.Fatalf("migration 22 reopen: %v %v", version, err)
	}
	var protectedColumn, bootstrapTable bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='config_versions' AND column_name='source_representation'
	), to_regclass('public.config_bootstrap_active') IS NOT NULL`).Scan(&protectedColumn, &bootstrapTable); err != nil || !protectedColumn || !bootstrapTable {
		t.Fatalf("migration 22 source/bootstrap schema missing: %t %t %v", protectedColumn, bootstrapTable, err)
	}
	binding := nha151Binding(t, "legacy.config", "configuration.yaml")
	candidate, exact := nha151Candidate(t, binding.Path(), "legacy-after-21")
	_, generation := nha151Write(t, store, ctx, candidate, binding, ExpectNoConfigActive())
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != generation ||
		len(active.Versions()) != 1 {
		t.Fatalf("legacy active after migration 22: %t %v", present, err)
	}
	read, hasBytes := active.Versions()[0].Bytes()
	if !hasBytes || !bytes.Equal(read, exact) || active.Versions()[0].SHA256() != candidate.Sources()[0].SHA256() {
		t.Fatal("migration 22 changed legacy exact source or hash")
	}
}

// The migration-22 replacement of the version guard must preserve every
// pre-existing active pointer, including the per-account human policy.
func TestNHA029GCGuardPreservesAllActiveVersionOwners(t *testing.T) {
	store, ctx := nha151Store(t)
	binding := nha151Binding(t, "nha029.gc.config", "configuration.yaml")
	first, _ := nha151Candidate(t, binding.Path(), "inactive before GC")
	inactiveRef, firstGeneration := nha151Write(t, store, ctx, first, binding, ExpectNoConfigActive())
	second, _ := nha151Candidate(t, binding.Path(), "active during GC")
	activeRef, _ := nha151Write(t, store, ctx, second, binding, nha151ExpectedActive(t, store, ctx, firstGeneration))

	admin := nha049AccountInput(t, "NHA029 GC Policy Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	policy, err := store.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	policyVersion, err := policy.Version()
	if err != nil {
		t.Fatal(err)
	}
	policyRevision, _, err := policyVersion.Values()
	if err != nil {
		t.Fatal(err)
	}
	policyBinding, err := policyBinding(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mfaDocument string
	var mfaRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT document_id,version_revision FROM public.auth_mfa_policy WHERE singleton`).
		Scan(&mfaDocument, &mfaRevision); err != nil {
		t.Fatal(err)
	}
	bootstrapBinding := nha151Binding(t, "nha029.gc.bootstrap", "configuration.yaml.bootstrap.yaml")
	bootstrapRaw := []byte("listen: 127.0.0.1:8123\n")
	var bootstrapRef ConfigVersionRef
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var e error
		bootstrapRef, e = tx.SaveBootstrapLaunchSource(bootstrapBinding, bootstrapRaw,
			sha256.Sum256(bootstrapRaw), nha151Metadata(t))
		if e != nil {
			return e
		}
		return tx.ActivateBootstrapSource(ExpectNoBootstrapActive(), bootstrapRef)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("bootstrap pointer setup: outcome=%v err=%v", outcome, err)
	}

	deleteWithGC := func(document string, revision int64) (TransactionOutcome, error) {
		return store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('homeward.config_gc','on',true)`); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `DELETE FROM public.config_versions WHERE document_id=$1 AND revision=$2`,
				document, revision)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("GC DELETE affected %d rows", tag.RowsAffected())
			}
			return nil
		})
	}
	outcome, err = deleteWithGC(inactiveRef.Document().String(), inactiveRef.Revision().Number())
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("legitimate inactive version collection refused: outcome=%v err=%v", outcome, err)
	}
	for _, test := range []struct {
		name     string
		document string
		revision int64
	}{
		{"configuration", activeRef.Document().String(), activeRef.Revision().Number()},
		{"human policy", policyBinding.id.document, int64(policyRevision)},
		{"MFA policy", mfaDocument, mfaRevision},
		{"bootstrap", bootstrapRef.Document().String(), bootstrapRef.Revision().Number()},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := deleteWithGC(test.document, test.revision)
			if outcome != TransactionNotCommitted || err == nil {
				t.Fatalf("active version deleted with GC flag: outcome=%v err=%v", outcome, err)
			}
			nha151SQLState(t, err, "23514")
			var stillPresent bool
			if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.config_versions
				WHERE document_id=$1 AND revision=$2)`, test.document, test.revision).Scan(&stillPresent); err != nil || !stillPresent {
				t.Fatalf("active version lost: present=%t err=%v", stillPresent, err)
			}
		})
	}
}

// A fresh connection is insufficient to prove an attempt absent while the
// original transaction still owns the NHA-151 configuration commit barrier.
func TestNHA029AttemptReadWaitsForCommitOrRollback(t *testing.T) {
	for _, test := range []struct {
		name   string
		commit bool
	}{{"commit", true}, {"rollback", false}} {
		t.Run(test.name, func(t *testing.T) {
			store, ctx := nha151Store(t)
			secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
			if err != nil {
				t.Fatal(err)
			}
			replay, err := NewConfigReplayRepository(store, secrets)
			if err != nil {
				t.Fatal(err)
			}
			binding := nha151Binding(t, "nha029.attempt.barrier", "configuration.yaml")
			metadata := nha151Metadata(t)
			raw := []byte("# exact operational attempt under SQL lock\nexternal_workers: []\n")
			attemptID, err := RandomActivationAttemptID()
			if err != nil {
				t.Fatal(err)
			}
			staged := make(chan struct{})
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			type writeResult struct {
				outcome TransactionOutcome
				err     error
			}
			writerDone := make(chan writeResult, 1)
			var generation ConfigGenerationRevision
			go func() {
				outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
					ref, err := tx.SaveProtectedOperationalSource(replay, binding, raw,
						sha256.Sum256(raw), metadata)
					if err != nil {
						return err
					}
					generation, err = tx.RegisterCompiledGeneration(binding.Path(),
						sha256.Sum256([]byte("compiled attempt barrier")), []ConfigVersionRef{ref})
					if err != nil {
						return err
					}
					if err = tx.ActivateGeneration(ExpectNoConfigActive(), generation); err != nil {
						return err
					}
					if err = tx.RecordActivationAttempt(attemptID, metadata); err != nil {
						return err
					}
					close(staged)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					if !test.commit {
						return fmt.Errorf("deliberate attempt rollback")
					}
					return nil
				})
				writerDone <- writeResult{outcome, err}
			}()
			select {
			case <-staged:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			type readResult struct {
				attempt ConfigActivationAttempt
				found   bool
				err     error
			}
			readerDone := make(chan readResult, 1)
			go func() {
				attempt, found, err := store.ReadActivationAttempt(ctx, attemptID)
				readerDone <- readResult{attempt, found, err}
			}()
			var early *readResult
			var waitErr error
			waitedOnBarrier := false
			deadline := time.Now().Add(5 * time.Second)
			for early == nil && !waitedOnBarrier && time.Now().Before(deadline) {
				select {
				case result := <-readerDone:
					early = &result
				default:
				}
				if early != nil {
					break
				}
				var waiting int
				waitErr = store.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
					WHERE locktype='advisory' AND classid=481316 AND objid=151 AND NOT granted`).Scan(&waiting)
				if waitErr != nil {
					break
				}
				waitedOnBarrier = waiting > 0
				if !waitedOnBarrier {
					time.Sleep(10 * time.Millisecond)
				}
			}
			close(release)
			released = true
			var written writeResult
			select {
			case written = <-writerDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var read readResult
			if early != nil {
				read = *early
			} else {
				select {
				case read = <-readerDone:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if waitErr != nil || !waitedOnBarrier || early != nil {
				t.Fatalf("attempt reader bypassed 151 before writer ended: waited=%t early=%v queryErr=%v",
					waitedOnBarrier, early, waitErr)
			}
			if test.commit {
				if written.outcome != TransactionCommitted || written.err != nil ||
					read.err != nil || !read.found || read.attempt.ID() != attemptID || read.attempt.Target() != generation {
					t.Fatalf("committed attempt not reconciled: write=%v %v read=%t %v",
						written.outcome, written.err, read.found, read.err)
				}
			} else if written.outcome != TransactionNotCommitted || written.err == nil || read.err != nil || read.found {
				t.Fatalf("rolled-back attempt not proven absent: write=%v %v read=%t %v",
					written.outcome, written.err, read.found, read.err)
			}
		})
	}
}

// A retired v2 manifest remains diagnosable after its source is legitimately
// collected, without making the protected bytes available to generic readers.
func TestNHA029RetiredProtectedGenerationBecomesIncompleteAfterTwentyVersions(t *testing.T) {
	store, ctx := nha151Store(t)
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewConfigReplayRepository(store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	binding := nha151Binding(t, "operational.retention", "configuration.yaml")
	expected := ExpectNoConfigActive()
	var firstRef ConfigVersionRef
	var firstGeneration, latestGeneration ConfigGenerationRevision
	var firstManifest [sha256.Size]byte
	for i := 1; i <= 21; i++ {
		raw := []byte(fmt.Sprintf("# protected operational revision %d\nexternal_workers: []\n", i))
		attemptID, err := RandomActivationAttemptID()
		if err != nil {
			t.Fatal(err)
		}
		var ref ConfigVersionRef
		var generation ConfigGenerationRevision
		outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
			var e error
			ref, e = tx.SaveProtectedOperationalSource(replay, binding, raw, sha256.Sum256(raw), nha151Metadata(t))
			if e != nil {
				return e
			}
			compiled := sha256.Sum256([]byte(fmt.Sprintf("compiled operational revision %d", i)))
			generation, e = tx.RegisterCompiledGeneration(binding.Path(), compiled, []ConfigVersionRef{ref})
			if e != nil {
				return e
			}
			if e = tx.ActivateGeneration(expected, generation); e != nil {
				return e
			}
			return tx.RecordActivationAttempt(attemptID, nha151Metadata(t))
		})
		if outcome != TransactionCommitted || err != nil {
			t.Fatalf("protected generation %d: outcome=%v err=%v", i, outcome, err)
		}
		active, present, err := store.ReadActiveConfig(ctx)
		if err != nil || !present || active.Revision() != generation ||
			active.Availability() != ConfigGenerationComplete {
			t.Fatalf("protected active generation %d: present=%t err=%v", i, present, err)
		}
		if i == 1 {
			firstRef, firstGeneration, firstManifest = ref, generation, active.ManifestSHA256()
		}
		expected, err = ExpectConfigActive(generation, active.ManifestSHA256())
		if err != nil {
			t.Fatal(err)
		}
		latestGeneration = generation
	}
	var retained bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.config_versions
		WHERE document_id=$1 AND revision=$2)`, firstRef.Document().String(),
		firstRef.Revision().Number()).Scan(&retained); err != nil || retained {
		t.Fatalf("old source was not purged: retained=%t err=%v", retained, err)
	}
	historical, err := store.ReadConfigGeneration(ctx, firstGeneration)
	if err != nil || historical.Availability() != ConfigGenerationIncomplete ||
		historical.ManifestSHA256() != firstManifest || len(historical.Versions()) != 1 {
		t.Fatalf("retired protected generation not incomplete: availability=%v err=%v",
			historical.Availability(), err)
	}
	version := historical.Versions()[0]
	if version.Ref() != firstRef || version.Present() {
		t.Fatal("purged protected source reported present")
	}
	if data, ok := version.Bytes(); ok || data != nil {
		t.Fatal("purged protected source leaked through generic reader")
	}
	if _, err := historical.Restore(); err == nil {
		t.Fatal("incomplete protected generation restored")
	} else {
		nha151Kind(t, err, ConfigIncomplete)
	}
	latest, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || latest.Revision() != latestGeneration ||
		latest.Availability() != ConfigGenerationComplete {
		t.Fatalf("current protected generation lost after collection: present=%t err=%v", present, err)
	}
}
