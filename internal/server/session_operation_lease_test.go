package server

import (
	"context"
	"errors"
	"fmt"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA201OperationLeaseOutlivesHTTPButRetainsExactCredentialRevocation(t *testing.T) {
	f := nha165GrantSetup(t)
	coordinator, err := newSessionCoordinator(&storage.Store{})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.authority = f.a.authority
	guard, err := coordinator.enter(f.ctx, f.account)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := contracts.ParseSessionID("nha165-cache-session")
	httpContext, cancelHTTP := context.WithCancel(f.ctx)
	defer cancelHTTP()
	requestContext, original := guard.register(httpContext, storage.SessionOwner{Account: f.account, Session: session})
	original.deadline = time.Now().Add(45 * time.Second)
	original.nativePrincipal = f.principal
	guard.release()
	defer original.release()
	until := time.Now().Add(30 * time.Second)
	// A different native session of the same account cannot borrow this lease.
	otherSession, _ := contracts.ParseSessionID("synthetic-other-session")
	otherPrincipal, _ := authorization.NativePrincipal(f.account, otherSession)
	foreign, err := authorization.WithPrincipal(requestContext, otherPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if _, release, err := retainOperationLease(foreign, until); err == nil || release != nil {
		t.Fatal("another credential borrowed the source lease")
	}
	retained, release, err := retainOperationLease(requestContext, until)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if retained.Value(sessionLeaseContextKey{}) == original {
		t.Fatal("operation reused the request's releasable lease")
	}
	original.release()
	cancelHTTP()
	if retained.Err() != nil {
		t.Fatal("returning the HTTP handle cancelled admitted execution")
	}
	guard, err = coordinator.enter(context.Background(), f.account)
	if err != nil {
		t.Fatal(err)
	}
	guard.cancelLeases(nil, &session)
	guard.release()
	if retained.Err() == nil {
		t.Fatal("session revocation did not cancel the retained operation lease")
	}
	release()
	release()
	coordinator.mu.Lock()
	accounts := len(coordinator.accounts)
	coordinator.mu.Unlock()
	if accounts != 0 {
		t.Fatal("retained credential leaked coordinator storage")
	}
	if _, release, err := retainOperationLease(requestContext, until); err == nil || release != nil {
		t.Fatal("released source created another operation lease")
	}
}

func TestNHA201OperationLeasePreservesNativeAndMCPCredentialExpiry(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, credentialFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("mcp=%t/credential_first=%t", scoped, credentialFirst), func(t *testing.T) {
				f := nha165GrantSetup(t)
				now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				clock := newNHA141Clock(now)
				coordinator, err := newSessionCoordinatorWithClock(&storage.Store{}, clock)
				if err != nil {
					t.Fatal(err)
				}
				coordinator.authority = f.a.authority
				principal := f.principal
				var key contracts.McpKeyID
				if scoped {
					permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, auth.ScopeOperation_SCOPE_OPERATION_READ)
					scopes, _ := authorization.NewScopeSet([]authorization.Permission{permission})
					key, _ = contracts.ParseMcpKeyID("synthetic-operation-key")
					principal, err = authorization.ScopedPrincipal(f.account, key, scopes)
					if err != nil {
						t.Fatal(err)
					}
				}
				ctx, err := authorization.WithPrincipal(context.Background(), principal)
				if err != nil {
					t.Fatal(err)
				}
				guard, err := coordinator.enter(ctx, f.account)
				if err != nil {
					t.Fatal(err)
				}
				originalCtx, original := guard.register(ctx, storage.SessionOwner{Account: f.account})
				original.deadline = now.Add(30 * time.Second)
				if credentialFirst {
					original.deadline = now.Add(2 * time.Second)
				}
				if scoped {
					original.mcpKey, original.mcpPrincipal = key, principal
					current, err := coordinator.authority.Current(principal)
					if err != nil {
						t.Fatal(err)
					}
					_, original.mcpBinding, err = current.Binding()
					if err != nil {
						t.Fatal(err)
					}
				} else {
					original.nativePrincipal = principal
				}
				guard.release()
				defer original.release()
				until := now.Add(2 * time.Second)
				if credentialFirst {
					until = now.Add(30 * time.Second)
				}
				retained, release, err := retainOperationLease(originalCtx, until)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				clock.Advance(now.Add(2 * time.Second))
				if retained.Err() == nil {
					t.Fatal("retained lease ignored earliest deadline")
				}
				if credentialFirst {
					var cause authorizationTerminationCause
					if !errors.As(context.Cause(retained), &cause) || cause.reason != auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED {
						t.Fatal("credential expiry lost its cause", context.Cause(retained))
					}
				} else if !errors.Is(context.Cause(retained), context.DeadlineExceeded) {
					t.Fatal("command deadline became a credential expiration", context.Cause(retained))
				}
			})
		}
	}
}
