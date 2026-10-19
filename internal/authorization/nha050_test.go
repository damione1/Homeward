package authorization

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA050SourceResourceBoundaries(t *testing.T) {
	account := testAccount(t, "account")
	for _, limit := range []struct {
		name   string
		n      int
		source func(int) string
	}{
		{"entities", MaxEntities, func(n int) string {
			var entries strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&entries, "          - entity_id: entity-%d\n            capabilities: {all: {}}\n", i)
			}
			return "  - scope: {family: actions, operation: write}\n    objects:\n      entities:\n        explicit:\n" + entries.String()
		}},
		{"slots", MaxSlots, func(n int) string {
			var entries strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&entries, "              - {schema_id: core.light.v1, endpoint: slot-%d}\n", i)
			}
			return "  - scope: {family: actions, operation: write}\n    objects:\n      entities:\n        all:\n          capabilities:\n            explicit:\n" + entries.String()
		}},
		{"dashboards", MaxDashboards, func(n int) string {
			var entries strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&entries, "          - dashboard-%d\n", i)
			}
			return "  - scope: {family: dashboards, operation: read}\n    objects:\n      dashboards:\n        explicit:\n" + entries.String()
		}},
	} {
		t.Run(limit.name, func(t *testing.T) {
			for _, extra := range []int{0, 1} {
				source := []byte("schema: homeward.authorization/1\naccount_id: account\ngrants:\n" + limit.source(limit.n+extra))
				_, err := AdmitPolicySource(account, source)
				if (err == nil) != (extra == 0) {
					t.Fatalf("boundary %d: %v", limit.n+extra, err)
				}
			}
		})
	}
}

func TestNHA050WithCurrentUsesPureSnapshotWhileWriterWaits(t *testing.T) {
	account := testAccount(t, "account")
	principal := testPrincipal(t, account)
	source := testSource(t, allEquipment)
	authority := NewAuthority()
	if err := authority.Hydrate([]AccountState{testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1)}); err != nil {
		t.Fatal(err)
	}
	permission := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	object := testEntity(t, "entity", "core.light.v1", "main")
	entered := make(chan struct{})
	continueRead := make(chan struct{})
	readDone := make(chan error, 1)
	writeDone := make(chan error, 1)
	go func() {
		readDone <- authority.WithCurrent(context.Background(), principal, func(current Current) error {
			close(entered)
			<-continueRead
			return current.Authorize(permission, object)
		})
	}()
	<-entered
	empty, _ := EmptySource(account)
	withdrawn := testState(t, empty, authv1.AccountRole_ACCOUNT_ROLE_USER, 2, 2)
	go func() {
		mutation, err := authority.BeginMutation(account)
		if err != nil {
			writeDone <- err
			return
		}
		defer mutation.Release()
		writeDone <- authority.Publish(withdrawn)
	}()
	// TryRLock fails precisely once this writer is waiting behind the callback's
	// reader. A timeout fails a stuck test; no sleep establishes the ordering.
	key, _ := account.Value()
	authority.mu.RLock()
	entry := authority.accounts[key]
	authority.mu.RUnlock()
	deadline := time.Now().Add(3 * time.Second)
	for entry.barrier.TryRLock() {
		entry.barrier.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("writer never waited behind capture")
		}
		runtime.Gosched()
	}
	close(continueRead)
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal("pure callback reacquired its authority", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pure snapshot callback deadlocked behind writer")
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not finish after capture")
	}
	current, err := authority.Current(principal)
	if err != nil {
		t.Fatal(err)
	}
	if current.Authorize(permission, object) == nil {
		t.Fatal("next read retained the withdrawn grant")
	}
}

func testAccount(t *testing.T, text string) contracts.AccountID {
	t.Helper()
	id, err := contracts.ParseAccountID(text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func testPermission(t *testing.T, family authv1.ScopeFamily, op authv1.ScopeOperation) Permission {
	t.Helper()
	p, err := NewPermission(family, op)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testPrincipal(t *testing.T, account contracts.AccountID) Principal {
	t.Helper()
	session, err := contracts.ParseSessionID("synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	p, err := NativePrincipal(account, session)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testSource(t *testing.T, grants string) PolicySource {
	t.Helper()
	source, err := AdmitPolicySource(testAccount(t, "account"), []byte("schema: homeward.authorization/1\naccount_id: account\ngrants:\n"+grants))
	if err != nil {
		t.Fatal(err)
	}
	return source
}
func testState(t *testing.T, source PolicySource, role authv1.AccountRole, revision, rights uint64) AccountState {
	t.Helper()
	hash, err := source.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	version, err := NewVersion(revision, hash)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewAccountState(source, role, authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, rights)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func testEntity(t *testing.T, text, schema, endpoint string) Object {
	t.Helper()
	id, err := domain.ParseEntityID(text)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := NewCapabilitySlot(schema, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	object, err := EntityObject(id, slot)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

const allEquipment = `  - scope: {family: state_registry, operation: read}
    objects:
      entities: {all: {capabilities: {all: {}}}}
  - scope: {family: actions, operation: write}
    objects:
      entities: {all: {capabilities: {all: {}}}}
`

func TestNHA050SourceAdmissionAndCopies(t *testing.T) {
	account := testAccount(t, "account")
	valid := []byte("schema: homeward.authorization/1\naccount_id: account\ngrants: []\n")
	for _, change := range []struct {
		name string
		data string
	}{
		{"missing-grants", "schema: homeward.authorization/1\naccount_id: account\n"},
		{"null-grants", "schema: homeward.authorization/1\naccount_id: account\ngrants: null\n"},
		{"duplicate-key", string(valid) + "grants: []\n"},
		{"unknown-field", string(valid) + "is_admin: true\n"},
		{"anchor", "schema: homeward.authorization/1\naccount_id: &account account\ngrants: []\n"},
		{"trailing-document", string(valid) + "---\n{}\n"},
		{"numeric-account", "schema: homeward.authorization/1\naccount_id: 12\ngrants: []\n"},
		{"different-account", strings.ReplaceAll(string(valid), "account_id: account", "account_id: other")},
		{"unknown-scope", "schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: future, operation: read}\n    objects: {entities: {explicit: []}}\n"},
		{"unsupported-operation", "schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: state_registry, operation: write}\n    objects: {entities: {explicit: []}}\n"},
		{"two-selectors", "schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {all: {}, explicit: []}}\n"},
		{"self-declared-owner", "schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: dashboards, operation: write}\n    objects: {dashboards: {own: {owner_id: other}}}\n"},
	} {
		t.Run(change.name, func(t *testing.T) {
			if _, err := AdmitPolicySource(account, []byte(change.data)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("admitted rejected source: %v", err)
			}
		})
	}
	source, err := AdmitPolicySource(account, valid)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := source.SHA256()
	valid[0] = '!'
	copyBytes, _ := source.Bytes()
	copyBytes[0] = '!'
	after, _ := source.SHA256()
	got, _ := source.Bytes()
	if before != after || bytes.HasPrefix(got, []byte("!")) {
		t.Fatal("policy source aliases caller bytes")
	}
	if _, err := AdmitPolicySource(account, bytes.Repeat([]byte("x"), MaxSourceBytes+1)); err == nil {
		t.Fatal("oversize admitted")
	}
	if _, err := EmptySource(account); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPermission(0, authv1.ScopeOperation_SCOPE_OPERATION_READ); err == nil {
		t.Fatal("zero family admitted")
	}
	if _, err := NewPermission(authv1.ScopeFamily(999), authv1.ScopeOperation_SCOPE_OPERATION_READ); err == nil {
		t.Fatal("future family admitted")
	}
	if _, _, err := (Permission{}).Values(); err == nil {
		t.Fatal("zero permission admitted")
	}
}

func TestNHA050EffectiveIntersectionAndIntrinsicAdministration(t *testing.T) {
	source := testSource(t, allEquipment+`  - scope: {family: dashboards, operation: read}
    objects: {dashboards: {own: {}}}
  - scope: {family: dashboards, operation: write}
    objects: {dashboards: {own: {}}}
`)
	account := testAccount(t, "account")
	native := testPrincipal(t, account)
	read := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	set, _ := NewScopeSet([]Permission{read})
	credential, _ := contracts.ParseMcpKeyID("scoped")
	scoped, err := ScopedPrincipal(account, credential, set)
	if err != nil {
		t.Fatal(err)
	}
	entity := testEntity(t, "entity", "core.light.v1", "main")
	dashboardID, _ := ParseDashboardID("dashboard")
	own, _ := DashboardObject(dashboardID, account)
	other, _ := DashboardObject(dashboardID, testAccount(t, "other"))
	for _, role := range []authv1.AccountRole{authv1.AccountRole_ACCOUNT_ROLE_ADMIN, authv1.AccountRole_ACCOUNT_ROLE_USER} {
		state := testState(t, source, role, 1, 1)
		for family := authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY; family <= authv1.ScopeFamily_SCOPE_FAMILY_BACKUPS; family++ {
			for op := authv1.ScopeOperation_SCOPE_OPERATION_READ; op <= authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER; op++ {
				permission := testPermission(t, family, op)
				object := entity
				if family == authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS {
					object = own
				}
				if family == authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS || family == authv1.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS || family == authv1.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION {
					object, _ = AdministrativeObject(family)
				}
				wantNative := family == authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY && op == authv1.ScopeOperation_SCOPE_OPERATION_READ || family == authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS && op == authv1.ScopeOperation_SCOPE_OPERATION_WRITE || family == authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS && (op == authv1.ScopeOperation_SCOPE_OPERATION_READ || op == authv1.ScopeOperation_SCOPE_OPERATION_WRITE) || role == authv1.AccountRole_ACCOUNT_ROLE_ADMIN && (family == authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS || family == authv1.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS || family == authv1.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION)
				if got := Evaluate(native, state, permission, object) == nil; got != wantNative {
					t.Fatalf("role %v family %v op %v: %v want %v", role, family, op, got, wantNative)
				}
				wantScoped := permission == read
				if got := Evaluate(scoped, state, permission, object) == nil; got != wantScoped {
					t.Fatalf("scope intersection role %v family %v op %v", role, family, op)
				}
			}
		}
		permission := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
		if Evaluate(native, state, permission, other) == nil {
			t.Fatal("own selector admitted someone else's dashboard")
		}
	}
	empty, _ := EmptySource(account)
	state := testState(t, empty, authv1.AccountRole_ACCOUNT_ROLE_ADMIN, 1, 1)
	adminPermission := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	adminObject, _ := AdministrativeObject(authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS)
	if Evaluate(native, state, adminPermission, adminObject) != nil {
		t.Fatal("empty policy locked administrator out")
	}
	if Evaluate(native, state, read, entity) == nil {
		t.Fatal("empty seed exposed equipment")
	}
	emptySet, _ := NewScopeSet(nil)
	limited, _ := ScopedPrincipal(account, credential, emptySet)
	if Evaluate(limited, state, adminPermission, adminObject) == nil {
		t.Fatal("empty credential ceiling became all")
	}
}

func TestNHA050PermitRevocationNoOpAndRegrant(t *testing.T) {
	account := testAccount(t, "account")
	principal := testPrincipal(t, account)
	source := testSource(t, allEquipment)
	state := testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1)
	authority := NewAuthority()
	if err := authority.Hydrate([]AccountState{state}); err != nil {
		t.Fatal(err)
	}
	permission := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	object := testEntity(t, "entity", "core.light.v1", "main")
	permit, err := authority.Permit(context.Background(), principal, permission, object)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	sub, err := authority.Subscribe(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	first, _ := authority.Current(principal)
	revision, binding, _ := first.Binding()
	data, _ := source.Bytes()
	same, err := AdmitPolicySource(account, append(data, []byte("# same rights\n")...))
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Publish(testState(t, same, authv1.AccountRole_ACCOUNT_ROLE_USER, 2, 1)); err != nil {
		t.Fatal(err)
	}
	current, _ := authority.Current(principal)
	afterRevision, afterBinding, _ := current.Binding()
	if revision != afterRevision || binding != afterBinding || !permit.Ready() {
		t.Fatal("source-only edit changed effective rights")
	}
	select {
	case <-sub.Changes():
		t.Fatal("source-only edit notified rights change")
	default:
	}
	empty, _ := EmptySource(account)
	if err := authority.Publish(testState(t, empty, authv1.AccountRole_ACCOUNT_ROLE_USER, 3, 3)); err != nil {
		t.Fatal(err)
	}
	if permit.Ready() || !errors.Is(context.Cause(permit.Context()), ErrDenied) {
		t.Fatal("revoked permit remained ready")
	}
	select {
	case <-sub.Changes():
	default:
		t.Fatal("idle subscriber not notified")
	}
	if err := authority.Publish(testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 4, 4)); err != nil {
		t.Fatal(err)
	}
	if permit.Ready() {
		t.Fatal("regrant resurrected prepared command")
	}
	if err := authority.Publish(state); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale publication admitted: %v", err)
	}
	if err := authority.QuarantineAccount(account); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Current(principal); !errors.Is(err, ErrUnavailable) {
		t.Fatal("quarantine served old snapshot")
	}
}

func TestNHA050ExplicitSlotAndCredentialBinding(t *testing.T) {
	source := testSource(t, `  - scope: {family: state_registry, operation: read}
    objects:
      entities:
        explicit:
          - entity_id: entity
            capabilities: {explicit: [{schema_id: core.light.v1, endpoint: main}]}
`)
	account := testAccount(t, "account")
	native := testPrincipal(t, account)
	state := testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1)
	read := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if Evaluate(native, state, read, testEntity(t, "entity", "core.light.v1", "main")) != nil || Evaluate(native, state, read, testEntity(t, "entity", "core.light.v1", "private")) == nil {
		t.Fatal("slot boundary lost")
	}
	firstID, _ := contracts.ParseMcpKeyID("first")
	secondID, _ := contracts.ParseMcpKeyID("second")
	ceiling, _ := NewScopeSet([]Permission{read})
	first, _ := ScopedPrincipal(account, firstID, ceiling)
	second, _ := ScopedPrincipal(account, secondID, ceiling)
	authority := NewAuthority()
	if err := authority.Hydrate([]AccountState{state}); err != nil {
		t.Fatal(err)
	}
	x, _ := authority.Current(first)
	y, _ := authority.Current(second)
	_, a, _ := x.Binding()
	_, b, _ := y.Binding()
	if a == b {
		t.Fatal("credentials shared view binding")
	}
}

func TestNHA050EmptySelectorsNormalizeToNoRights(t *testing.T) {
	account := testAccount(t, "account")
	empty, err := EmptySource(account)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := empty.SemanticSHA256()
	for _, grants := range []string{
		"  - scope: {family: state_registry, operation: read}\n    objects: {entities: {explicit: []}}\n",
		"  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {explicit: []}}}}\n",
		"  - scope: {family: dashboards, operation: write}\n    objects: {dashboards: {explicit: []}}\n",
		"  - scope: {family: state_registry, operation: read}\n    objects: {entities: {explicit: [{entity_id: entity, capabilities: {explicit: []}}]}}\n",
	} {
		source := testSource(t, grants)
		hash, _ := source.SemanticSHA256()
		if hash != base {
			t.Fatal("empty selector changed effective rights")
		}
	}
}
