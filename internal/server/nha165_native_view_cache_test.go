package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

type nha165GrantFixture struct {
	a         *policyViewAuthorizer
	ctx       context.Context
	principal authorization.Principal
	actor     domain.ActorID
	account   contracts.AccountID
	source    authorization.PolicySource
	record    integrations.RegistryRecord
	session   *integrations.Session
	binding   integrations.Binding
	service   *view.Service
}

func nha165GrantSetup(t *testing.T) nha165GrantFixture {
	t.Helper()
	ctx := context.Background()
	host, fixture, e := fake.NewCommandFixture()
	if e != nil {
		t.Fatal(e)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha165-cache-native")
	session, e := host.OpenSession(instance)
	if e != nil {
		t.Fatal(e)
	}
	driver, e := fake.NewDriver(time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if e = driver.Start(ctx, session); e != nil {
		t.Fatal(e)
	}
	binding, e := driver.Binding()
	if e != nil {
		t.Fatal(e)
	}
	device, entity, e := binding.IDs()
	if e != nil {
		t.Fatal(e)
	}
	_, key, e := binding.Source()
	if e != nil {
		t.Fatal(e)
	}
	alias, _ := domain.ParseHAAlias("switch.nha165_cache")
	record, e := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); e != nil {
		t.Fatal(e)
	}
	account, _ := contracts.ParseAccountID("nha165-cache-account")
	source, e := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha165-cache-account\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	authority := authorization.NewAuthority()
	if e = authority.Hydrate([]authorization.AccountState{nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_USER, 1)}); e != nil {
		t.Fatal(e)
	}
	sessionID, _ := contracts.ParseSessionID("nha165-cache-session")
	principal, e := authorization.NativePrincipal(account, sessionID)
	if e != nil {
		t.Fatal(e)
	}
	actor, e := principal.Actor()
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = authorization.WithPrincipal(ctx, principal)
	if e != nil {
		t.Fatal(e)
	}
	a := &policyViewAuthorizer{authority: authority, source: fixture.Sequencer()}
	stamp, _ := view.NewConfigStamp(1, [32]byte{1})
	service, e := view.NewService(fixture.Sequencer(), a, nha050ViewConfig{stamp}, sequencer.RealClock{})
	if e != nil {
		t.Fatal(e)
	}
	return nha165GrantFixture{a, ctx, principal, actor, account, source, record, session, binding, service}
}
func (f nha165GrantFixture) rows(t *testing.T, want int) {
	t.Helper()
	p, e := f.service.OpenView(f.ctx, f.actor, view.All(), 100)
	if e != nil || len(p.Items) != want {
		t.Fatalf("native current view rows=%d want=%d: %v", len(p.Items), want, e)
	}
}

func TestNHA165NativeViewCacheRetainsPolicyAndCredentialFences(t *testing.T) {
	f := nha165GrantSetup(t)
	f.rows(t, 1)
	// A different scoped credential cannot borrow the native cached grant.
	historyRead, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	scopes, _ := authorization.NewScopeSet([]authorization.Permission{historyRead})
	id, _ := contracts.ParseMcpKeyID("nha165-no-state-ceiling")
	limited, e := authorization.ScopedPrincipal(f.account, id, scopes)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := authorization.WithPrincipal(context.Background(), limited)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.a.CurrentGrant(ctx, f.actor); !errors.Is(e, authorization.ErrDenied) {
		t.Fatalf("scoped ceiling borrowed native access: %v", e)
	}
	// A known policy commit changes rights even with identical equipment.
	empty, e := authorization.EmptySource(f.account)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.a.authority.Publish(nha164AuthorityState(t, empty, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); e != nil {
		t.Fatal(e)
	}
	f.rows(t, 0)
	if e = f.a.authority.Publish(nha164AuthorityState(t, f.source, auth.AccountRole_ACCOUNT_ROLE_USER, 3)); e != nil {
		t.Fatal(e)
	}
	f.rows(t, 1)
	if e = f.a.authority.Quarantine(f.principal); e != nil {
		t.Fatal(e)
	}
	if _, e = f.a.CurrentGrant(f.ctx, f.actor); !errors.Is(e, authorization.ErrUnavailable) {
		t.Fatalf("cached grant escaped quarantine: %v", e)
	}
}
func TestNHA165NativeViewCacheRetainsCatalogueAndPublicationFences(t *testing.T) {
	f := nha165GrantSetup(t)
	f.rows(t, 1)
	position, e := f.a.source.CataloguePosition()
	if e != nil {
		t.Fatal(e)
	}
	// A state observation does not change the readable catalogue or require a
	// full850entity grant expansion on every frame.
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value, at := domain.NewSwitchValue(true), time.Now()
	observation, e := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.session.Publish(f.ctx, f.binding, observation); e != nil {
		t.Fatal(e)
	}
	same, e := f.a.source.CataloguePosition()
	if e != nil || same != position {
		t.Fatal("state advanced catalogue fence", e)
	}
	f.rows(t, 1)
	publication, e := f.a.source.BeginConfigurationPublication()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.a.CurrentGrant(f.ctx, f.actor); !errors.Is(e, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("cached grant escaped configuration capture barrier: %v", e)
	}
	if e = publication.Release(); e != nil {
		t.Fatal(e)
	}
	f.rows(t, 1)
	_, entity, _ := f.binding.IDs()
	if e = f.a.source.WithdrawDiscovery(f.ctx, entity); e != nil {
		t.Fatal(e)
	}
	f.rows(t, 0)
	changed, e := f.a.source.CataloguePosition()
	if e != nil || changed == position {
		t.Fatal("capability withdrawal reused old catalogue fence", e)
	}
	f.a.source.InvalidateRegistry()
	if _, e = f.a.CurrentGrant(f.ctx, f.actor); !errors.Is(e, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("cached grant escaped registry invalidation: %v", e)
	}
	if e = f.a.source.PublishRegistry(f.ctx, []integrations.RegistryRecord{f.record}); e != nil {
		t.Fatal(e)
	}
	recovered, e := f.a.source.CataloguePosition()
	if e != nil || recovered == changed {
		t.Fatal("reload reused withdrawn catalogue fence", e)
	}
	f.rows(t, 0)
}
func TestNHA165NativeViewCacheIsBoundedAcrossCredentialFamilies(t *testing.T) {
	f := nha165GrantSetup(t)
	for i := 0; i < policyViewGrantCacheEntries*3; i++ {
		session, _ := contracts.ParseSessionID(fmt.Sprintf("nha165-native-family-%03d", i))
		p, e := authorization.NativePrincipal(f.account, session)
		if e != nil {
			t.Fatal(e)
		}
		ctx, e := authorization.WithPrincipal(context.Background(), p)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.a.CurrentGrant(ctx, f.actor); e != nil {
			t.Fatal(e)
		}
	}
	f.a.grantMu.Lock()
	count, bytes := len(f.a.grantCache), f.a.grantBytes
	f.a.grantMu.Unlock()
	if count > policyViewGrantCacheEntries || bytes > policyViewGrantCacheBytes {
		t.Fatalf("unbounded grant retention: entries=%d bytes=%d", count, bytes)
	}
	// Eviction affects only recomputation, never admission or readable rows.
	f.rows(t, 1)
}
