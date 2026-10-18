package storage

import (
	"context"
	"testing"
	"time"
)

func TestNHA024MigrationPreservesRegistryAndMatchesTypedStates(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.migrate(ctx, migrations[:2]); err != nil || version.Number() != 2 {
		t.Fatalf("registry schema = %v %v", version, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES ('fixture-device');
		INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES ('Host:fixture','fixture-device','switch.fixture')`); err != nil {
		t.Fatal(err)
	}
	if version, err := store.Migrate(ctx); err != nil || version.Number() != 3 {
		t.Fatalf("ledger migration = %v %v", version, err)
	}
	var alias string
	if err := store.pool.QueryRow(ctx, `SELECT manifest_alias FROM public.registry_entities WHERE id='Host:fixture'`).Scan(&alias); err != nil || alias != "switch.fixture" {
		t.Fatalf("registry data lost: %q %v", alias, err)
	}
	if version, err := store.Migrate(ctx); err != nil || version.Number() != 3 {
		t.Fatalf("repeat migration = %v %v", version, err)
	}
	for _, pair := range []struct {
		state IntentState
		label string
	}{{IntentAccepted, "accepted"}, {IntentDispatching, "dispatching"}, {IntentResult, "result"}, {IntentIndeterminate, "indeterminate"}} {
		label, ok := intentStateText(pair.state)
		if !ok || label != pair.label {
			t.Fatalf("Go state %v = %q %t", pair.state, label, ok)
		}
		var present bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_enum e JOIN pg_type t ON t.oid=e.enumtypid WHERE t.typname='command_intent_state' AND e.enumlabel=$1)`, label).Scan(&present); err != nil || !present {
			t.Fatalf("SQL state %q: %t %v", label, present, err)
		}
	}
	for _, pair := range []struct {
		kind  IntentError
		label string
	}{{IntentNoError, "none"}, {IntentRouteBusy, "route_busy"}, {IntentRouteUnavailable, "route_unavailable"}, {IntentCanceled, "canceled"}, {IntentDeadline, "deadline"}, {IntentOtherError, "unknown"}, {IntentOutcomeIndeterminate, "indeterminate"}} {
		label, ok := intentErrorText(pair.kind)
		if !ok || label != pair.label {
			t.Fatalf("Go error %v = %q %t", pair.kind, label, ok)
		}
		var present bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_enum e JOIN pg_type t ON t.oid=e.enumtypid WHERE t.typname='command_result_error' AND e.enumlabel=$1)`, label).Scan(&present); err != nil || !present {
			t.Fatalf("SQL error %q: %t %v", label, present, err)
		}
	}
	if _, ok := intentStateText(IntentUnknown); ok {
		t.Fatal("zero state admitted")
	}
	if _, ok := intentErrorText(IntentErrorUnknown); ok {
		t.Fatal("zero error kind admitted")
	}
	for name, want := range map[string]int{"command_intent_state": 4, "command_result_error": 7} {
		var count int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM pg_enum e JOIN pg_type t ON t.oid=e.enumtypid WHERE t.typname=$1`, name).Scan(&count); err != nil || count != want {
			t.Fatalf("%s cardinality = %d %v", name, count, err)
		}
	}
}
