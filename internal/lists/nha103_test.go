package lists

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha103Fixture struct {
	ctx       context.Context
	store     *storage.Store
	service   *Service
	authority *authorization.Authority
	admin     contracts.AccountID
	settings  storage.Settings
	manifest  registry.Manifest
}

func nha103Account(t *testing.T, name string, role authv1.AccountRole) storage.NewAccount {
	t.Helper()
	id, err := accounts.NewAccountID()
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName(name)
	if err != nil {
		t.Fatal(err)
	}
	display, err := accounts.ParseDisplayName(name)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := accounts.HashNewPassword(context.Background(), "NHA103 synthetic isolated password 🔒")
	if err != nil {
		t.Fatal(err)
	}
	return storage.NewAccount{ID: id, Login: login, Display: display, Role: role, Verifier: verifier}
}
func nha103Source(t *testing.T, id contracts.AccountID, grants string) authorization.PolicySource {
	t.Helper()
	text, err := id.Value()
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.AdmitPolicySource(id, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n%s", text, grants)))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

const nha103All = `  - scope: {family: lists, operation: read}
    objects: {lists: {all: {}}}
  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`

func nha103Replace(t *testing.T, f nha103Fixture, actor, target contracts.AccountID, source authorization.PolicySource) authorization.AccountState {
	t.Helper()
	current, err := f.store.AuthorizationPolicy(f.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	version, err := current.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := f.store.ReplaceAuthorizationPolicy(f.ctx, actor, target, source, expected)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := outcome.Result()
	if err != nil {
		t.Fatal(err)
	}
	state, err := snapshot.State()
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func nha103Setup(t *testing.T) nha103Fixture {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if version, err := store.Migrate(ctx); err != nil || version != storage.CurrentVersion() {
		t.Fatalf("migration: %v %v", version, err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	admin := nha103Account(t, "NHA103 Synthetic Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	fixture := nha103Fixture{ctx: ctx, store: store, authority: authority, admin: admin.ID, settings: settings, manifest: manifest}
	state := nha103Replace(t, fixture, admin.ID, admin.ID, nha103Source(t, admin.ID, nha103All))
	if err := authority.Hydrate([]authorization.AccountState{state}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.service = service
	return fixture
}
func (f nha103Fixture) as(t *testing.T, id contracts.AccountID) context.Context {
	t.Helper()
	session, err := contracts.ParseSessionID("nha103-synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authorization.NativePrincipal(id, session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authorization.WithPrincipal(f.ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
func listRev(t *testing.T, receipt MutationReceipt) ListRevision {
	t.Helper()
	rev, ok := receipt.ListRevision()
	if !ok {
		t.Fatal("missing committed list revision")
	}
	return rev
}
func itemRev(t *testing.T, receipt MutationReceipt) ItemRevision {
	t.Helper()
	rev, ok := receipt.ItemRevision()
	if !ok {
		t.Fatal("missing committed item revision")
	}
	return rev
}

func TestNHA103CRUDImportAndCAS(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	created, err := f.service.CreateList(ctx, mustList(t, "Synthetic tasks"))
	if err != nil {
		t.Fatal(err)
	}
	id := created.ListID()
	rev := listRev(t, created)
	date, err := NewDueDate("2026-10-20")
	if err != nil {
		t.Fatal(err)
	}
	description := "A synthetic item"
	item, err := NewItemPayload(ItemInput{Summary: "One", Description: &description, Due: date})
	if err != nil {
		t.Fatal(err)
	}
	added, err := f.service.AddItem(ctx, id, rev, item)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := added.ItemUID()
	rev = listRev(t, added)
	irev := itemRev(t, added)
	if count, err := f.service.CountItems(ctx, id); err != nil || count != 1 {
		t.Fatalf("count: %d %v", count, err)
	}
	read, err := f.service.GetItem(ctx, id, uid)
	if err != nil || read.Summary() != "One" || read.Completed() {
		t.Fatalf("read: %v", err)
	}
	completed, err := f.service.SetCompleted(ctx, id, uid, rev, irev, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.SetCompleted(ctx, id, uid, rev, irev, false); !IsCode(err, Conflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	rev = listRev(t, completed)
	irev = itemRev(t, completed)
	uncompleted, err := f.service.SetCompleted(ctx, id, uid, rev, irev, false)
	if err != nil {
		t.Fatal(err)
	}
	read, err = f.service.GetItem(ctx, id, uid)
	if err != nil || read.Completed() {
		t.Fatalf("decompletion: %v", err)
	}
	page, err := f.service.ListItems(ctx, id, ItemCursor{}, 1)
	if err != nil || len(page.Entries()) != 1 {
		t.Fatalf("item page: %v", err)
	}
	renamed, err := f.service.RenameList(ctx, id, listRev(t, uncompleted), "Renamed tasks")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.service.GetList(ctx, id)
	if err != nil || got.Name() != "Renamed tasks" {
		t.Fatalf("rename: %v", err)
	}
	if _, err := f.service.DeleteList(ctx, id, listRev(t, renamed)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetList(ctx, id); !IsCode(err, NotFound) {
		t.Fatalf("deleted list: %v", err)
	}

	alias, err := domain.ParseHAAlias("todo.brico")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Synthetic historic"))
	if err != nil {
		t.Fatal(err)
	}
	legacyID := legacy.ListID()
	legacyRev := listRev(t, legacy)
	legacyUID, err := ParseItemUID("ics:opaque@calendar")
	if err != nil {
		t.Fatal(err)
	}
	legacyItem, err := NewLegacyItemPayload(legacyUID, 7, item)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := f.service.ImportLegacyItem(ctx, legacyID, legacyItem, legacyRev)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := f.service.EditItem(ctx, legacyID, legacyUID, listRev(t, imported), itemRev(t, imported), mustItem(t, "Local edit"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.service.ImportLegacyItem(ctx, legacyID, legacyItem, legacyRev)
	if err != nil || again.Outcome() != AlreadyImported {
		t.Fatalf("repeat import: %v", err)
	}
	read, err = f.service.GetItem(ctx, legacyID, legacyUID)
	if err != nil || read.Summary() != "Local edit" {
		t.Fatalf("import overwrote edit: %v", err)
	}
	if _, err := f.service.DeleteItem(ctx, legacyID, legacyUID, listRev(t, changed), itemRev(t, changed)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ImportLegacyItem(ctx, legacyID, legacyItem, legacyRev); err != nil {
		t.Fatalf("repeat after delete: %v", err)
	}
	if _, err := f.service.GetItem(ctx, legacyID, legacyUID); !IsCode(err, NotFound) {
		t.Fatalf("import resurrected item: %v", err)
	}
	beforeCollision, err := f.service.GetList(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewLegacyItemPayload(legacyUID, 7, mustItem(t, "Divergent source"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ImportLegacyItem(ctx, legacyID, other, legacyRev); !IsCode(err, ImportConflict) {
		t.Fatalf("divergent import: %v", err)
	}
	changedDate, err := NewDueDate("2026-10-21")
	if err != nil {
		t.Fatal(err)
	}
	changedTemporal, err := NewItemPayload(ItemInput{Summary: "One", Description: &description, Due: changedDate})
	if err != nil {
		t.Fatal(err)
	}
	temporalImport, err := NewLegacyItemPayload(legacyUID, 7, changedTemporal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ImportLegacyItem(ctx, legacyID, temporalImport, legacyRev); !IsCode(err, ImportConflict) {
		t.Fatalf("divergent temporal import: %v", err)
	}
	afterCollision, err := f.service.GetList(ctx, legacyID)
	if err != nil || afterCollision.Revision() != beforeCollision.Revision() {
		t.Fatalf("conflicting import changed list revision: %v", err)
	}
	renamedLegacy, err := f.service.RenameList(ctx, legacyID, afterCollision.Revision(), "Locally renamed history")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Synthetic historic")); err != nil || again.Outcome() != AlreadyImported || again.ListID() != legacyID {
		t.Fatalf("repeat list import after CRUD: %v", err)
	}
	currentLegacy, err := f.service.GetList(ctx, legacyID)
	if err != nil || currentLegacy.Name() != "Locally renamed history" {
		t.Fatalf("repeat list import overwrote rename: %v", err)
	}
	if _, err := f.service.DeleteList(ctx, legacyID, listRev(t, renamedLegacy)); err != nil {
		t.Fatal(err)
	}
	if again, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Synthetic historic")); err != nil || again.Outcome() != AlreadyImported {
		t.Fatalf("repeat list import after delete: %v", err)
	}
	if _, err := f.service.GetList(ctx, legacyID); !IsCode(err, NotFound) {
		t.Fatalf("repeat list import resurrected tombstone: %v", err)
	}
	if _, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Divergent historical name")); !IsCode(err, ImportConflict) {
		t.Fatalf("divergent list import after delete: %v", err)
	}
}
func mustList(t *testing.T, name string) ListPayload {
	t.Helper()
	p, err := NewListPayload(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func mustLegacyList(t *testing.T, name string) LegacyListPayload {
	t.Helper()
	p, err := NewLegacyListPayload(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func mustItem(t *testing.T, name string) ItemPayload {
	t.Helper()
	p, err := NewItemPayload(ItemInput{Summary: name, Due: NoDue()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNHA103ACLFilteringAndSQLFence(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	first, err := f.service.CreateList(adminCtx, mustList(t, "First synthetic list"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.CreateList(adminCtx, mustList(t, "Second synthetic list"))
	if err != nil {
		t.Fatal(err)
	}
	firstID, secondID := first.ListID(), second.ListID()
	secondItem, err := f.service.AddItem(adminCtx, secondID, listRev(t, second), mustItem(t, "Hidden item"))
	if err != nil {
		t.Fatal(err)
	}
	second = secondItem
	secondUID, _ := secondItem.ItemUID()
	firstText, _ := firstID.Value()
	user := nha103Account(t, "NHA103 Synthetic Reader", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	readOne := fmt.Sprintf(`  - scope: {family: lists, operation: read}
    objects: {lists: {explicit: [%q]}}
`, firstText)
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, readOne))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	userCtx := f.as(t, user.ID)
	if _, err := f.service.GetList(userCtx, firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetList(userCtx, secondID); !IsCode(err, Denied) {
		t.Fatalf("ungranted list read: %v", err)
	}
	if _, err := f.service.CountItems(userCtx, secondID); !IsCode(err, Denied) {
		t.Fatalf("ungranted item count: %v", err)
	}
	if _, err := f.service.GetItem(userCtx, secondID, secondUID); !IsCode(err, Denied) {
		t.Fatalf("ungranted item read: %v", err)
	}
	if _, err := f.service.ListItems(userCtx, secondID, ItemCursor{}, 1); !IsCode(err, Denied) {
		t.Fatalf("ungranted item page: %v", err)
	}
	if _, err := f.service.AddItem(userCtx, secondID, listRev(t, second), mustItem(t, "Hidden write")); !IsCode(err, Denied) {
		t.Fatalf("ungranted item write: %v", err)
	}
	if _, err := f.service.RenameList(userCtx, secondID, listRev(t, second), "Hidden rename"); !IsCode(err, Denied) {
		t.Fatalf("ungranted list write: %v", err)
	}
	page, err := f.service.ListLists(userCtx, ListCursor{}, MaxPage)
	if err != nil || len(page.Entries()) != 1 || page.Entries()[0].ID() != firstID {
		t.Fatalf("filtered page: %v", err)
	}
	if count, err := f.service.CountLists(userCtx); err != nil || count != 1 {
		t.Fatalf("filtered count: %d %v", count, err)
	}
	if _, err := f.service.CreateList(userCtx, mustList(t, "Denied")); !IsCode(err, Denied) {
		t.Fatalf("creation without WRITE all: %v", err)
	}
	if _, err := f.service.SubscribeList(userCtx, secondID); !IsCode(err, Denied) {
		t.Fatalf("ungranted subscription: %v", err)
	}
	sub, err := f.service.SubscribeCatalog(userCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	secondRename, err := f.service.RenameList(adminCtx, secondID, listRev(t, second), "Second renamed")
	if err != nil {
		t.Fatal(err)
	}
	_ = secondRename
	if len(sub.queue) != 0 {
		t.Fatal("ungranted list identity entered notification queue")
	}
	firstRename, err := f.service.RenameList(adminCtx, firstID, listRev(t, first), "First renamed")
	if err != nil {
		t.Fatal(err)
	}
	change, err := sub.Next(userCtx)
	if err != nil || change.ListID() != firstID || change.Revision() != listRev(t, firstRename) {
		t.Fatalf("granted notification: %v", err)
	}
	writeOnly := `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`
	state = nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, writeOnly))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Next(userCtx); !IsCode(err, Denied) {
		t.Fatalf("revoked collection notification: %v", err)
	}
	sub.Close()
	writeReceipt, err := f.service.CreateList(userCtx, mustList(t, "Write-only synthetic list"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetList(userCtx, writeReceipt.ListID()); !IsCode(err, Denied) {
		t.Fatalf("WRITE implied READ: %v", err)
	}
	if _, err := f.service.CountLists(userCtx); !IsCode(err, Denied) {
		t.Fatalf("WRITE implied collection READ: %v", err)
	}
	explicitWrite := fmt.Sprintf(`  - scope: {family: lists, operation: write}
    objects: {lists: {explicit: [%q]}}
`, firstText)
	state = nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, explicitWrite))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RenameList(userCtx, firstID, listRev(t, firstRename), "Explicitly writable"); err != nil {
		t.Fatalf("explicit WRITE object: %v", err)
	}
	if _, err := f.service.GetList(userCtx, firstID); !IsCode(err, Denied) {
		t.Fatalf("explicit WRITE implied READ: %v", err)
	}
	if _, err := f.service.CreateList(userCtx, mustList(t, "Unadmitted create")); !IsCode(err, Denied) {
		t.Fatalf("explicit WRITE created collection: %v", err)
	}
	alias, _ := domain.ParseHAAlias("todo.brico")
	if _, err := f.service.ImportLegacyList(userCtx, alias, mustLegacyList(t, "Unadmitted import")); !IsCode(err, Denied) {
		t.Fatalf("explicit WRITE imported collection: %v", err)
	}
	legacy, err := f.service.ImportLegacyList(adminCtx, alias, mustLegacyList(t, "Admin imported"))
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := ParseItemUID("ics:acl-opaque@local")
	legacyItem, err := NewLegacyItemPayload(uid, 0, mustItem(t, "Unadmitted item import"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ImportLegacyItem(userCtx, legacy.ListID(), legacyItem, listRev(t, legacy)); !IsCode(err, Denied) {
		t.Fatalf("explicit WRITE imported historical item: %v", err)
	}
	state = nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, writeOnly))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	disabled := authv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	if _, err := f.store.UpdateAccount(f.ctx, f.admin, user.ID, storage.AccountChanges{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	// The memory projection is deliberately stale. The SQL account fence must
	// still deny before any list row is read or written.
	if _, err := f.service.CreateList(userCtx, mustList(t, "SQL-denied")); !IsCode(err, Denied) {
		t.Fatalf("stale authority bypassed SQL status: %v", err)
	}
	readPerm, _ := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_LISTS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	set, _ := authorization.NewScopeSet([]authorization.Permission{readPerm})
	key, _ := contracts.ParseMcpKeyID("nha103-synthetic-key")
	principal, err := authorization.ScopedPrincipal(f.admin, key, set)
	if err != nil {
		t.Fatal(err)
	}
	scopedCtx, err := authorization.WithPrincipal(f.ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetList(scopedCtx, firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RenameList(scopedCtx, firstID, listRev(t, firstRename), "Scoped denied"); !IsCode(err, Denied) {
		t.Fatalf("scoped key escaped READ ceiling: %v", err)
	}
}

func TestNHA103SevenAliasesOrderPagesAndReopen(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	aliases := []string{"todo.brico", "todo.costco", "todo.grosseries", "todo.summer_fun", "todo.to_do", "todo.to_do_secondary", "todo.shopping_list"}
	type historical struct {
		id       domain.ListID
		uid      ItemUID
		origin   Origin
		alias    domain.HAAlias
		revision ListRevision
	}
	created := make(map[string]historical, len(aliases))
	for i, text := range aliases {
		alias, err := domain.ParseHAAlias(text)
		if err != nil {
			t.Fatal(err)
		}
		source, ok := f.manifest.SourceFor(alias)
		if !ok {
			t.Fatalf("missing frozen alias %s", text)
		}
		originText, _ := source.Value()
		origin, err := originForAlias(originText)
		if err != nil {
			t.Fatal(err)
		}
		list, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Synthetic "+text))
		if err != nil {
			t.Fatal(err)
		}
		uidText := fmt.Sprintf("ics:opaque-%d@local", i)
		if origin == ShoppingList {
			uidText = "0123456789abcdef0123456789abcdef"
		}
		uid, err := ParseItemUID(uidText)
		if err != nil {
			t.Fatal(err)
		}
		item := mustItem(t, "Duplicate allowed")
		if origin == LocalTodo {
			date, _ := NewDueDate("2026-10-21")
			desc := "Synthetic description"
			if i == 1 {
				desc = "Synthetic first line\nsecond line\r\nthird line"
			}
			item, err = NewItemPayload(ItemInput{Summary: "Duplicate allowed", Description: &desc, Due: date})
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				completedAt := time.Date(2026, 10, 19, 15, 30, 0, 0, time.UTC)
				instant, instantErr := NewDueInstant(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), "America/Toronto")
				if instantErr != nil {
					t.Fatal(instantErr)
				}
				item, err = NewItemPayload(ItemInput{Summary: "Duplicate allowed", Completed: true, Description: &desc, Due: instant, CompletedAt: &completedAt})
				if err != nil {
					t.Fatal(err)
				}
			}
		} else {
			item, err = NewItemPayload(ItemInput{Summary: "Duplicate allowed", Completed: true, Due: NoDue()})
			if err != nil {
				t.Fatal(err)
			}
		}
		payload, err := NewLegacyItemPayload(uid, int64(i+3), item)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := f.service.ImportLegacyItem(ctx, list.ListID(), payload, listRev(t, list))
		if err != nil {
			t.Fatal(err)
		}
		created[text] = historical{list.ListID(), uid, origin, alias, listRev(t, imported)}
	}
	for i := 0; i < 2; i++ {
		if _, err := f.service.CreateList(ctx, mustList(t, fmt.Sprintf("Extra native %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := f.service.CountLists(ctx); err != nil || count != 9 {
		t.Fatalf("seven plus native: %d %v", count, err)
	}
	brico := created["todo.brico"]
	duplicate, err := f.service.AddItem(ctx, brico.id, brico.revision, mustItem(t, "Duplicate allowed"))
	if err != nil {
		t.Fatal(err)
	}
	itemPage, err := f.service.ListItems(ctx, brico.id, ItemCursor{}, 1)
	if err != nil || len(itemPage.Entries()) != 1 {
		t.Fatalf("first item page: %v", err)
	}
	itemCursor, ok := itemPage.Next()
	if !ok {
		t.Fatal("missing item cursor")
	}
	movedUID, _ := duplicate.ItemUID()
	moved, err := f.service.MoveItem(ctx, brico.id, movedUID, nil, listRev(t, duplicate), itemRev(t, duplicate))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListItems(ctx, brico.id, itemCursor, 1); !IsCode(err, StaleCursor) {
		t.Fatalf("stale item page: %v", err)
	}
	items, err := f.service.ListItems(ctx, brico.id, ItemCursor{}, 2)
	if err != nil || len(items.Entries()) != 2 || items.Entries()[0].UID() != movedUID || items.Entries()[1].UID() != brico.uid {
		t.Fatalf("moved order: %v", err)
	}
	if _, _, number, _ := items.Entries()[1].Revision().Values(); number != 2 {
		t.Fatalf("shifted item revision=%d", number)
	}
	brico.revision = listRev(t, moved)
	created["todo.brico"] = brico
	shopping := created["todo.shopping_list"]
	desc := "Not allowed on shopping"
	bad, err := NewItemPayload(ItemInput{Summary: "No", Description: &desc, Due: NoDue()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AddItem(ctx, shopping.id, shopping.revision, bad); !IsCode(err, Invalid) {
		t.Fatalf("shopping admitted local field: %v", err)
	}
	page, err := f.service.ListLists(ctx, ListCursor{}, 1)
	if err != nil || len(page.Entries()) != 1 {
		t.Fatalf("list page: %v", err)
	}
	cursor, ok := page.Next()
	if !ok {
		t.Fatal("missing list cursor")
	}
	if _, err := f.service.CreateList(ctx, mustList(t, "Another native")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListLists(ctx, cursor, 1); !IsCode(err, StaleCursor) {
		t.Fatalf("stale list page: %v", err)
	}
	if _, err := f.service.ListLists(ctx, ListCursor{}, MaxPage+1); !IsCode(err, Invalid) {
		t.Fatalf("unbounded page: %v", err)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.store.Close()
	newStore, err := storage.Open(f.ctx, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(newStore.Close)
	if version, err := newStore.Migrate(f.ctx); err != nil || version != storage.CurrentVersion() {
		t.Fatalf("reopen schema: %v %v", version, err)
	}
	snapshot, err := newStore.AuthorizationPolicy(f.ctx, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	state, err := snapshot.State()
	if err != nil {
		t.Fatal(err)
	}
	newAuthority := authorization.NewAuthority()
	if err := newAuthority.Hydrate([]authorization.AccountState{state}); err != nil {
		t.Fatal(err)
	}
	newRepo, err := NewRepository(newStore, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	newService, err := NewService(newRepo, newAuthority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = newService.Close() })
	for _, text := range aliases {
		old := created[text]
		list, err := newService.GetList(ctx, old.id)
		if err != nil || list.Origin() != old.origin {
			t.Fatalf("reopen list %s: %v", text, err)
		}
		alias, ok := list.Alias()
		if !ok || alias != old.alias {
			t.Fatalf("alias changed %s", text)
		}
		item, err := newService.GetItem(ctx, old.id, old.uid)
		if err != nil || item.UID() != old.uid {
			t.Fatalf("UID changed %s: %v", text, err)
		}
		if old.origin == ShoppingList && !item.Completed() {
			t.Fatal("shopping completion lost")
		}
		if text == "todo.brico" {
			if !item.Completed() {
				t.Fatal("local completion lost")
			}
			if _, ok := item.CompletedAt(); !ok {
				t.Fatal("local completion time lost")
			}
			_, at, zone, dueErr := item.Due().Values()
			if dueErr != nil || at.IsZero() || zone != "America/Toronto" {
				t.Fatalf("local timed due lost: %v", dueErr)
			}
		}
		if text == "todo.costco" {
			date, _, _, dueErr := item.Due().Values()
			if dueErr != nil || date != "2026-10-21" {
				t.Fatalf("local civil due lost: %v", dueErr)
			}
			description, present := item.Description()
			if !present || description != "Synthetic first line\nsecond line\r\nthird line" {
				t.Fatal("multiline description changed on reopen")
			}
		}
	}
}

func TestNHA103ConcurrentSameRevision(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	created, err := f.service.CreateList(ctx, mustList(t, "Concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	id := created.ListID()
	expected := listRev(t, created)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"Winner A", "Winner B"} {
		name := name
		go func() { <-start; _, err := f.service.RenameList(ctx, id, expected, name); results <- err }()
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil && IsCode(second, Conflict)) == (second == nil && IsCode(first, Conflict)) {
		t.Fatalf("expected exactly one commit and one conflict: %v %v", first, second)
	}
	current, err := f.service.GetList(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, number, _ := current.Revision().Values()
	if number != 2 || current.Name() != "Winner A" && current.Name() != "Winner B" {
		t.Fatalf("partial concurrent result: revision=%d", number)
	}
}

func TestNHA103SparseMovePersistsReceiptAndExhaustionIsAtomic(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	importPair := func(aliasText string) (domain.ListID, ListRevision, ItemUID, ItemUID) {
		alias, _ := domain.ParseHAAlias(aliasText)
		list, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Synthetic sparse"))
		if err != nil {
			t.Fatal(err)
		}
		listID := list.ListID()
		rev := listRev(t, list)
		uids := make([]ItemUID, 0, 2)
		for i := 1; i <= 2; i++ {
			uid, _ := ParseItemUID(fmt.Sprintf("ics:sparse-%d@local", i))
			payload, err := NewLegacyItemPayload(uid, int64(i), mustItem(t, fmt.Sprintf("Sparse %d", i)))
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := f.service.ImportLegacyItem(ctx, listID, payload, rev)
			if err != nil {
				t.Fatal(err)
			}
			rev = listRev(t, receipt)
			uids = append(uids, uid)
		}
		return listID, rev, uids[0], uids[1]
	}
	id, rev, a, b := importPair("todo.brico")
	aBefore, err := f.service.GetItem(ctx, id, a)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := f.service.MoveItem(ctx, id, a, &b, rev, aBefore.Revision())
	if err != nil {
		t.Fatal(err)
	}
	aAfter, err := f.service.GetItem(ctx, id, a)
	if err != nil {
		t.Fatal(err)
	}
	if aAfter.Revision() != itemRev(t, moved) || aAfter.Position() != 1 {
		t.Fatal("sparse move receipt diverged from persisted revision/position")
	}
	if _, err := f.service.EditItem(ctx, id, a, listRev(t, moved), itemRev(t, moved), mustItem(t, "Post-move CAS")); err != nil {
		t.Fatalf("receipt rejected by next CAS: %v", err)
	}

	id, rev, a, b = importPair("todo.costco")
	aBefore, err = f.service.GetItem(ctx, id, a)
	if err != nil {
		t.Fatal(err)
	}
	bBefore, err := f.service.GetItem(ctx, id, b)
	if err != nil {
		t.Fatal(err)
	}
	idText, _ := id.Value()
	bText, _ := b.Value()
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `UPDATE public.homeward_list_items SET revision=$3 WHERE list_id=$1 AND item_uid=$2`, idText, bText, int64(math.MaxInt64))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.MoveItem(ctx, id, a, &b, rev, aBefore.Revision()); !IsCode(err, Capacity) {
		t.Fatalf("shifted revision exhaustion: %v", err)
	}
	aAfter, err = f.service.GetItem(ctx, id, a)
	if err != nil || aAfter.Revision() != aBefore.Revision() || aAfter.Position() != aBefore.Position() {
		t.Fatalf("failed move changed A: %v", err)
	}
	bAfter, err := f.service.GetItem(ctx, id, b)
	if err != nil || bAfter.Position() != bBefore.Position() {
		t.Fatalf("failed move changed B position: %v", err)
	}
	listAfter, err := f.service.GetList(ctx, id)
	if err != nil || listAfter.Revision() != rev {
		t.Fatalf("failed move changed list: %v", err)
	}
}

func TestNHA103MissingPrincipalAndUnpublishedSQLPolicy(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	created, err := f.service.CreateList(adminCtx, mustList(t, "Private synthetic list"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetList(f.ctx, created.ListID()); !IsCode(err, Denied) {
		t.Fatalf("missing principal read: %v", err)
	}
	if _, err := f.service.RenameList(f.ctx, created.ListID(), listRev(t, created), "No principal"); !IsCode(err, Denied) {
		t.Fatalf("missing principal write: %v", err)
	}
	user := nha103Account(t, "NHA103 Unpublished Policy User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, `  - scope: {family: lists, operation: read}
    objects: {lists: {all: {}}}
`))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	userCtx := f.as(t, user.ID)
	if _, err := f.service.GetList(userCtx, created.ListID()); err != nil {
		t.Fatal(err)
	}
	empty, err := authorization.EmptySource(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Leave the in-memory projection stale: SQL rights revision must win before
	// touching a list row, including collection counts.
	_ = nha103Replace(t, f, f.admin, user.ID, empty)
	if _, err := f.service.GetList(userCtx, created.ListID()); !IsCode(err, Denied) {
		t.Fatalf("unpublished SQL policy read: %v", err)
	}
	if _, err := f.service.CountLists(userCtx); !IsCode(err, Denied) {
		t.Fatalf("unpublished SQL policy count: %v", err)
	}
}

func TestNHA103ExplicitCursorIgnoresForbiddenActivityAndStablePages(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	first, err := f.service.CreateList(adminCtx, mustList(t, "Visible one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.CreateList(adminCtx, mustList(t, "Visible two"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden, err := f.service.CreateList(adminCtx, mustList(t, "Forbidden"))
	if err != nil {
		t.Fatal(err)
	}
	firstText, _ := first.ListID().Value()
	secondText, _ := second.ListID().Value()
	user := nha103Account(t, "NHA103 Cursor Reader", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	grants := fmt.Sprintf(`  - scope: {family: lists, operation: read}
    objects: {lists: {explicit: [%q, %q]}}
`, firstText, secondText)
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, grants))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	ctx := f.as(t, user.ID)
	page, err := f.service.ListLists(ctx, ListCursor{}, 1)
	if err != nil || len(page.Entries()) != 1 {
		t.Fatalf("first scoped page: %v", err)
	}
	cursor, ok := page.Next()
	if !ok {
		t.Fatal("expected second scoped page")
	}
	if cursor.kind != cursorViewExplicit || cursor.revision != 0 {
		t.Fatal("explicit cursor retained global catalogue revision")
	}
	adminPage, err := f.service.ListLists(adminCtx, ListCursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	adminCursor, ok := adminPage.Next()
	if !ok {
		t.Fatal("expected global second page")
	}
	if _, err := f.service.RenameList(adminCtx, forbidden.ListID(), listRev(t, forbidden), "Forbidden changed"); err != nil {
		t.Fatal(err)
	}
	if count, err := f.service.CountLists(ctx); err != nil || count != 2 {
		t.Fatalf("forbidden mutation changed scoped count: %d %v", count, err)
	}
	page2, err := f.service.ListLists(ctx, cursor, 1)
	if err != nil || len(page2.Entries()) != 1 || page2.Entries()[0].ID() == page.Entries()[0].ID() {
		t.Fatalf("forbidden mutation changed scoped page: %v", err)
	}
	if _, err := f.service.ListLists(adminCtx, adminCursor, 1); !IsCode(err, StaleCursor) {
		t.Fatalf("all cursor missed global mutation: %v", err)
	}
	if _, err := f.service.ListLists(ctx, ListCursor{}, MaxPage); err != nil {
		t.Fatalf("maximum admitted list page: %v", err)
	}
	if _, err := f.service.RenameList(adminCtx, second.ListID(), listRev(t, second), "Visible changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListLists(ctx, cursor, 1); !IsCode(err, StaleCursor) {
		t.Fatalf("visible mutation did not stale scoped cursor: %v", err)
	}
	all := nha103Source(t, user.ID, `  - scope: {family: lists, operation: read}
    objects: {lists: {all: {}}}
`)
	state = nha103Replace(t, f, f.admin, user.ID, all)
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListLists(ctx, cursor, 1); !IsCode(err, StaleCursor) {
		t.Fatalf("grant change did not stale cursor: %v", err)
	}
	revision := listRev(t, first)
	for i := 0; i < 3; i++ {
		receipt, err := f.service.AddItem(adminCtx, first.ListID(), revision, mustItem(t, fmt.Sprintf("Page item %d", i)))
		if err != nil {
			t.Fatal(err)
		}
		revision = listRev(t, receipt)
	}
	seen := make(map[ItemUID]struct{}, 3)
	itemCursor := ItemCursor{}
	for {
		items, err := f.service.ListItems(adminCtx, first.ListID(), itemCursor, 1)
		if err != nil || len(items.Entries()) != 1 {
			t.Fatalf("stable item page: %v", err)
		}
		uid := items.Entries()[0].UID()
		if _, duplicate := seen[uid]; duplicate {
			t.Fatal("duplicate UID across stable pages")
		}
		seen[uid] = struct{}{}
		next, hasNext := items.Next()
		if !hasNext {
			break
		}
		itemCursor = next
	}
	if len(seen) != 3 {
		t.Fatal("stable pagination skipped item")
	}
	if items, err := f.service.ListItems(adminCtx, first.ListID(), ItemCursor{}, MaxPage); err != nil || len(items.Entries()) != 3 {
		t.Fatalf("maximum admitted item page: %v", err)
	}
	state = nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, grants))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	beforeDelete, err := f.service.ListLists(ctx, ListCursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	deleteCursor, ok := beforeDelete.Next()
	if !ok {
		t.Fatal("expected second page before tombstone")
	}
	currentFirst, err := f.service.GetList(adminCtx, first.ListID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.DeleteList(adminCtx, first.ListID(), currentFirst.Revision()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListLists(ctx, deleteCursor, 1); !IsCode(err, StaleCursor) {
		t.Fatalf("visible tombstone did not stale scoped cursor: %v", err)
	}
}

func TestNHA103CommitUnknownResyncAndWithheldReceipt(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	first, err := f.service.CreateList(adminCtx, mustList(t, "Unknown first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.CreateList(adminCtx, mustList(t, "Unknown second"))
	if err != nil {
		t.Fatal(err)
	}
	firstSub, err := f.service.SubscribeList(adminCtx, first.ListID())
	if err != nil {
		t.Fatal(err)
	}
	defer firstSub.Close()
	secondSub, err := f.service.SubscribeList(adminCtx, second.ListID())
	if err != nil {
		t.Fatal(err)
	}
	defer secondSub.Close()
	catalog, err := f.service.SubscribeCatalog(adminCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `CREATE FUNCTION public.nha103_refuse_list_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred commit rejection'; END $$;
			CREATE CONSTRAINT TRIGGER nha103_refuse_list_commit AFTER UPDATE ON public.homeward_lists DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha103_refuse_list_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if receipt, err := f.service.RenameList(adminCtx, first.ListID(), listRev(t, first), "Rejected at deferred COMMIT"); !IsCode(err, CommitUnknown) || receipt.Outcome() != ReceiptInvalid {
		t.Fatalf("deferred COMMIT announced success: %v", err)
	}
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `DROP TRIGGER nha103_refuse_list_commit ON public.homeward_lists; DROP FUNCTION public.nha103_refuse_list_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if change, err := firstSub.Next(adminCtx); !IsCode(err, ResyncRequired) || change.ListID() != (domain.ListID{}) {
		t.Fatalf("affected subscription: %v", err)
	}
	if change, err := catalog.Next(adminCtx); !IsCode(err, ResyncRequired) || change.ListID() != (domain.ListID{}) {
		t.Fatalf("catalog resync: %v", err)
	}
	if len(secondSub.queue) != 0 {
		t.Fatal("unaffected list saw unknown mutation")
	}
	read, err := f.service.GetList(adminCtx, first.ListID())
	if err != nil || read.Name() != "Unknown first" || read.Revision() != listRev(t, first) {
		t.Fatalf("resync did not read durable rollback state: %v", err)
	}
	user := nha103Account(t, "NHA103 Synthetic Writer", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	userCtx := f.as(t, user.ID)
	f.service.afterKnownCommit = func() {
		f.service.afterKnownCommit = nil
		mutation, err := f.authority.BeginMutation(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer mutation.Release()
		empty, err := authorization.EmptySource(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		withdrawn := nha103Replace(t, f, f.admin, user.ID, empty)
		if err := f.authority.Publish(withdrawn); err != nil {
			t.Fatal(err)
		}
	}
	if receipt, err := f.service.RenameList(userCtx, first.ListID(), read.Revision(), "Committed before withdrawal"); !IsCode(err, CommittedResponseWithheld) || receipt.Outcome() != ReceiptInvalid {
		t.Fatalf("known commit mislabeled: %v", err)
	}
	final, err := f.service.GetList(adminCtx, first.ListID())
	if err != nil || final.Name() != "Committed before withdrawal" {
		t.Fatalf("withheld response rolled back mutation: %v", err)
	}
}

func TestNHA103MidTransactionFailureRollsBackRowsLedgerAndSignals(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	baseline, err := f.service.CreateList(ctx, mustList(t, "Rollback baseline"))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := f.service.SubscribeCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	catalogRevision := func() int64 {
		var revision int64
		if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, `SELECT revision FROM public.homeward_lists_catalog WHERE singleton=true`).Scan(&revision)
		}); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	before := catalogRevision()
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `CREATE FUNCTION public.nha103_refuse_catalog_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic immediate rollback'; END $$;
			CREATE TRIGGER nha103_refuse_catalog_update AFTER UPDATE ON public.homeward_lists_catalog FOR EACH ROW EXECUTE FUNCTION public.nha103_refuse_catalog_update()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("todo.brico")
	if receipt, err := f.service.ImportLegacyList(ctx, alias, mustLegacyList(t, "Rollback imported")); !IsCode(err, Unavailable) || receipt.Outcome() != ReceiptInvalid {
		t.Fatalf("failed import announced success: %v", err)
	}
	if receipt, err := f.service.AddItem(ctx, baseline.ListID(), listRev(t, baseline), mustItem(t, "Rollback item")); !IsCode(err, Unavailable) || receipt.Outcome() != ReceiptInvalid {
		t.Fatalf("failed item add announced success: %v", err)
	}
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `DROP TRIGGER nha103_refuse_catalog_update ON public.homeward_lists_catalog; DROP FUNCTION public.nha103_refuse_catalog_update()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after := catalogRevision(); after != before {
		t.Fatalf("failed writes advanced catalog %d→%d", before, after)
	}
	if count, err := f.service.CountLists(ctx); err != nil || count != 1 {
		t.Fatalf("failed import left business row: %d %v", count, err)
	}
	if count, err := f.service.CountItems(ctx, baseline.ListID()); err != nil || count != 0 {
		t.Fatalf("failed item add left business row: %d %v", count, err)
	}
	list, err := f.service.GetList(ctx, baseline.ListID())
	if err != nil || list.Revision() != listRev(t, baseline) {
		t.Fatalf("failed add advanced list revision: %v", err)
	}
	var listRows, ledgerRows int64
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM public.homeward_lists WHERE legacy_alias='todo.brico'`).Scan(&listRows); err != nil {
			return err
		}
		return tx.QueryRow(f.ctx, `SELECT count(*) FROM public.homeward_legacy_import_ledger WHERE legacy_alias='todo.brico'`).Scan(&ledgerRows)
	}); err != nil {
		t.Fatal(err)
	}
	if listRows != 0 || ledgerRows != 0 {
		t.Fatalf("failed import left row or fingerprint: %d %d", listRows, ledgerRows)
	}
	if len(sub.queue) != 0 || sub.terminal {
		t.Fatal("rolled-back mutation emitted success or resync")
	}
}

func TestNHA103PolicyWriterWaitsForSQLFenceWithoutDeadlock(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	list, err := f.service.CreateList(adminCtx, mustList(t, "SQL-race"))
	if err != nil {
		t.Fatal(err)
	}
	user := nha103Account(t, "NHA103 SQL Race User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	userCtx := f.as(t, user.ID)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBlocked()
	f.service.repo.afterFence = func() { once.Do(func() { close(entered) }); <-release }
	writeDone := make(chan error, 1)
	go func() {
		_, err := f.service.RenameList(userCtx, list.ListID(), listRev(t, list), "Serialized write")
		writeDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("list never reached SQL fence")
	}
	current, err := f.store.AuthorizationPolicy(f.ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	version, err := current.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := authorization.EmptySource(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	policyDone := make(chan error, 1)
	go func() {
		mutation, err := f.authority.BeginMutation(user.ID)
		if err != nil {
			policyDone <- err
			return
		}
		defer mutation.Release()
		close(barrier)
		outcome, err := f.store.ReplaceAuthorizationPolicy(f.ctx, f.admin, user.ID, empty, expected)
		if err != nil {
			policyDone <- err
			return
		}
		snapshot, _, err := outcome.Result()
		if err != nil {
			policyDone <- err
			return
		}
		withdrawn, err := snapshot.State()
		if err != nil {
			policyDone <- err
			return
		}
		policyDone <- f.authority.Publish(withdrawn)
	}()
	select {
	case <-barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("policy writer never acquired account barrier")
	}
	releaseBlocked()
	select {
	case err := <-writeDone:
		if err != nil && !IsCode(err, CommittedResponseWithheld) {
			t.Fatalf("list write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("list write deadlocked")
	}
	select {
	case err := <-policyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("policy replacement deadlocked")
	}
	f.service.repo.afterFence = nil
	final, err := f.service.GetList(adminCtx, list.ListID())
	if err != nil || final.Name() != "Serialized write" {
		t.Fatalf("serialized write missing: %v", err)
	}
}

func TestNHA103RevocationWinsAfterEvidenceBeforeFenceAndWaitingNoticeDenied(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	created, err := f.service.CreateList(adminCtx, mustList(t, "Revocation race"))
	if err != nil {
		t.Fatal(err)
	}
	user := nha103Account(t, "NHA103 Revocation Race User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, `  - scope: {family: lists, operation: read}
    objects: {lists: {all: {}}}
`))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	ctx := f.as(t, user.ID)
	sub, err := f.service.SubscribeList(ctx, created.ListID())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	notice := make(chan error, 1)
	go func() {
		change, err := sub.Next(ctx)
		if change.ListID() != (domain.ListID{}) {
			notice <- fmt.Errorf("revoked notice disclosed list identity")
			return
		}
		notice <- err
	}()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBlocked()
	f.service.repo.beforeFence = func() { once.Do(func() { close(entered) }); <-release }
	readDone := make(chan error, 1)
	go func() {
		snapshot, err := f.service.GetList(ctx, created.ListID())
		if snapshot.ID() != (domain.ListID{}) {
			readDone <- fmt.Errorf("revoked read disclosed list identity")
			return
		}
		readDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not capture evidence")
	}
	mutation, err := f.authority.BeginMutation(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Release()
	empty, err := authorization.EmptySource(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	withdrawn := nha103Replace(t, f, f.admin, user.ID, empty)
	if err := f.authority.Publish(withdrawn); err != nil {
		t.Fatal(err)
	}
	mutation.Release()
	releaseBlocked()
	select {
	case err := <-readDone:
		if !IsCode(err, Denied) {
			t.Fatalf("stale evidence read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not finish")
	}
	select {
	case err := <-notice:
		if !IsCode(err, Denied) {
			t.Fatalf("waiting notice after withdrawal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting notice did not finish")
	}
	f.service.repo.beforeFence = nil
}

func TestNHA103AccountDisableWinsAfterEvidenceBeforeFence(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	created, err := f.service.CreateList(adminCtx, mustList(t, "Account race"))
	if err != nil {
		t.Fatal(err)
	}
	user := nha103Account(t, "NHA103 Account Race User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, `  - scope: {family: lists, operation: read}
    objects: {lists: {all: {}}}
`))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	ctx := f.as(t, user.ID)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBlocked()
	f.service.repo.beforeFence = func() { once.Do(func() { close(entered) }); <-release }
	readDone := make(chan error, 1)
	go func() {
		snapshot, err := f.service.GetList(ctx, created.ListID())
		if snapshot.ID() != (domain.ListID{}) {
			readDone <- fmt.Errorf("disabled account received list")
			return
		}
		readDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not capture account evidence")
	}
	mutation, err := f.authority.BeginMutation(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Release()
	disabled := authv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	if _, err := f.store.UpdateAccount(f.ctx, f.admin, user.ID, storage.AccountChanges{Status: &disabled}); err != nil {
		mutation.Release()
		t.Fatal(err)
	}
	snapshot, err := f.store.AuthorizationPolicy(f.ctx, user.ID)
	if err != nil {
		mutation.Release()
		t.Fatal(err)
	}
	updated, err := snapshot.State()
	if err != nil {
		mutation.Release()
		t.Fatal(err)
	}
	if err := f.authority.Publish(updated); err != nil {
		mutation.Release()
		t.Fatal(err)
	}
	mutation.Release()
	releaseBlocked()
	select {
	case err := <-readDone:
		if !IsCode(err, Denied) {
			t.Fatalf("disabled account read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disabled account read did not finish")
	}
	f.service.repo.beforeFence = nil
}

type nha103NilContext struct{ context.Context }

func TestNHA103LifecycleTypedNilAndOverflow(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	created, err := f.service.CreateList(ctx, mustList(t, "Lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	id := created.ListID()
	var pointer *nha103NilContext
	var typed context.Context = pointer
	if _, err := f.service.GetList(typed, id); !IsCode(err, Denied) {
		t.Fatalf("typed-nil read: %v", err)
	}
	if _, err := f.service.SubscribeList(typed, id); !IsCode(err, Denied) {
		t.Fatalf("typed-nil subscribe: %v", err)
	}
	sub, err := f.service.SubscribeList(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Next(typed); !IsCode(err, Invalid) {
		t.Fatalf("typed-nil next: %v", err)
	}
	for i := 0; i <= MaxNotificationQueue; i++ {
		f.service.publish(id, int64(i+2))
	}
	if change, err := sub.Next(ctx); !IsCode(err, ResyncRequired) || change.ListID() != (domain.ListID{}) {
		t.Fatalf("overflow leaked queued identity: %v", err)
	}
	sub.Close()
	if _, err := sub.Next(ctx); !IsCode(err, Closed) {
		t.Fatalf("closed subscription: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBlocked()
	f.service.repo.afterFence = func() { once.Do(func() { close(entered) }); <-release }
	callDone := make(chan error, 1)
	go func() { _, err := f.service.CreateList(ctx, mustList(t, "Canceled during Close")); callDone <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("call never reached fence")
	}
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- f.service.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.service.mu.Lock()
		closed := f.service.closed
		f.service.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close admission did not stop")
		}
		runtime.Gosched()
	}
	go func() { secondDone <- f.service.Close() }()
	select {
	case <-secondDone:
		t.Fatal("second Close returned before active call joined")
	case <-time.After(20 * time.Millisecond):
	}
	releaseBlocked()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("Close-canceled call announced success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled call did not end")
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Close did not join")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Close did not join")
	}
	if _, err := f.service.GetList(ctx, id); !IsCode(err, Closed) {
		t.Fatalf("service accepted after Close: %v", err)
	}
}

func TestNHA103CallerCancellationDuringBlockedSQLAndCloseJoin(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	created, err := f.service.CreateList(adminCtx, mustList(t, "SQL cancellation"))
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	blockerDone := make(chan error, 1)
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBlocked()
	go func() {
		blockerDone <- f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			var revision int64
			if err := tx.QueryRow(f.ctx, `SELECT revision FROM public.homeward_lists_catalog WHERE singleton=true FOR UPDATE`).Scan(&revision); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("catalog blocker did not start")
	}
	entered := make(chan struct{})
	var once sync.Once
	f.service.repo.afterFence = func() { once.Do(func() { close(entered) }) }
	callCtx, cancel := context.WithCancel(adminCtx)
	defer cancel()
	type cancellationResult struct {
		receipt MutationReceipt
		err     error
	}
	callDone := make(chan cancellationResult, 1)
	expected := listRev(t, created)
	go func() {
		receipt, err := f.service.RenameList(callCtx, created.ListID(), expected, "Must roll back")
		callDone <- cancellationResult{receipt: receipt, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rename did not pass SQL fence")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
				AND wait_event_type='Lock' AND query LIKE '%homeward_lists_catalog%' AND pid<>pg_backend_pid())`).Scan(&waiting)
		})
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rename did not block in actual SQL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.service.Close() }()
	releaseBlocked()
	select {
	case err := <-blockerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SQL blocker did not release")
	}
	select {
	case result := <-callDone:
		if result.err == nil {
			t.Fatal("canceled SQL announced success")
		}
		if result.receipt.Outcome() != ReceiptInvalid {
			t.Fatal("canceled SQL returned a mutation receipt")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled SQL did not return")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join canceled SQL")
	}
	f.service.repo.afterFence = nil
	reopened, err := NewService(f.service.repo, f.authority)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	read, err := reopened.GetList(adminCtx, created.ListID())
	if err != nil || read.Name() != "SQL cancellation" || read.Revision() != listRev(t, created) {
		t.Fatalf("canceled SQL changed durable row: %v", err)
	}
}

func TestNHA103CloseJoinsInheritedAuthorityBarrierAfterRelease(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	created, err := f.service.CreateList(ctx, mustList(t, "Barrier close"))
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := f.authority.BeginMutation(f.admin)
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Release()
	callDone := make(chan error, 1)
	go func() { _, err := f.service.GetList(ctx, created.ListID()); callDone <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.service.mu.Lock()
		active := f.service.active
		f.service.mu.Unlock()
		if active == 1 {
			break
		}
		if time.Now().After(deadline) {
			mutation.Release()
			t.Fatal("call did not enter service")
		}
		runtime.Gosched()
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.service.Close() }()
	select {
	case <-closeDone:
		mutation.Release()
		t.Fatal("Close returned before Authority barrier released")
	case <-time.After(20 * time.Millisecond):
	}
	mutation.Release()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("canceled barrier waiter succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("barrier waiter did not finish")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join after barrier release")
	}
}
