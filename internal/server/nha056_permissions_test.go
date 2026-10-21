package server

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/observability"
)

func TestNHA056AdministrativeReadUsesCurrentRoleExactCeilingAndBinding(t *testing.T) {
	account, err := contracts.ParseAccountID("nha056-policy-account")
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
	version, err := authorization.NewVersion(1, hash)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authorization.NewAccountState(source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, 1)
	if err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	if err := authority.Hydrate([]authorization.AccountState{state}); err != nil {
		t.Fatal(err)
	}
	h := &observationHandler{runtime: &observationRuntime{authority: authority}}
	key, err := contracts.ParseMcpKeyID("nha056-admitted-scoped-key")
	if err != nil {
		t.Fatal(err)
	}
	contextFor := func(principal authorization.Principal) context.Context {
		actor, err := principal.Actor()
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := authorization.WithPrincipal(context.WithValue(context.Background(), actorContextKey{}, actor), principal)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	var readContext context.Context
	for _, operation := range []auth.ScopeOperation{auth.ScopeOperation_SCOPE_OPERATION_READ, auth.ScopeOperation_SCOPE_OPERATION_WRITE, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER} {
		permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, operation)
		if err != nil {
			t.Fatal(err)
		}
		scopes, err := authorization.NewScopeSet([]authorization.Permission{permission})
		if err != nil {
			t.Fatal(err)
		}
		principal, err := authorization.ScopedPrincipal(account, key, scopes)
		if err != nil {
			t.Fatal(err)
		}
		ctx := contextFor(principal)
		if operation == auth.ScopeOperation_SCOPE_OPERATION_READ {
			readContext = ctx
		}
		called := false
		err = h.withCurrent(ctx, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(observability.ReadBinding) error { called = true; return nil })
		if (operation == auth.ScopeOperation_SCOPE_OPERATION_READ) != (err == nil && called) {
			t.Fatal("administrative reader weakened the exact READ ceiling")
		}
	}
	var binding observability.ReadBinding
	if err := h.withCurrent(readContext, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(value observability.ReadBinding) error { binding = value; return nil }); err != nil {
		t.Fatal(err)
	}
	// This is the actual second check used after SQL, with the current authority
	// changed in between. There is no cached role or second policy implementation.
	changedVersion, err := authorization.NewVersion(2, hash)
	if err != nil {
		t.Fatal(err)
	}
	downgraded, err := authorization.NewAccountState(source, auth.AccountRole_ACCOUNT_ROLE_USER, auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, changedVersion, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Publish(downgraded); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := h.withCurrent(readContext, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, &binding, func(observability.ReadBinding) error { called = true; return nil }); err == nil || called {
		t.Fatal("role change let a captured response escape")
	}
	if err := h.withCurrent(readContext, auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, nil, func(observability.ReadBinding) error { return nil }); err == nil {
		t.Fatal("user administrative read admitted")
	}
	h.runtime.authority = authorization.NewAuthority()
	called = false
	if err := h.withCurrent(readContext, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, nil, func(observability.ReadBinding) error { called = true; return nil }); !errors.Is(err, authorization.ErrUnavailable) || called {
		t.Fatal("unavailable current authority exposed diagnostics", err)
	}
}
