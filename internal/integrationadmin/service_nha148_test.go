package integrationadmin

import (
	"context"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

func TestNHA148SharedServiceUsesIntegrationCeilingAndCurrentHumanRole(t *testing.T) {
	account, _ := contracts.ParseAccountID("nha148-admin")
	source, err := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha148-admin\ngrants: []\n"))
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
	authority := authorization.NewAuthority()
	state, err := authorization.NewAccountState(source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Hydrate([]authorization.AccountState{state}); err != nil {
		t.Fatal(err)
	}
	service, err := New(&configdocuments.Service{}, authority, func(context.Context, domain.IntegrationInstanceID) (*obs.IntegrationReportPresence, *obs.IntegrationSupervision, error) {
		t.Fatal("permission check attempted health I/O")
		return nil, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := contracts.ParseMcpKeyID("nha148-key")
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	read, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, auth.ScopeOperation_SCOPE_OPERATION_READ)
	for _, test := range []struct {
		permissions []authorization.Permission
		allowed     bool
	}{{nil, false}, {[]authorization.Permission{read}, false}, {[]authorization.Permission{permission}, true}} {
		permissions := test.permissions
		ceiling, err := authorization.NewScopeSet(permissions)
		if err != nil {
			t.Fatal(err)
		}
		principal, err := authorization.ScopedPrincipal(account, key, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := authorization.WithPrincipal(context.Background(), principal)
		if err != nil {
			t.Fatal(err)
		}
		permit, _, err := service.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
		if (err == nil) != test.allowed {
			t.Fatal("scoped ceiling bypass or accidental Native-only restriction", err)
		}
		if permit != nil {
			if !permit.Ready() {
				t.Fatal("authorized scoped admin not admitted")
			}
			permit.Release()
		}
	}
	userState, err := authorization.NewAccountState(source, auth.AccountRole_ACCOUNT_ROLE_USER, auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, 1)
	if err != nil {
		t.Fatal(err)
	}
	authority = authorization.NewAuthority()
	if err := authority.Hydrate([]authorization.AccountState{userState}); err != nil {
		t.Fatal(err)
	}
	service.authority = authority
	ceiling, _ := authorization.NewScopeSet([]authorization.Permission{permission})
	principal, _ := authorization.ScopedPrincipal(account, key, ceiling)
	ctx, _ := authorization.WithPrincipal(context.Background(), principal)
	if permit, _, err := service.permit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER); err == nil {
		permit.Release()
		t.Fatal("scope elevated user role")
	}
}
