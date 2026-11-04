package server

import (
	"context"
	obsv2 "github.com/damione1/Homeward/gen/home/api/v2"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA056ActualTLSAuditReadWithholdsSQLResultAfterAccountDisabled(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	run := nha162StartDefaultRun(t, cfg.startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 SQL revoked synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 SQL owner", password)
	authentication := apiv2connect.NewAuthServiceClient(client, base)
	owner := nha162RunLogin(t, ctx, authentication, "NHA056 SQL owner", password).AccessToken
	accounts := apiv2connect.NewAccountServiceClient(client, base)
	created, err := accounts.CreateAccount(ctx, nha056SessionRequest(&obsv2.CreateAccountRequest{Parent: nha203ObservationHome(t, ctx, client, base, owner), Account: &obsv2.Account{LoginName: "NHA056 SQL reader", DisplayName: "NHA056 SQL reader", Role: obsv2.AccountRole_ACCOUNT_ROLE_ADMIN, InitialPassword: password}}, owner))
	if err != nil {
		t.Fatal(err)
	}
	readerToken := nha162RunLogin(t, ctx, authentication, "NHA056 SQL reader", password).AccessToken
	api := apiv2connect.NewObservabilityServiceClient(client, base)
	home := nha203ObservationHome(t, ctx, client, base, readerToken)
	if _, err := api.QueryAudits(ctx, nha056SessionRequest(&obsv2.QueryAuditsRequest{Parent: home, Source: obsv2.ObservationAuditSource_OBSERVATION_AUDIT_SOURCE_SECRET_ACCESS}, readerToken)); err != nil {
		t.Fatal("active administrative reader was not admitted", err)
	}
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	probe, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	blocker := nha030ServerObserver(t, cluster.Port)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// This table is only read after the observability handler's first current
	// permission check. Its lock does not block auth_audit transport inserts or
	// the real account mutation, so this witnesses the SQL handoff itself.
	if _, err := tx.Exec(ctx, "LOCK TABLE public.secret_access_audit IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		response *connect.Response[obsv2.QueryAuditsResponse]
		err      error
	}
	returned := make(chan result, 1)
	go func() {
		response, err := api.QueryAudits(ctx, nha056SessionRequest(&obsv2.QueryAuditsRequest{Parent: home, Source: obsv2.ObservationAuditSource_OBSERVATION_AUDIT_SOURCE_SECRET_ACCESS}, readerToken))
		returned <- result{response, err}
	}()
	nha162WaitSQLBlockedBy(t, ctx, probe, blockerPID, "%SELECT id FROM public.secret_access_audit%")
	if _, err := accounts.UpdateAccount(ctx, nha056SessionRequest(&obsv2.UpdateAccountRequest{Account: &obsv2.Account{Name: created.Msg.Name, Etag: created.Msg.Etag, Access: obsv2.AccountAccess_ACCOUNT_ACCESS_DISABLED}}, owner)); err != nil {
		t.Fatal("actual concurrent disable failed", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-returned:
		if got.response != nil || connect.CodeOf(got.err) != connect.CodePermissionDenied {
			t.Fatal("SQL response escaped after current rights changed", got.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("bounded SQL reader did not join")
	}
	if _, err := api.GetHealth(ctx, nha056SessionRequest(&obsv2.GetHealthRequest{Name: home + "/health"}, readerToken)); err == nil {
		t.Fatal("disabled native session remained usable")
	}
}
