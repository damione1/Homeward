package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/jackc/pgx/v5"
)

func TestNHA141LoginKeyCapacityExpiresAtWindowBoundary(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.auth_login_attempts(login_digest,attempts)
			SELECT decode(md5(n::text)||md5(n::text),'hex'), ARRAY[$1::timestamptz]
			FROM generate_series(1,4096) AS x(n)`, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName("NHA141 capacity unknown login")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveLoginAttempt(ctx, login, now); !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("4097th live login key was admitted: %v", err)
	}
	if err := store.ReserveLoginAttempt(ctx, login, now.Add(loginAttemptWindow)); err != nil {
		t.Fatalf("expired keys were not reclaimed at exact boundary: %v", err)
	}
}

func nha141Digests(t *testing.T) (accounts.AccessDigest, accounts.RefreshDigest) {
	t.Helper()
	access, err := accounts.NewAccessSecret()
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := accounts.NewRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	a, err := access.Digest()
	if err != nil {
		t.Fatal(err)
	}
	r, err := refresh.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return a, r
}

func TestNHA141StoreRotationReplayAndRollingLimit(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	input := nha049AccountInput(t, "NHA141 Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	record, err := store.BootstrapAdmin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := record.Revision()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < 5; i++ {
		if err := store.ReserveLoginAttempt(ctx, input.Login, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if err := store.ReserveLoginAttempt(ctx, input.Login, now.Add(5*time.Second)); !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("sixth attempt: %v", err)
	}
	if err := store.ReserveLoginAttempt(ctx, input.Login, now.Add(loginAttemptWindow)); err != nil {
		t.Fatalf("rolling window did not release oldest timestamp at exact boundary: %v", err)
	}
	id, err := accounts.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	access, refresh := nha141Digests(t)
	created, err := store.CreateSession(ctx, SessionCreation{ID: id, Account: input.ID, Revision: revision,
		AccessDigest: access, RefreshDigest: refresh, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Session.ExpiresAt().Equal(now.Add(30*24*time.Hour)) || !created.AccessExpiresAt.Equal(now.Add(15*time.Minute)) {
		t.Fatal("session/access lifetime changed")
	}
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: id}, access, created.AccessExpiresAt); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("access admitted at exact expiry: %v", err)
	}
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: id}, access, now); err != nil {
		t.Fatal(err)
	}
	newAccess, newRefresh := nha141Digests(t)
	rotated, err := store.RotateRefresh(ctx, RefreshRotation{Owner: SessionOwner{Account: input.ID, Session: id},
		OldDigest: refresh, NewDigest: newRefresh, NewAccessDigest: newAccess, Now: now.Add(time.Minute)})
	rotatedRecord, _, rotatedOK := rotated.Rotated()
	if err != nil || !rotatedOK || rotated.ReplayRevoked() || rotated.GenerationExhausted() {
		t.Fatalf("first rotation: %+v %v", rotated, err)
	}
	if !rotatedRecord.ExpiresAt().Equal(created.Session.ExpiresAt()) {
		t.Fatal("refresh slid absolute expiry")
	}
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: id}, access, now.Add(time.Minute)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("old access survived rotation: %v", err)
	}
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: id}, newAccess, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.RotateRefresh(ctx, RefreshRotation{Owner: SessionOwner{Account: input.ID, Session: id},
		OldDigest: refresh, NewDigest: newRefresh, NewAccessDigest: newAccess, Now: now.Add(2 * time.Minute)})
	if err != nil || !replayed.ReplayRevoked() {
		t.Fatalf("consumed digest did not revoke family: %+v %v", replayed, err)
	}
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: id}, newAccess, now.Add(2*time.Minute)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("replayed family access survived: %v", err)
	}
	listed, err := store.ListSessions(ctx, input.ID, input.ID, now.Add(2*time.Minute))
	if err != nil || len(listed) != 0 {
		t.Fatalf("revoked session listed: %d %v", len(listed), err)
	}
	for i := 0; i < 5; i++ {
		if err := store.ReserveLoginAttempt(ctx, input.Login, now.Add(time.Duration(i+3)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReserveLoginAttempt(ctx, input.Login, now.Add(9*time.Minute)); !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("expected block before recovery: %v", err)
	}
	recovered, err := store.RecoverAccount(ctx, input.ID, input.Verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveLoginAttempt(ctx, input.Login, now.Add(9*time.Minute)); err != nil {
		t.Fatalf("recovery did not clear limiter: %v", err)
	}
	recoveredRevision, err := recovered.Revision()
	if err != nil {
		t.Fatal(err)
	}
	nextID, err := accounts.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	nextAccess, nextRefresh := nha141Digests(t)
	if _, err := store.CreateSession(ctx, SessionCreation{ID: nextID, Account: input.ID, Revision: recoveredRevision,
		AccessDigest: nextAccess, RefreshDigest: nextRefresh, Now: created.Session.ExpiresAt()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LookupRefreshOwner(ctx, refresh); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired refresh family survived bounded pruning: %v", err)
	}
}

func TestNHA141StoreChallengeAttemptReservation(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	input := nha049AccountInput(t, "NHA141 Challenge Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	record, err := store.BootstrapAdmin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := record.Revision()
	if err != nil {
		t.Fatal(err)
	}
	id, err := accounts.NewChallengeID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	issued, err := store.IssueChallenge(ctx, ChallengeCreation{ID: id, Account: input.ID, Revision: revision, Payload: []byte{0x01}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LookupChallenge(ctx, id, issued.ExpiresAt); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("challenge admitted at expiry: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := store.ReserveChallengeAttempt(ctx, issued, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("proof slot %d: %v", i, err)
		}
	}
	if err := store.ReserveChallengeAttempt(ctx, issued, now.Add(5*time.Second)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("sixth proof slot: %v", err)
	}
}

func TestNHA141StoreOldestSessionEvictionAndGenerationLimit(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	input := nha049AccountInput(t, "NHA141 Capacity Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	record, err := store.BootstrapAdmin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := record.Revision()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	var firstIDText string
	var firstAccess accounts.AccessDigest
	var lastIDText string
	var lastAccess accounts.AccessDigest
	var lastRefresh accounts.RefreshDigest
	for i := 0; i < 17; i++ {
		id, err := accounts.NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		access, refresh := nha141Digests(t)
		created, err := store.CreateSession(ctx, SessionCreation{ID: id, Account: input.ID, Revision: revision,
			AccessDigest: access, RefreshDigest: refresh, Now: now.Add(time.Duration(i) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		idText, _ := id.Value()
		if i == 0 {
			firstIDText, firstAccess = idText, access
		}
		if i == 16 {
			lastIDText, lastAccess, lastRefresh = idText, access, refresh
			if created.Evicted == nil {
				t.Fatal("oldest session was not evicted")
			}
			evicted, err := created.Evicted.Value()
			if err != nil || evicted != firstIDText {
				t.Fatalf("wrong session evicted: %v %v", created.Evicted, err)
			}
		} else if created.Evicted != nil {
			t.Fatalf("early eviction at session %d", i)
		}
	}
	firstID, _ := contracts.ParseSessionID(firstIDText)
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: firstID}, firstAccess, now.Add(17*time.Second)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("evicted access survived: %v", err)
	}
	listed, err := store.ListSessions(ctx, input.ID, input.ID, now.Add(17*time.Second))
	if err != nil || len(listed) != 16 {
		t.Fatalf("active session bound: count=%d err=%v", len(listed), err)
	}
	// Seed the final generation to exercise the 4096 boundary without 4095
	// redundant network transactions. The production RotateRefresh path runs.
	if _, err := store.pool.Exec(ctx, `UPDATE public.auth_refresh_generations SET generation=4096 WHERE session_id=$1`, lastIDText); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.auth_sessions SET current_generation=4096 WHERE id=$1`, lastIDText); err != nil {
		t.Fatal(err)
	}
	lastID, _ := contracts.ParseSessionID(lastIDText)
	newAccess, newRefresh := nha141Digests(t)
	result, err := store.RotateRefresh(ctx, RefreshRotation{Owner: SessionOwner{Account: input.ID, Session: lastID},
		OldDigest: lastRefresh, NewDigest: newRefresh, NewAccessDigest: newAccess, Now: now.Add(18 * time.Second)})
	if err != nil || !result.GenerationExhausted() {
		t.Fatalf("generation 4097 admitted: %v %+v", err, result)
	}
	if _, err := store.AdmitAccess(ctx, SessionOwner{Account: input.ID, Session: lastID}, lastAccess, now.Add(18*time.Second)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("exhausted family access survived: %v", err)
	}
}
