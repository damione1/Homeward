package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func TestNHA050UnpublishedCreationsReconcileBoundedlyAfterPrimaryReturns(t *testing.T) {
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	pgSettings, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, pgSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	coordinator, err := prepareLocalAuthorization(ctx, store, realSessionClock{})
	if err != nil {
		t.Fatal(err)
	}
	h := &accountHandler{store: store, coordinator: coordinator}
	const password = "NHA050 unpublished creation synthetic password"
	input, err := newAccountInput(ctx, "Creation Admin", "Creation Admin", password, authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := h.accountCreationGuard(ctx, input.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		t.Fatal(err)
	}
	_, err = store.BootstrapAdmin(ctx, input)
	if err := finishAccountMutation(guard, ctx, err, false); err != nil {
		t.Fatal(err)
	}
	session, _ := contracts.ParseSessionID("unpublished-creation-session")
	principal, _ := authorization.NativePrincipal(input.ID, session)
	actor, _ := principal.Actor()
	nativeCtx, _ := authorization.WithPrincipal(context.WithValue(ctx, actorContextKey{}, actor), principal)
	var dataDirectory string
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT current_setting('data_directory')`).Scan(&dataDirectory); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE SEQUENCE public.nha050_account_insert_attempts;
CREATE FUNCTION public.nha050_count_account_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('public.nha050_account_insert_attempts'); RETURN NEW; END $$;
CREATE TRIGGER nha050_count_account_insert BEFORE INSERT ON public.auth_accounts FOR EACH ROW EXECUTE FUNCTION public.nha050_count_account_insert();
CREATE FUNCTION public.nha050_unknown_account_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id LIKE 'unpublished-absent-%' THEN RAISE EXCEPTION 'synthetic unknown creation commit'; END IF; RETURN NEW; END $$;
CREATE CONSTRAINT TRIGGER nha050_unknown_account_commit AFTER INSERT ON public.auth_accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha050_unknown_account_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ids []contracts.AccountID
	// Model 18 independently admitted creations whose responses are lost. Half
	// fail at an actual deferred COMMIT, half committed before the synthetic
	// lost-result boundary. Their gates/reservations remain unpublished.
	for i := 0; i < creationReconciliationLimit+2; i++ {
		kind := "committed"
		if i%2 == 0 {
			kind = "absent"
		}
		id, _ := contracts.ParseAccountID(fmt.Sprintf("unpublished-%s-%d", kind, i))
		login, _ := accounts.ParseLoginName(fmt.Sprintf("Unpublished %d", i))
		display, _ := accounts.ParseDisplayName(fmt.Sprintf("Unpublished %d", i))
		pendingInput := storage.NewAccount{ID: id, Login: login, Display: display, Role: authv1.AccountRole_ACCOUNT_ROLE_USER, Verifier: input.Verifier}
		pending, err := h.accountMutationGuard(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		pending.creating = true // these creations were admitted concurrently
		if err := reserveAccountSeed(pending); err != nil {
			t.Fatal(err)
		}
		_, err = store.CreateAccount(ctx, input.ID, pendingInput)
		if i%2 == 0 && !errors.Is(err, storage.ErrAccountCommitUnknown) || i%2 != 0 && err != nil {
			t.Fatal("unexpected primary creation outcome", i, err)
		}
		if err := finishAccountMutation(pending, ctx, storage.ErrAccountCommitUnknown, false); !errors.Is(err, storage.ErrAccountCommitUnknown) {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	pendingCount := func() int {
		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return coordinator.pendingCreations.Len()
	}
	if pendingCount() != 18 {
		t.Fatal("unknown creations not retained")
	}
	queryCounts := func() (attempts, accounts int64) {
		t.Helper()
		if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT (SELECT last_value FROM public.nha050_account_insert_attempts),(SELECT count(*) FROM public.auth_accounts)`).Scan(&attempts, &accounts)
		}); err != nil {
			t.Fatal(err)
		}
		return attempts, accounts
	}
	beforeAttempts, beforeAccounts := queryCounts()
	if beforeAttempts != 18 || beforeAccounts != 10 {
		t.Fatal("unknown creation fixture counts", beforeAttempts, beforeAccounts)
	}
	// Stop/restart only the disposable testpg cluster's own data directory.
	// Its registered cleanup remains responsible for the restarted instance.
	running := true
	restartLog := filepath.Join(t.TempDir(), "restarted-postgres.log")
	restart := func() error {
		options := fmt.Sprintf("-h 127.0.0.1 -p %d -c unix_socket_directories=''", cluster.Port)
		output, err := exec.Command("pg_ctl", "-D", dataDirectory, "-l", restartLog, "-o", options, "-w", "start").CombinedOutput()
		if err != nil {
			return fmt.Errorf("restart disposable primary: %w: %s", err, output)
		}
		running = true
		return nil
	}
	t.Cleanup(func() {
		if !running {
			if err := restart(); err != nil {
				t.Error(err)
			}
		}
	})
	if output, err := exec.Command("pg_ctl", "-D", dataDirectory, "-m", "fast", "-w", "stop").CombinedOutput(); err != nil {
		t.Fatalf("stop disposable primary: %v: %s", err, output)
	}
	running = false
	create := func(login string) error {
		_, err := h.CreateAccount(nativeCtx, connect.NewRequest(&authv1.CreateAccountRequest{LoginName: login, DisplayName: login, InitialPassword: password, Role: authv1.AccountRole_ACCOUNT_ROLE_USER}))
		return err
	}
	if err := create("Primary Unavailable New Account"); connect.CodeOf(err) != connect.CodeUnavailable || pendingCount() != 18 {
		t.Fatal("primary outage accumulated another unknown creation", err, pendingCount())
	}
	if err := restart(); err != nil {
		t.Fatal(err)
	}
	// Keep one candidate busy. The bounded retry must rotate past it instead
	// of waiting while owning any other gate, then revisit it on the next call.
	coordinator.mu.Lock()
	busyKey := coordinator.pendingCreations.Front().Value.(string)
	busyGate := coordinator.accounts[busyKey]
	select {
	case <-busyGate.permit:
		busyGate.refs++
	default:
		coordinator.mu.Unlock()
		t.Fatal("fixture gate unexpectedly busy")
	}
	coordinator.mu.Unlock()
	busy := &sessionGuard{coordinator: coordinator, key: busyKey, gate: busyGate}
	t.Cleanup(busy.release)
	done := make(chan error, 1)
	go func() { done <- create("Bounded Retry New Account") }()
	select {
	case err := <-done:
		if connect.CodeOf(err) != connect.CodeUnavailable || pendingCount() != 3 {
			t.Fatal("retry exceeded 16 candidates or lost fair rotation", err, pendingCount())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("creation waited for a busy quarantine gate")
	}
	if attempts, accounts := queryCounts(); attempts != beforeAttempts || accounts != beforeAccounts {
		t.Fatal("bounded reconciliation retried an old INSERT", attempts, accounts)
	}
	busy.release()
	if err := create("Recovered Primary New Account"); err != nil || pendingCount() != 0 {
		t.Fatal("new creation could not recover without restart", err, pendingCount())
	}
	if attempts, accounts := queryCounts(); attempts != beforeAttempts+1 || accounts != beforeAccounts+1 {
		t.Fatal("reconciliation resent an old creation", attempts, accounts)
	}
	for _, id := range ids {
		key, _ := id.Value()
		coordinator.mu.Lock()
		gate := coordinator.accounts[key]
		coordinator.mu.Unlock()
		if gate != nil {
			t.Fatal("settled unpublished gate was not evicted", key)
		}
		// Every old reservation was either consumed by its committed seed or
		// freed by confirmed absence. A new reservation cannot collide with it.
		mutation, _ := coordinator.authority.BeginMutation(id)
		empty, _ := authorization.EmptySource(id)
		if err := mutation.ReservePolicy(empty); err != nil {
			t.Fatal("settled creation leaked a reservation", key, err)
		}
		mutation.Release()
	}
	if _, err := prepareLocalAuthorization(ctx, store, realSessionClock{}); err != nil {
		t.Fatal("settled seeds did not hydrate after restart", err)
	}
}
