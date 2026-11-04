package server

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

// This explicit synthetic authority tests the owner/response lifetime boundary;
// it does not claim authentication, PostgreSQL or HTTP/2 coverage.
func authReadAuthorityFixture(t *testing.T, ctx context.Context) (context.Context, *sessionCoordinator, func()) {
	t.Helper()
	account, err := contracts.ParseAccountID("synthetic-auth-read-admin")
	if err != nil {
		t.Fatal(err)
	}
	session, err := contracts.ParseSessionID("synthetic-auth-read-session")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authorization.NativePrincipal(account, session)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = authorization.WithPrincipal(context.WithValue(ctx, actorContextKey{}, actor), principal)
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.EmptySource(account)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := source.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	state := func(role native.AccountRole, revision uint64) authorization.AccountState {
		version, err := authorization.NewVersion(revision, hash)
		if err != nil {
			t.Fatal(err)
		}
		value, err := authorization.NewAccountState(source, role, native.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, revision)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	authority := authorization.NewAuthority()
	if err := authority.Hydrate([]authorization.AccountState{state(native.AccountRole_ACCOUNT_ROLE_ADMIN, 1)}); err != nil {
		t.Fatal(err)
	}
	return ctx, &sessionCoordinator{authority: authority}, func() {
		if err := authority.Publish(state(native.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNHA203AuthReadKeepsNativePermissionUntilResponseClose(t *testing.T) {
	ctx, scope := newPublicResponseScope(t.Context(), nil)
	defer scope.close()
	ctx, coordinator, withdraw := authReadAuthorityFixture(t, ctx)
	admission, err := admitAuthAdministrativeRead(ctx, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authReadResponse(ctx, admission, &api.ListAccountsResponse{}); err != nil {
		t.Fatal(err)
	}
	admission.close() // handler return must no longer release the owner.
	if !admission.permit.Ready() {
		t.Fatal("permission released before response completion")
	}
	withdraw()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("permission withdrawal did not cancel response")
	}
	if admission.permit.Ready() {
		t.Fatal("withdrawn permission remained valid")
	}
	scope.close()
	if admission.permit.Context().Err() == nil {
		t.Fatal("permit leaked beyond HTTP completion")
	}
}

func TestNHA203AuthReadRejectsWithdrawalBeforeFinalAdmission(t *testing.T) {
	ctx, coordinator, withdraw := authReadAuthorityFixture(t, t.Context())
	admission, err := admitAuthAdministrativeRead(ctx, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.close()
	withdraw()
	if response, err := authReadResponse(ctx, admission, &api.ListAccountsResponse{}); err == nil || response != nil {
		t.Fatal("materialized response admitted after native permission withdrawal")
	}
}

func TestNHA203AuthReadDirectCallKeepsLocalCleanup(t *testing.T) {
	ctx, coordinator, _ := authReadAuthorityFixture(t, t.Context())
	admission, err := admitAuthAdministrativeRead(ctx, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authReadResponse(ctx, admission, &api.ListAccountsResponse{}); err != nil {
		t.Fatal(err)
	}
	if admission.transferred {
		t.Fatal("direct caller transferred cleanup without a response owner")
	}
	admission.close()
	if admission.permit.Ready() {
		t.Fatal("direct caller leaked native permission")
	}
}
