package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// waitForNHA141GateWaiter observes the actual account gate. The timeout only
// fails a stuck test; no wall-clock sleep establishes the race ordering.
func waitForNHA141GateWaiter(t *testing.T, coordinator *sessionCoordinator, account contracts.AccountID) {
	t.Helper()
	key, err := account.Value()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		coordinator.mu.Lock()
		gate := coordinator.accounts[key]
		waiting := gate != nil && gate.refs >= 2 && len(gate.permit) == 0
		coordinator.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("access admission never reached the account gate")
		}
		runtime.Gosched()
	}
}

func TestNHA141AccountGateCommitOrderRollbackAndUnknown(t *testing.T) {
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	storeSettings, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, storeSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := newAccountInput(ctx, "NHA141 Gate Admin", "NHA141 Gate Admin", "NHA141 synthetic gate admin password", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	adminRecord, err := store.BootstrapAdmin(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	user, err := newAccountInput(ctx, "NHA141 Gate User", "NHA141 Gate User", "NHA141 synthetic gate user password", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount(ctx, admin.ID, user); err != nil {
		t.Fatal(err)
	}
	clock := newNHA141Clock(time.Now().UTC().Truncate(time.Microsecond))
	coordinator, err := newSessionCoordinatorWithClock(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	auth := &sessionAuth{store: store, coordinator: coordinator}
	issue := func(record storage.AccountRecord) string {
		t.Helper()
		revision, err := record.Revision()
		if err != nil {
			t.Fatal(err)
		}
		id, err := accounts.NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		access, err := accounts.NewAccessSecret()
		if err != nil {
			t.Fatal(err)
		}
		refresh, err := accounts.NewRefreshSecret()
		if err != nil {
			t.Fatal(err)
		}
		accessDigest, err := access.Digest()
		if err != nil {
			t.Fatal(err)
		}
		refreshDigest, err := refresh.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateSession(ctx, storage.SessionCreation{ID: id, Account: admin.ID, Revision: revision,
			AccessDigest: accessDigest, RefreshDigest: refreshDigest, Now: clock.Now()}); err != nil {
			t.Fatal(err)
		}
		text, err := access.Text()
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	authenticate := func(access string) (context.Context, func(), error) {
		request := httptest.NewRequest(http.MethodGet, "/nha141/barrier", nil)
		request.TLS = &tls.ConnectionState{}
		request.Header.Set("Authorization", "Bearer "+access)
		_, leaseCtx, release, err := auth.AuthenticateWithLease(ctx, request)
		return leaseCtx, release, err
	}
	firstAccess := issue(adminRecord)
	firstLease, firstRelease, err := authenticate(firstAccess)
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()
	guard, err := coordinator.enter(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	adminRecord, err = store.RecoverAccount(ctx, admin.ID, admin.Verifier)
	finishAccountMutation(guard, ctx, err, true)
	if err != nil {
		t.Fatal(err)
	}
	if firstLease.Err() == nil {
		t.Fatal("flow registered before confirmed recovery was not canceled")
	}
	if _, release, err := authenticate(firstAccess); !errors.Is(err, errSessionUnauthenticated) || release != nil {
		t.Fatalf("old access admitted after recovery: %v", err)
	}
	secondAccess := issue(adminRecord)
	guard, err = coordinator.enter(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitingAdmission := make(chan error, 1)
	go func() {
		_, release, err := authenticate(secondAccess)
		if release != nil {
			release()
		}
		waitingAdmission <- err
	}()
	waitForNHA141GateWaiter(t, coordinator, admin.ID)
	adminRecord, err = store.RecoverAccount(ctx, admin.ID, admin.Verifier)
	finishAccountMutation(guard, ctx, err, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitingAdmission:
		if !errors.Is(err, errSessionUnauthenticated) {
			t.Fatalf("admission waiting behind mutation crossed its commit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("admission did not finish after mutation released its gate")
	}
	thirdAccess := issue(adminRecord)
	thirdLease, thirdRelease, err := authenticate(thirdAccess)
	if err != nil {
		t.Fatal(err)
	}
	defer thirdRelease()
	adminText, err := admin.ID.Value()
	if err != nil {
		t.Fatal(err)
	}
	adminActor, err := domain.ParseActorID(adminText)
	if err != nil {
		t.Fatal(err)
	}
	adminContext := context.WithValue(ctx, actorContextKey{}, adminActor)
	accountHandler := &accountHandler{store: store, coordinator: coordinator}
	revision, err := adminRecord.Revision()
	if err != nil {
		t.Fatal(err)
	}
	assertStillAdmitted := func(label string) {
		t.Helper()
		if thirdLease.Err() != nil {
			t.Fatalf("%s canceled an existing lease", label)
		}
		if _, release, err := authenticate(thirdAccess); err != nil {
			t.Fatalf("%s revoked access: %v", label, err)
		} else {
			release()
		}
		updated, err := store.GetAccount(ctx, admin.ID, admin.ID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := updated.Revision()
		if err != nil {
			t.Fatal(err)
		}
		if current != revision {
			t.Fatalf("%s changed revocation revision", label)
		}
	}
	currentRole := authv1.AccountRole_ACCOUNT_ROLE_ADMIN
	currentStatus := authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	if _, err := accountHandler.UpdateAccount(adminContext, connect.NewRequest(&authv1.UpdateAccountRequest{
		AccountId: &authv1.AccountId{Value: adminText}, Role: &currentRole, Status: &currentStatus,
	})); err != nil {
		t.Fatalf("no-op role/status update: %v", err)
	}
	assertStillAdmitted("no-op role/status update")
	displayOnly := "NHA141 Gate Admin Renamed"
	if _, err := accountHandler.UpdateAccount(adminContext, connect.NewRequest(&authv1.UpdateAccountRequest{
		AccountId: &authv1.AccountId{Value: adminText}, DisplayName: &displayOnly,
	})); err != nil {
		t.Fatalf("display-only update: %v", err)
	}
	assertStillAdmitted("display-only update")
	userText, _ := user.ID.Value()
	userActor, err := domain.ParseActorID(userText)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := context.WithValue(ctx, actorContextKey{}, userActor)
	demoted := authv1.AccountRole_ACCOUNT_ROLE_USER
	_, err = accountHandler.UpdateAccount(unauthorized,
		connect.NewRequest(&authv1.UpdateAccountRequest{AccountId: &authv1.AccountId{Value: adminText}, Role: &demoted}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("unauthorized mutation did not roll back: %v", err)
	}
	if thirdLease.Err() != nil {
		t.Fatal("known rollback canceled an existing lease")
	}
	if _, release, err := authenticate(thirdAccess); err != nil {
		t.Fatalf("known rollback revoked access: %v", err)
	} else {
		release()
	}
	// A deferred PostgreSQL constraint trigger fails COMMIT after the mutation
	// callback succeeds, as in NHA-096. The client cannot infer a rollback
	// from this commit response and must quarantine without retrying mutation.
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION public.nha141_refuse_account_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'deferred synthetic account failure'; END $$;
			CREATE CONSTRAINT TRIGGER nha141_refuse_account_commit AFTER UPDATE ON public.auth_accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha141_refuse_account_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	guard, err = coordinator.enter(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RecoverAccount(ctx, admin.ID, admin.Verifier)
	finishAccountMutation(guard, ctx, err, true)
	if !errors.Is(err, storage.ErrAccountCommitUnknown) {
		t.Fatalf("deferred commit failure was not indeterminate: %v", err)
	}
	if thirdLease.Err() == nil {
		t.Fatal("unknown commit outcome left an existing lease running")
	}
	key, _ := admin.ID.Value()
	coordinator.mu.Lock()
	quarantined := coordinator.accounts[key] != nil && coordinator.accounts[key].quarantined
	coordinator.mu.Unlock()
	if !quarantined {
		t.Fatal("unknown commit outcome lost quarantine before an authoritative read")
	}
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DROP TRIGGER nha141_refuse_account_commit ON public.auth_accounts;
			DROP FUNCTION public.nha141_refuse_account_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, release, err := authenticate(thirdAccess); err != nil {
		t.Fatalf("authoritative read after indeterminate commit: %v", err)
	} else {
		release()
	}
	coordinator.mu.Lock()
	quarantined = coordinator.accounts[key] != nil && coordinator.accounts[key].quarantined
	coordinator.mu.Unlock()
	if quarantined {
		t.Fatal("quarantine survived a confirmed primary-database read")
	}
	guard, err = coordinator.enter(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RecoverAccount(ctx, admin.ID, admin.Verifier)
	finishAccountMutation(guard, ctx, err, true)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newSessionCoordinatorWithClock(store, clock)
	if err != nil {
		t.Fatal(err)
	}
	auth.coordinator = restarted
	if _, release, err := authenticate(thirdAccess); !errors.Is(err, errSessionUnauthenticated) || release != nil {
		t.Fatalf("restart admitted DB-revoked access: %v", err)
	}
}
