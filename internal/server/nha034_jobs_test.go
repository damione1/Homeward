package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha034ServerStore(t *testing.T) *storage.Store {
	t.Helper()
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	pg, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}
func nha034Account(t *testing.T, store *storage.Store) storage.AccountRecord {
	t.Helper()
	input, err := newAccountInput(context.Background(), "NHA034 isolated admin", "NHA034 isolated admin", "NHA034 synthetic password", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.BootstrapAdmin(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func nha034Session(t *testing.T, store *storage.Store, account storage.AccountRecord, name string, now time.Time) (storage.SessionOwner, time.Time) {
	t.Helper()
	accountID, _ := account.ID()
	revision, _ := account.Revision()
	id, _ := contracts.ParseSessionID(name)
	access, err := accounts.NewAccessSecret()
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := accounts.NewRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := access.Digest()
	r, _ := refresh.Digest()
	created, err := store.CreateSession(context.Background(), storage.SessionCreation{ID: id, Account: accountID, Revision: revision, AccessDigest: a, RefreshDigest: r, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return storage.SessionOwner{Account: accountID, Session: id}, created.Session.ExpiresAt()
}

func TestNHA034RealSessionGrantReAdmissionRevocationExpiryAndLeaseLifetime(t *testing.T) {
	store := nha034ServerStore(t)
	account := nha034Account(t, store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := newNHA141Clock(now)
	coordinator, err := prepareLocalAuthorization(context.Background(), store, clock)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := nha034Session(t, store, account, "nha034-current-session", now)
	text, _ := owner.Account.Value()
	actor, _ := domain.ParseActorID(text)
	grant, _ := jobs.NewSessionGrant(actor, owner.Session)
	admission := &jobSessionAdmission{store, coordinator}
	lease, err := admission.Admit(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	leaseCtx, err := lease.Context()
	if err != nil || leaseCtx.Err() != nil {
		t.Fatalf("SQL context leaked into execution lease: %v", err)
	}
	// The account gate must have been released before the port returns. This
	// is the same gate subsequent 050 mutations and re-admission acquire.
	gateCtx, gateCancel := context.WithTimeout(context.Background(), time.Second)
	guard, err := coordinator.enter(gateCtx, owner.Account)
	gateCancel()
	if err != nil {
		t.Fatalf("admission retained account gate: %v", err)
	}
	if err := store.RevokeSession(context.Background(), owner.Account, owner, now); err != nil {
		guard.release()
		t.Fatal(err)
	}
	guard.cancelLeases(context.Background(), &owner.Session)
	guard.release()
	select {
	case <-leaseCtx.Done():
	default:
		t.Fatal("native revocation did not cancel job credential lease")
	}
	lease.Close()
	lease.Close()
	if _, err := admission.Admit(context.Background(), grant); err == nil {
		t.Fatal("revoked persisted session re-admitted")
	}
	owner2, expires := nha034Session(t, store, account, "nha034-expiring-session", now)
	grant2, _ := jobs.NewSessionGrant(actor, owner2.Session)
	lease2, err := admission.Admit(context.Background(), grant2)
	if err != nil {
		t.Fatal(err)
	}
	leaseCtx2, _ := lease2.Context()
	clock.Advance(expires)
	select {
	case <-leaseCtx2.Done():
	default:
		t.Fatal("session absolute expiry did not cancel job lease")
	}
	lease2.Close()
	if _, err := admission.Admit(context.Background(), grant2); err == nil {
		t.Fatal("expired persisted session re-admitted")
	}
	wrong, _ := domain.ParseActorID("different-owner")
	wrongGrant, _ := jobs.NewSessionGrant(wrong, owner2.Session)
	if _, err := admission.Admit(context.Background(), wrongGrant); err == nil {
		t.Fatal("session grant changed owner")
	}
	// Reopening the concrete coordinator retains the SQL revocation/expiry
	// rules, rather than reviving an old in-memory Principal.
	reopened, err := prepareLocalAuthorization(context.Background(), store, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&jobSessionAdmission{store, reopened}).Admit(context.Background(), grant); err == nil {
		t.Fatal("restart revived revoked session")
	}
	coordinator.mu.Lock()
	remaining := len(coordinator.accounts)
	coordinator.mu.Unlock()
	clock.mu.Lock()
	timers := len(clock.timers)
	clock.mu.Unlock()
	if remaining != 0 || timers != 0 {
		t.Fatalf("credential resources retained: gates=%d timers=%d", remaining, timers)
	}
	if _, err := (&jobSessionAdmission{}).Admit(nil, jobs.SessionGrant{}); err == nil {
		t.Fatal("zero admission accepted")
	}
}

func TestNHA034SessionGrantSharesBearerRevocationRevisionAndDisabledAccount(t *testing.T) {
	store := nha034ServerStore(t)
	admin := nha034Account(t, store)
	adminID, _ := admin.ID()
	input, err := newAccountInput(context.Background(), "NHA034 isolated user", "NHA034 isolated user", "NHA034 synthetic user password", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateAccount(context.Background(), adminID, input)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := newNHA141Clock(now)
	coordinator, err := prepareLocalAuthorization(context.Background(), store, clock)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := nha034Session(t, store, user, "nha034-user-session", now)
	text, _ := owner.Account.Value()
	actor, _ := domain.ParseActorID(text)
	grant, _ := jobs.NewSessionGrant(actor, owner.Session)
	admission := &jobSessionAdmission{store, coordinator}
	lease, err := admission.Admit(context.Background(), grant)
	if err != nil {
		t.Fatalf("nominal session reference: %v", err)
	}
	lease.Close()
	role := authv1.AccountRole_ACCOUNT_ROLE_ADMIN
	updated, err := store.UpdateAccount(context.Background(), adminID, owner.Account, storage.AccountChanges{Role: &role})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListSessions(context.Background(), owner.Account, owner.Account, now)
	if err != nil || len(listed) != 1 {
		t.Fatalf("management view witness: %d %v", len(listed), err)
	}
	if _, err := admission.Admit(context.Background(), grant); err == nil {
		t.Fatal("old account revision re-admitted through management view")
	}
	fresh, _ := nha034Session(t, store, updated, "nha034-current-revision", now)
	freshGrant, _ := jobs.NewSessionGrant(actor, fresh.Session)
	freshLease, err := admission.Admit(context.Background(), freshGrant)
	if err != nil {
		t.Fatalf("fresh revision refused: %v", err)
	}
	freshLease.Close()
	disabled := authv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	if _, err := store.UpdateAccount(context.Background(), adminID, owner.Account, storage.AccountChanges{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Admit(context.Background(), freshGrant); err == nil {
		t.Fatal("disabled account session grant admitted")
	}
}

type nha034ServerTimer struct {
	entered chan time.Duration
	tick    chan struct{}
}

func (timer *nha034ServerTimer) Wait(ctx context.Context, duration time.Duration) error {
	select {
	case timer.entered <- duration:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-timer.tick:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type nha034ServerBackup struct {
	effects atomic.Int32
	mu      sync.Mutex
	keys    map[jobs.IdempotencyKey]jobs.BackupKey
}

func (backup *nha034ServerBackup) ExecuteBackup(_ context.Context, key jobs.BackupKey, idempotency jobs.IdempotencyKey) (jobs.Outcome, error) {
	backup.mu.Lock()
	defer backup.mu.Unlock()
	if backup.keys == nil {
		backup.keys = make(map[jobs.IdempotencyKey]jobs.BackupKey)
	}
	if previous, found := backup.keys[idempotency]; found {
		if previous != key {
			return jobs.Refused, integrations.ErrInvalidInput
		}
		return jobs.Completed, nil
	}
	backup.keys[idempotency] = key
	backup.effects.Add(1)
	return jobs.Completed, nil
}
func TestNHA034CompositionRejectsAbsentTypedNilAndZeroPorts(t *testing.T) {
	var clock *nha141Clock
	var timer *nha034ServerTimer
	var backup *nha034ServerBackup
	for _, input := range []DurableJobsInput{
		{},
		{Backup: &nha034ServerBackup{}, Clock: clock, Timer: jobs.RealTimer{}},
		{Backup: &nha034ServerBackup{}, Clock: jobs.RealClock{}, Timer: timer},
		{Backup: backup, Clock: jobs.RealClock{}, Timer: jobs.RealTimer{}},
		{Commands: &DurableJobCommandInput{}, Clock: jobs.RealClock{}, Timer: jobs.RealTimer{}},
	} {
		if _, err := NewDurableJobsComposition(input); !errors.Is(err, integrations.ErrInvalidInput) {
			t.Fatalf("invalid ports admitted: %v", err)
		}
	}
	composition, err := NewDurableJobsComposition(DurableJobsInput{Backup: &nha034ServerBackup{}, Clock: jobs.RealClock{}, Timer: jobs.RealTimer{}})
	if err != nil || composition == nil {
		t.Fatal(err)
	}
	if _, err := newDurableJobsRuntime(nil, nil, nil, nil, composition); err == nil {
		t.Fatal("zero runtime admitted")
	}
}
