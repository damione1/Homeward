package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
)

func TestNHA198SlidingSessionsRecoverOnlyCurrentSavedRenewal(t *testing.T) {
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
	input := nha049AccountInput(t, "Sliding Session Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	account, err := store.BootstrapAdmin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := account.Revision()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	create := func() (SessionOwner, accounts.AccessDigest, accounts.RefreshDigest) {
		t.Helper()
		id, err := accounts.NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		access, refresh := nha141Digests(t)
		created, err := store.CreateSession(ctx, SessionCreation{ID: id, Account: input.ID, Revision: revision, AccessDigest: access, RefreshDigest: refresh, Now: now})
		if err != nil || !created.Session.ExpiresAt().Equal(now.Add(7*24*time.Hour)) {
			t.Fatalf("initial inactivity deadline: %v", err)
		}
		return SessionOwner{Account: input.ID, Session: id}, access, refresh
	}
	owner, access, refresh := create()
	admittedAt := now.Add(14 * time.Minute)
	admitted, err := store.AdmitAccess(ctx, owner, access, admittedAt)
	if err != nil || !admitted.SessionExpiresAt.Equal(admittedAt.Add(7*24*time.Hour)) {
		t.Fatalf("authenticated access did not extend deadline: %v", err)
	}
	newAccess, newRefresh := nha141Digests(t)
	rotationAt := now.Add(6 * 24 * time.Hour)
	rotation := RefreshRotation{Owner: owner, OldDigest: refresh, NewDigest: newRefresh, NewAccessDigest: newAccess, Now: rotationAt, Recoverable: true}
	rotated, err := store.RotateRefresh(ctx, rotation)
	first, _, ok := rotated.Rotated()
	if err != nil || !ok || !first.ExpiresAt().Equal(rotationAt.Add(7*24*time.Hour)) {
		t.Fatalf("first saved renewal: %v", err)
	}
	// Its response is lost. Retry after the old local session deadline has
	// passed, while the committed successor is still active on the server.
	recoveredAccess, _ := nha141Digests(t)
	rotation.NewAccessDigest = recoveredAccess
	rotation.Now = rotationAt.Add(6 * 24 * time.Hour)
	recovered, err := store.RotateRefresh(ctx, rotation)
	record, _, ok := recovered.Rotated()
	if err != nil || !ok || !record.ExpiresAt().Equal(rotation.Now.Add(7*24*time.Hour)) {
		t.Fatalf("saved operation could not recover lost response: %v", err)
	}
	if _, err := store.AdmitAccess(ctx, owner, recoveredAccess, rotation.Now); err != nil {
		t.Fatalf("recovered access denied: %v", err)
	}
	if _, err := store.AdmitAccess(ctx, owner, newAccess, rotation.Now); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("old access survived recovery: %v", err)
	}
	ownerID, _ := owner.Session.Value()
	var generations int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_refresh_generations WHERE session_id=$1`, ownerID).Scan(&generations); err != nil || generations != 2 {
		t.Fatalf("recovery advanced generation: count=%d %v", generations, err)
	}
	// Continue well beyond the old 30-day absolute limit with six-day gaps.
	current := newRefresh
	for i := 0; i < 8; i++ {
		nextAccess, nextRefresh := nha141Digests(t)
		rotation.Now = rotation.Now.Add(6 * 24 * time.Hour)
		result, err := store.RotateRefresh(ctx, RefreshRotation{Owner: owner, OldDigest: current, NewDigest: nextRefresh, NewAccessDigest: nextAccess, Now: rotation.Now, Recoverable: true})
		if _, _, ok := result.Rotated(); err != nil || !ok {
			t.Fatalf("active long-lived session expired at step%d: %v", i, err)
		}
		current = nextRefresh
	}
	// The very first saved operation may never resurrect a later generation.
	rotation.Now = rotation.Now.Add(time.Minute)
	result, err := store.RotateRefresh(ctx, rotation)
	if err != nil || !result.ReplayRevoked() {
		t.Fatalf("ancient saved operation was accepted: %v", err)
	}

	expiredOwner, _, expiredRefresh := create()
	expiredAccess, expiredNext := nha141Digests(t)
	_, err = store.RotateRefresh(ctx, RefreshRotation{Owner: expiredOwner, OldDigest: expiredRefresh, NewDigest: expiredNext, NewAccessDigest: expiredAccess, Now: now.Add(7 * 24 * time.Hour), Recoverable: true})
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("exact seven-day inactivity boundary: %v", err)
	}

	ringOwner, _, ringRefresh := create()
	ringID, _ := ringOwner.Session.Value()
	if _, err := store.pool.Exec(ctx, `UPDATE public.auth_refresh_generations SET generation=4096 WHERE session_id=$1`, ringID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE public.auth_sessions SET current_generation=4096 WHERE id=$1`, ringID); err != nil {
		t.Fatal(err)
	}
	_, ancientDigest := nha141Digests(t)
	ancientRaw, _ := ancientDigest.Bytes()
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.auth_refresh_generations(session_id,generation,token_digest,state,issued_at,consumed_at) VALUES($1,1,$2,2,$3,$3)`, ringID, ancientRaw[:], now); err != nil {
		t.Fatal(err)
	}
	ringAccess, ringNext := nha141Digests(t)
	ringRotation := RefreshRotation{Owner: ringOwner, OldDigest: ringRefresh, NewDigest: ringNext, NewAccessDigest: ringAccess, Now: now.Add(time.Minute), Recoverable: true}
	wrapped, err := store.RotateRefresh(ctx, ringRotation)
	if _, _, ok := wrapped.Rotated(); err != nil || !ok {
		t.Fatalf("ring rollover forced logout: %v", err)
	}
	if _, err := store.LookupRefreshOwner(ctx, ancientDigest); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("evicted ancient bearer survived: %v", err)
	}
	retryAccess, _ := nha141Digests(t)
	ringRotation.NewAccessDigest = retryAccess
	retry, err := store.RotateRefresh(ctx, ringRotation)
	if _, _, ok := retry.Rotated(); err != nil || !ok {
		t.Fatalf("lost response at ring wrap was not recoverable: %v", err)
	}
	var currentSlot int
	if err := store.pool.QueryRow(ctx, `SELECT current_generation FROM public.auth_sessions WHERE id=$1`, ringID).Scan(&currentSlot); err != nil || currentSlot != 1 {
		t.Fatalf("wrong current ring slot: %d %v", currentSlot, err)
	}
	// An unrelated supplied digest cannot satisfy the recovery proof.
	wrongAccess, wrongRefresh := nha141Digests(t)
	_, err = store.RotateRefresh(ctx, RefreshRotation{Owner: ringOwner, OldDigest: ancientDigest, NewDigest: wrongRefresh, NewAccessDigest: wrongAccess, Now: now.Add(time.Minute), Recoverable: true})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("evicted bearer was re-admitted: %v", err)
	}
	if _, err := store.AdmitAccess(ctx, ringOwner, retryAccess, now.Add(time.Minute)); err != nil {
		t.Fatalf("unknown ancient token revoked current session: %v", err)
	}
}
