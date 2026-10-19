package server

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// These real PostgreSQL triggers fail either COMMIT after the mutation callback
// succeeds or the audit INSERT inside it. Neither test substitutes a fake
// transaction outcome or a fake authentication provider.
func TestNHA141UnknownIssuanceAndAuditFailure(t *testing.T) {
	const password = "NHA141 synthetic commit and audit password"
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
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := newAccountInput(ctx, "NHA141 Failure Admin", "NHA141 Failure Admin", password, authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	clock := newNHA141Clock(time.Now().UTC().Truncate(time.Microsecond))
	coordinator, err := prepareLocalAuthorization(ctx, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthHandler(store, coordinator, nil)
	if err != nil {
		t.Fatal(err)
	}
	login := func() (*connect.Response[authv1.StartLoginResponse], error) {
		return handler.StartLogin(ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: "NHA141 Failure Admin", Password: password}))
	}
	first, err := login()
	if err != nil || first == nil || first.Msg.GetDecision().GetAuthenticated() == nil {
		t.Fatalf("baseline login did not issue a session: %v", err)
	}
	tokens := first.Msg.GetDecision().GetAuthenticated()
	sessionID := tokens.GetSession().GetId().GetValue()
	access := tokens.GetAccessToken()
	refresh := tokens.GetRefreshToken()
	if sessionID == "" || access == "" || refresh == "" {
		t.Fatal("baseline login issued incomplete credentials")
	}
	accountID, err := admin.ID.Value()
	if err != nil {
		t.Fatal(err)
	}
	auth := &sessionAuth{store: store, coordinator: coordinator}
	admit := func(access string) (context.Context, func(), error) {
		request := httptest.NewRequest(http.MethodGet, "/nha141/failure", nil)
		request.TLS = &tls.ConnectionState{}
		request.Header.Set("Authorization", "Bearer "+access)
		_, leaseCtx, release, err := auth.AuthenticateWithLease(ctx, request)
		return leaseCtx, release, err
	}
	lease, release, err := admit(access)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ddl := func(sql string) {
		t.Helper()
		if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertState := func(label string) {
		t.Helper()
		var sessions, generation int
		if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_sessions WHERE account_id=$1`, accountID).Scan(&sessions); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT current_generation FROM public.auth_sessions WHERE id=$1`, sessionID).Scan(&generation)
		}); err != nil {
			t.Fatal(err)
		}
		if sessions != 1 || generation != 1 {
			t.Fatalf("%s changed committed session state: sessions=%d generation=%d", label, sessions, generation)
		}
	}
	assertQuarantined := func(label string) {
		t.Helper()
		key, _ := admin.ID.Value()
		coordinator.mu.Lock()
		quarantined := coordinator.accounts[key] != nil && coordinator.accounts[key].quarantined
		coordinator.mu.Unlock()
		if !quarantined {
			t.Fatalf("%s failed to quarantine account after unknown commit", label)
		}
	}
	// The deferred failure happens after CreateSession has inserted credentials
	// and audit, but before COMMIT can be confirmed to the handler.
	ddl(`CREATE FUNCTION public.nha141_refuse_session_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic unknown session create'; END $$;
		CREATE CONSTRAINT TRIGGER nha141_refuse_session_insert AFTER INSERT ON public.auth_sessions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha141_refuse_session_insert()`)
	issued, err := login()
	if issued != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("unknown login commit exposed a response or bearer: responsePresent=%t error=%v", issued != nil, err)
	}
	if lease.Err() == nil {
		t.Fatal("unknown login commit left an existing lease running")
	}
	assertQuarantined("login")
	ddl(`DROP TRIGGER nha141_refuse_session_insert ON public.auth_sessions;
		DROP FUNCTION public.nha141_refuse_session_insert()`)
	assertState("unknown login commit")
	// Admission makes a primary-DB read, settles the indeterminate outcome and
	// proves the original access still exists after this forced rollback.
	lease, release, err = admit(access)
	if err != nil {
		t.Fatalf("authoritative admission after unknown login commit: %v", err)
	}
	defer release()
	ddl(`CREATE FUNCTION public.nha141_refuse_session_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic unknown session refresh'; END $$;
		CREATE CONSTRAINT TRIGGER nha141_refuse_session_update AFTER UPDATE ON public.auth_sessions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha141_refuse_session_update()`)
	rotated, err := handler.RefreshSession(ctx, connect.NewRequest(&authv1.RefreshSessionRequest{RefreshToken: refresh}))
	if rotated != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("unknown refresh commit exposed a response or bearer: responsePresent=%t error=%v", rotated != nil, err)
	}
	if lease.Err() == nil {
		t.Fatal("unknown refresh commit left an existing lease running")
	}
	assertQuarantined("refresh")
	ddl(`DROP TRIGGER nha141_refuse_session_update ON public.auth_sessions;
		DROP FUNCTION public.nha141_refuse_session_update()`)
	assertState("unknown refresh commit")
	if _, after, err := admit(access); err != nil {
		t.Fatalf("original access did not survive forced rollback: %v", err)
	} else {
		after()
	}
	// PostgreSQL remains available; only the audit INSERT fails. Emission and
	// rotation must roll back their credential changes with that audit failure.
	ddl(`CREATE FUNCTION public.nha141_refuse_session_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit failure'; END $$;
		CREATE TRIGGER nha141_refuse_session_audit BEFORE INSERT ON public.auth_audit FOR EACH ROW EXECUTE FUNCTION public.nha141_refuse_session_audit()`)
	issued, err = login()
	if issued != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("audit failure exposed login bearer: responsePresent=%t error=%v", issued != nil, err)
	}
	assertState("audit failure during login")
	rotated, err = handler.RefreshSession(ctx, connect.NewRequest(&authv1.RefreshSessionRequest{RefreshToken: refresh}))
	if rotated != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("audit failure exposed refreshed bearer: responsePresent=%t error=%v", rotated != nil, err)
	}
	assertState("audit failure during refresh")
	ddl(`DROP TRIGGER nha141_refuse_session_audit ON public.auth_audit;
		DROP FUNCTION public.nha141_refuse_session_audit()`)
	if _, after, err := admit(access); err != nil {
		t.Fatalf("audit rollback invalidated original access: %v", err)
	} else {
		after()
	}
	rotated, err = handler.RefreshSession(ctx, connect.NewRequest(&authv1.RefreshSessionRequest{RefreshToken: refresh}))
	if err != nil || rotated == nil || rotated.Msg.GetTokens() == nil {
		t.Fatalf("original refresh was consumed by failed audit: %v", err)
	}
}
