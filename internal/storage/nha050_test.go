package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

func nha050Source(t *testing.T, account contracts.AccountID, allow bool, comment string) authorization.PolicySource {
	t.Helper()
	text, err := account.Value()
	if err != nil {
		t.Fatal(err)
	}
	grants := "grants: []\n"
	if allow {
		grants = `grants:
  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
	}
	data := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\n%s# %s\n", text, grants, comment))
	source, err := authorization.AdmitPolicySource(account, data)
	if err != nil {
		t.Fatal(err)
	}
	return source
}
func nha050Expected(t *testing.T, snapshot AuthorizationPolicySnapshot) authorization.ExpectedVersion {
	t.Helper()
	version, err := snapshot.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	return expected
}
func nha050Replace(t *testing.T, s *Store, ctx context.Context, admin, target contracts.AccountID, source authorization.PolicySource, expected authorization.ExpectedVersion) (AuthorizationPolicySnapshot, bool) {
	t.Helper()
	outcome, err := s.ReplaceAuthorizationPolicy(ctx, admin, target, source, expected)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, changed, err := outcome.Result()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, changed
}

func TestNHA050PolicySeedCASAndCurrentAccount(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "Policy Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	user := nha049AccountInput(t, "Policy User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := s.CreateAccount(ctx, admin.ID, user); err != nil {
		t.Fatal(err)
	}
	seed, err := s.AuthorizationPolicy(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	version, err := seed.Version()
	if err != nil {
		t.Fatal(err)
	}
	revision, _, _ := version.Values()
	if revision != 1 {
		t.Fatalf("seed revision=%d", revision)
	}
	if _, err := s.GetAuthorizationPolicy(ctx, user.ID, admin.ID); !errors.Is(err, ErrAccountPermission) {
		t.Fatalf("user policy read: %v", err)
	}
	source := nha050Source(t, user.ID, true, "first")
	first, changed := nha050Replace(t, s, ctx, admin.ID, user.ID, source, nha050Expected(t, seed))
	if !changed {
		t.Fatal("new rights reported unchanged")
	}
	if _, err := s.ReplaceAuthorizationPolicy(ctx, admin.ID, user.ID, source, nha050Expected(t, seed)); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	comment := nha050Source(t, user.ID, true, "comments only")
	second, changed := nha050Replace(t, s, ctx, admin.ID, user.ID, comment, nha050Expected(t, first))
	if changed {
		t.Fatal("comment changed rights")
	}
	before, _ := first.State()
	after, _ := second.State()
	a, _ := before.RightsRevision()
	b, _ := after.RightsRevision()
	if a != b {
		t.Fatal("no-op advanced effective revision")
	}
	disabled := authv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	if _, err := s.UpdateAccount(ctx, admin.ID, user.ID, AccountChanges{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	current, err := s.AuthorizationPolicy(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := current.State()
	if err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	if err := authority.Hydrate([]authorization.AccountState{state}); err != nil {
		t.Fatal(err)
	}
	session, _ := contracts.ParseSessionID("synthetic-session")
	principal, _ := authorization.NativePrincipal(user.ID, session)
	if _, err := authority.Current(principal); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("disabled role/status snapshot remained active")
	}
}

func TestNHA050PolicyRetentionABAAndHistoricalHADiscrimination(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "Policy Retention Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	current, err := s.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	policyA := nha050Source(t, admin.ID, true, "A")
	current, _ = nha050Replace(t, s, ctx, admin.ID, admin.ID, policyA, nha050Expected(t, current))
	versionA, _ := current.Version()
	revA, hashA, _ := versionA.Values()
	current, _ = nha050Replace(t, s, ctx, admin.ID, admin.ID, nha050Source(t, admin.ID, false, "B"), nha050Expected(t, current))
	current, _ = nha050Replace(t, s, ctx, admin.ID, admin.ID, policyA, nha050Expected(t, current))
	versionAgain, _ := current.Version()
	revAgain, hashAgain, _ := versionAgain.Values()
	if hashA != hashAgain || revAgain <= revA {
		t.Fatal("A to B to A reused source revision")
	}
	for i := 0; i < 21; i++ {
		current, _ = nha050Replace(t, s, ctx, admin.ID, admin.ID, nha050Source(t, admin.ID, true, fmt.Sprint(i)), nha050Expected(t, current))
	}
	binding, err := policyBinding(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_versions WHERE document_id=$1`, binding.id.document).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 20 {
		t.Fatalf("policy retained %d versions", count)
	}
	activeVersion, _ := current.Version()
	activeRevision, _, _ := activeVersion.Values()
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('homeward.config_gc','on',true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM public.config_versions WHERE document_id=$1 AND revision=$2`, binding.id.document, int64(activeRevision))
		return err
	})
	if outcome != TransactionNotCommitted {
		t.Fatalf("deleted active policy: %v %v", outcome, err)
	}
	nha151SQLState(t, err, "23514")
	// Stage later unactivated versions. Collection must preserve the older active
	// policy as well as the latest history, using the same collector as HA.
	for i := 0; i < 21; i++ {
		outcome, err := s.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
			_, err := config.SaveAuthorizationPolicy(nha050Source(t, admin.ID, true, fmt.Sprintf("staged-%d", i)), nha151Metadata(t))
			return err
		})
		if err != nil || outcome != TransactionCommitted {
			t.Fatalf("stage: %v %v", outcome, err)
		}
	}
	if _, err := s.AuthorizationPolicy(ctx, admin.ID); err != nil {
		t.Fatalf("collector lost old active policy: %v", err)
	}
	// Presence of the new document kind must not broaden or break the historical
	// HA reader. Its generation still contains only an admitted HA candidate.
	candidate, _ := nha151Candidate(t, "configuration.yaml", "HA after policy")
	_, generation := nha151Write(t, s, ctx, candidate, nha151Binding(t, "ha-document", "configuration.yaml"), ExpectNoConfigActive())
	history, err := s.ReadConfigGeneration(ctx, generation)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := history.Restore()
	if err != nil || !restored.Valid() || restored.SHA256() != candidate.SHA256() {
		t.Fatalf("HA restore with policy present: %v", err)
	}
	if len(history.Versions()) != 1 || history.Versions()[0].Ref().Document() == binding.id {
		t.Fatal("policy leaked into HA generation reader")
	}
}

func TestNHA050ConcurrentCASRollbackAndBoundedHydration(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "Policy Concurrent Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	seed, err := s.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := nha050Expected(t, seed)
	var wait sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, results[i] = s.ReplaceAuthorizationPolicy(ctx, admin.ID, admin.ID, nha050Source(t, admin.ID, i == 0, fmt.Sprint(i)), expected)
		}(i)
	}
	wait.Wait()
	passed, conflicted := 0, 0
	for _, err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, authorization.ErrConflict) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || conflicted != 1 {
		t.Fatalf("CAS outcomes %d/%d", passed, conflicted)
	}
	before, err := s.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic failure after pointer update")
	outcome, err := s.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
		ref, err := config.SaveAuthorizationPolicy(nha050Source(t, admin.ID, false, "rolled back"), nha151Metadata(t))
		if err != nil {
			return err
		}
		if _, err := config.ActivateAuthorizationPolicy(admin.ID, nha050Expected(t, before), ref); err != nil {
			return err
		}
		return failure
	})
	if outcome != TransactionNotCommitted || !errors.Is(err, failure) {
		t.Fatalf("rollback outcome %v %v", outcome, err)
	}
	after, err := s.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeVersion, _ := before.Version()
	afterVersion, _ := after.Version()
	if beforeVersion != afterVersion {
		t.Fatal("rollback published policy")
	}
	if err := s.EnsureAuthorizationPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	if err := authority.HydrateFrom(func(yield func(authorization.AccountState) error) error {
		return s.ForEachAuthorizationPolicy(ctx, yield)
	}); err != nil {
		t.Fatal(err)
	}
	session, _ := contracts.ParseSessionID("hydrated-session")
	principal, _ := authorization.NativePrincipal(admin.ID, session)
	if _, err := authority.Current(principal); err != nil {
		t.Fatalf("hydrated admin: %v", err)
	}
}

func TestNHA050PointerProtectionDeleteCascadeAndUnknownCommit(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "Policy Delete Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	user := nha049AccountInput(t, "Policy Delete User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := s.CreateAccount(ctx, admin.ID, user); err != nil {
		t.Fatal(err)
	}
	userText, _ := user.ID.Value()
	for _, statement := range []string{`DELETE FROM public.authorization_policy_active WHERE account_id=$1`, `TRUNCATE public.authorization_policy_active`} {
		var err error
		if statement[0] == 'D' {
			_, err = s.pool.Exec(ctx, statement, userText)
		} else {
			_, err = s.pool.Exec(ctx, statement)
		}
		if err == nil {
			t.Fatal("direct pointer removal accepted")
		}
		nha151SQLState(t, err, "23514")
	}
	seed, err := s.AuthorizationPolicy(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The real commit is rejected by PostgreSQL after the callback succeeded.
	// The repository must classify that outcome conservatively and return no
	// admitted snapshot; the server layer will quarantine before reconciliation.
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION public.nha050_refuse_policy_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic unknown policy commit'; END $$;
		CREATE CONSTRAINT TRIGGER nha050_refuse_policy_commit AFTER UPDATE ON public.authorization_policy_active
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha050_refuse_policy_commit()`); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.ReplaceAuthorizationPolicy(ctx, admin.ID, user.ID, nha050Source(t, user.ID, true, "unknown"), nha050Expected(t, seed))
	if !errors.Is(err, ErrAccountCommitUnknown) {
		t.Fatalf("commit outcome not conservative: %v", err)
	}
	if _, _, err := outcome.Result(); err == nil {
		t.Fatal("unknown commit exposed admitted policy")
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER nha050_refuse_policy_commit ON public.authorization_policy_active;
		DROP FUNCTION public.nha050_refuse_policy_commit()`); err != nil {
		t.Fatal(err)
	}
	after, err := s.AuthorizationPolicy(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedVersion, _ := seed.Version()
	afterVersion, _ := after.Version()
	if seedVersion != afterVersion {
		t.Fatal("forced commit rollback changed pointer")
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION public.nha050_refuse_delete_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic unknown account delete commit'; END $$;
		CREATE CONSTRAINT TRIGGER nha050_refuse_delete_commit AFTER DELETE ON public.auth_accounts
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha050_refuse_delete_commit()`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, admin.ID, user.ID); !errors.Is(err, ErrAccountCommitUnknown) {
		t.Fatalf("delete commit outcome: %v", err)
	}
	if _, err := s.AuthorizationPolicy(ctx, user.ID); err != nil {
		t.Fatalf("rollback lost cascading pointer: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER nha050_refuse_delete_commit ON public.auth_accounts;
		DROP FUNCTION public.nha050_refuse_delete_commit()`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, admin.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizationPolicy(ctx, user.ID); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("deleted account survived: %v", err)
	}
	var pointers int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.authorization_policy_active WHERE account_id=$1`, userText).Scan(&pointers); err != nil || pointers != 0 {
		t.Fatalf("cascade count=%d error=%v", pointers, err)
	}
	if err := s.DeleteAccount(ctx, admin.ID, admin.ID); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("last administrator lost: %v", err)
	}
	if _, err := s.AuthorizationPolicy(ctx, admin.ID); err != nil {
		t.Fatal("last-admin refusal lost its policy")
	}
}
