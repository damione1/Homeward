package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHF023CanceledAuthenticationPolicyReadKeepsOtherAuthorizedLeases(t *testing.T) {
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	pg, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, pg)
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
	handler := &accountHandler{store: store, coordinator: coordinator}
	created, err := handler.BootstrapAdmin(ctx, connect.NewRequest(&auth.BootstrapAdminRequest{
		LoginName: "NHF023 cancellation", DisplayName: "Synthetic cancellation", Password: "NHF023 synthetic policy-read cancellation password"}))
	if err != nil {
		t.Fatal(err)
	}
	account, err := contracts.ParseAccountID(created.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	session, err := contracts.ParseSessionID("nhf023-existing-synthetic-stream")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authorization.NativePrincipal(account, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			guard, err := coordinator.enter(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.release()
			stream, lease := guard.register(ctx, storage.SessionOwner{Account: account, Session: session})
			defer lease.release()
			if _, err := coordinator.authority.Current(principal); err != nil {
				t.Fatal(err)
			}
			var readCtx context.Context
			var stop context.CancelFunc
			if deadline {
				readCtx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			} else {
				readCtx, stop = context.WithCancel(ctx)
			}
			stop()
			if err := guard.refreshForAuthentication(readCtx); !errors.Is(err, readCtx.Err()) {
				t.Fatal("canceled read was not rejected", err)
			}
			if _, err := coordinator.authority.Current(principal); err != nil {
				t.Fatal("one canceled read quarantined a previously authorized account", err)
			}
			if stream.Err() != nil {
				t.Fatal("one canceled read terminated another stream", context.Cause(stream))
			}
		})
	}
	// A genuine source failure still quarantines the account and terminates
	// existing leases. Cancellation handling must not turn into fail-open reads.
	guard, err := coordinator.enter(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.release()
	stream, lease := guard.register(ctx, storage.SessionOwner{Account: account, Session: session})
	defer lease.release()
	store.Close()
	if err := guard.refreshForAuthentication(ctx); err == nil {
		t.Fatal("closed source admitted authentication")
	}
	if _, err := coordinator.authority.Current(principal); !errors.Is(err, authorization.ErrUnavailable) {
		t.Fatal("genuine source failure kept policy authority usable", err)
	}
	if stream.Err() == nil {
		t.Fatal("genuine source failure kept an old lease usable")
	}
}
