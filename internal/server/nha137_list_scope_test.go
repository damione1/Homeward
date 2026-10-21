package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

func nha137ListImages(t *testing.T, ctx context.Context, store *storage.Store) (string, string) {
	t.Helper()
	var identities, content string
	if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT jsonb_build_array(
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM public.registry_devices x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY instance_id,source_device),'[]'::jsonb) FROM public.registry_device_sources x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY id),'[]'::jsonb) FROM public.registry_entities x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY instance_id,source_device,source_entity),'[]'::jsonb) FROM public.registry_source_bindings x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY alias),'[]'::jsonb) FROM public.registry_aliases x)
		)::text,jsonb_build_array(
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY singleton),'[]'::jsonb) FROM public.homeward_lists_catalog x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY list_id),'[]'::jsonb) FROM public.homeward_lists x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY list_id,item_uid),'[]'::jsonb) FROM public.homeward_list_items x),
		(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY legacy_alias,item_uid),'[]'::jsonb) FROM public.homeward_legacy_import_ledger x)
		)::text`).Scan(&identities, &content)
	}); err != nil {
		t.Fatal(err)
	}
	return identities, content
}

// The importer below belongs to the actual default Run on each cold start.
// Its human principal is separate from internal automation execution.
func TestNHA137DefaultRunListImportKeepsEcobeeLineageAcrossSameDatabase(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	historical := nha137LaunchFixture(cluster.Port, data, "ecobee_mvp_v1")
	parsed, err := config.ParseWithEnv(historical, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parsed.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
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
	account := nha034Account(t, store)
	accountID, err := account.ID()
	if err != nil {
		t.Fatal(err)
	}
	nha162SetPolicy(t, &nha162RPCRig{ctx: ctx, store: store, admin: accountID}, accountID, nha162All)
	owner, _ := nha034Session(t, store, account, "nha137-list-native-importer", time.Now())
	principal, err := authorization.NativePrincipal(owner.Account, owner.Session)
	if err != nil {
		t.Fatal(err)
	}
	importCtx, err := authorization.WithPrincipal(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	amendment, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, amendment)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha137-list-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	ecobeeKey, err := integrations.NewSourceKey("synthetic-ecobee", "thermostat")
	if err != nil {
		t.Fatal(err)
	}
	ecobee, err := repository.ConfigureBinding(ctx, instance, ecobeeKey, "climate.home")
	if err != nil {
		t.Fatal(err)
	}
	listKey, err := integrations.NewSourceKey("synthetic-list", "list")
	if err != nil {
		t.Fatal(err)
	}
	listBinding, err := repository.ConfigureBinding(ctx, instance, listKey, "todo.brico")
	if err != nil {
		t.Fatal(err)
	}
	identities, _ := nha137ListImages(t, ctx, store)
	alias, err := domain.ParseHAAlias("todo.brico")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := lists.NewLegacyListPayload("NHA137 synthetic imported list")
	if err != nil {
		t.Fatal(err)
	}
	entity, err := ecobee.Entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	var durableList domain.ListID
	var durableShopping domain.ListID
	for step, cold := range []scope.Revision{scope.BaselineV1(), scope.EcobeeMVPV1(), scope.BaselineV1()} {
		name, err := cold.Value()
		if err != nil {
			t.Fatal(err)
		}
		startup := nha137StartupFixture(t, historical, nha137LaunchFixture(cluster.Port, data, name))
		if startup.Origin() != config.BootstrapOriginExplicitSidecar {
			t.Fatal("detached list fixture lost explicit bootstrap provenance")
		}
		runCtx, stop := context.WithCancel(ctx)
		run := &nha162Running{stop: stop, done: make(chan error, 1)}
		ready := make(chan net.Addr, 1)
		observedLists := make(chan nha162ObservedRun, 1)
		observedRuntime := make(chan *observationRuntime, 1)
		go func() {
			run.done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil,
				runDependencies{observeLists: func(lists *listComposition, sessions *sessionCoordinator) {
					observedLists <- nha162ObservedRun{lists, sessions}
				},
					observeRuntime: func(actual *observationRuntime) { observedRuntime <- actual }})
		}()
		t.Cleanup(func() {
			if !run.joined {
				if err := run.join(t); err != nil {
					t.Error(err)
				}
			}
		})
		select {
		case run.address = <-ready:
		case err := <-run.done:
			run.joined = true
			t.Fatalf("default list Run scope %s before Ready: %v", name, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var runtime *observationRuntime
		select {
		case run.observed = <-observedLists:
		default:
			t.Fatal("Ready without actual list composition")
		}
		select {
		case runtime = <-observedRuntime:
		default:
			t.Fatal("Ready without actual runtime observation")
		}
		image, err := runtime.sequence.Capture()
		if err != nil {
			t.Fatalf("default list Run scope %s Capture: %v", name, err)
		}
		_, projectedList := image.Registry[listBinding.Entity]
		_, projectedEcobee := image.Registry[ecobee.Entity]
		if !image.RegistryReady || !projectedList || projectedEcobee != (cold == scope.EcobeeMVPV1()) || len(image.Registry) != 1+boolToNHA137Int(projectedEcobee) {
			t.Fatalf("default list Run projection scope %s: ready=%t list=%t Ecobee=%t count=%d", name, image.RegistryReady, projectedList, projectedEcobee, len(image.Registry))
		}
		actual := run.observed.lists
		selected, err := registry.LoadManifest(cold)
		if err != nil {
			t.Fatal(err)
		}
		owned, err := actual.registry.ScopeManifest()
		if err != nil || !reflect.DeepEqual(actual.manifest, selected) || !reflect.DeepEqual(owned, selected) {
			t.Fatalf("Run list importer selected a separate scope: %v", err)
		}
		receipt, err := actual.ImportLegacyList(importCtx, alias, payload)
		want := lists.Committed
		if step > 0 {
			want = lists.AlreadyImported
		}
		if err != nil || receipt.Outcome() != want {
			t.Fatalf("scope %s rejected valid list with durable Ecobee: outcome=%v err=%v", name, receipt.Outcome(), err)
		}
		if step == 0 {
			durableList = receipt.ListID()
		} else if receipt.ListID() != durableList {
			t.Fatal("scope change created a second list identity")
		}
		current, err := actual.service.GetList(importCtx, durableList)
		if err != nil || current.Name() != "NHA137 synthetic imported list" {
			t.Fatalf("imported list unavailable: %v", err)
		}
		uid, err := lists.ParseItemUID(fmt.Sprintf("nha137-item-%d", step))
		if err != nil {
			t.Fatal(err)
		}
		item, err := lists.NewItemPayload(lists.ItemInput{Summary: "NHA137 synthetic imported item", Due: lists.NoDue()})
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := lists.NewLegacyItemPayload(uid, int64(step), item)
		if err != nil {
			t.Fatal(err)
		}
		added, err := actual.ImportLegacyItem(importCtx, durableList, alias, legacy, current.Revision())
		if err != nil || added.Outcome() != lists.Committed {
			t.Fatalf("scope %s rejected valid item with durable Ecobee: %v", name, err)
		}
		page, err := actual.service.ListItems(importCtx, durableList, lists.ItemCursor{}, 10)
		if err != nil || len(page.Entries()) != step+1 || page.Entries()[step].UID() != uid {
			t.Fatalf("scope %s lost imported items: %v", name, err)
		}
		expected, present := added.ListRevision()
		if !present {
			t.Fatal("committed item lost its list revision")
		}
		shoppingAlias, err := domain.ParseHAAlias("todo.shopping_list")
		if err != nil {
			t.Fatal(err)
		}
		shopping, err := actual.ImportLegacyList(importCtx, shoppingAlias, payload)
		if err != nil || shopping.Outcome() != want {
			t.Fatalf("scope %s rejected the shopping_list origin: %v", name, err)
		}
		if step == 0 {
			durableShopping = shopping.ListID()
		} else if shopping.ListID() != durableShopping {
			t.Fatal("scope change created a second shopping list")
		}
		shoppingState, err := actual.service.GetList(importCtx, durableShopping)
		if err != nil || shoppingState.Origin() != lists.ShoppingList {
			t.Fatalf("shopping import lost its typed origin: %v", err)
		}
		shoppingItem, err := actual.ImportLegacyItem(importCtx, durableShopping, shoppingAlias, legacy, shoppingState.Revision())
		if err != nil || shoppingItem.Outcome() != lists.Committed {
			t.Fatalf("scope %s rejected a shopping item: %v", name, err)
		}
		_, beforeRefusal := nha137ListImages(t, ctx, store)
		for _, foreign := range []string{"climate.home", "sensor.home_current_temperature", "todo.nha137_foreign"} {
			outside, err := domain.ParseHAAlias(foreign)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := actual.ImportLegacyList(importCtx, outside, payload); !errors.Is(err, ErrListImportBinding) || result.Outcome() != lists.ReceiptInvalid {
				t.Fatalf("scope %s imported a non-list origin %s: %v", name, foreign, err)
			}
		}
		for _, mutation := range []struct {
			name, corrupt, restore string
			want                   error
		}{
			{"foreign-original", `UPDATE public.registry_entities SET manifest_alias='climate.nha137_foreign' WHERE id=$1`, `UPDATE public.registry_entities SET manifest_alias='climate.home' WHERE id=$1`, registry.ErrOutsideManifest},
			{"corrupt-binding", `UPDATE public.registry_source_bindings SET source_entity=repeat('x',257) WHERE entity_id=$1`, `UPDATE public.registry_source_bindings SET source_entity='thermostat' WHERE entity_id=$1`, registry.ErrStorage},
			{"stolen-lineage-alias", `INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES('todo.costco',$1,false)`, `DELETE FROM public.registry_aliases WHERE entity_id=$1 AND alias='todo.costco'`, ErrListImportBinding},
		} {
			t.Run(fmt.Sprintf("%d-%s-%s", step, name, mutation.name), func(t *testing.T) {
				mutate := func(sql string) {
					t.Helper()
					if err := store.InTransaction(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, entity); return err }); err != nil {
						t.Fatal(err)
					}
				}
				mutate(mutation.corrupt)
				defer mutate(mutation.restore)
				for _, err := range []error{
					func() error { _, err := actual.ImportLegacyList(importCtx, alias, payload); return err }(),
					func() error {
						_, err := actual.ImportLegacyItem(importCtx, durableList, alias, legacy, expected)
						return err
					}(),
				} {
					if !errors.Is(err, mutation.want) {
						t.Fatalf("import hid corrupt registry: %v want %v", err, mutation.want)
					}
					if mutation.want == ErrListImportBinding && !errors.Is(err, registry.ErrAliasConflict) {
						t.Fatal("import translation lost typed registry collision")
					}
				}
				if _, after := nha137ListImages(t, ctx, store); after != beforeRefusal {
					t.Fatal("refused imports changed list data, revisions or import history")
				}
			})
		}
		if got, err := repository.Resolve(ctx, instance, []integrations.SourceKey{ecobeeKey}); err != nil || len(got) != 1 || got[0] != ecobee {
			t.Fatalf("import changed durable Ecobee identity: %v", err)
		}
		if got, _ := nha137ListImages(t, ctx, store); got != identities {
			t.Fatal("scope change or import rewrote durable registry identities")
		}
		if err := run.join(t); err != nil {
			t.Fatal(err)
		}
		nha162RunClosed(t, run.observed)
	}
}
