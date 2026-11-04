package view_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/entityfilter"
	"github.com/damione1/Homeward/internal/view"
)

func TestEntityViewResourceWatchUsesExactJournalAndRetires(t *testing.T) {
	f := newWatchFixture(t, 2)
	for i := range f.ids {
		f.publish(t, i, false)
	}
	service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
		return projectPreparedItem(t, item)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
	filter, _ := entityfilter.Parse("")
	descriptor, err := service.CreateEntityView(ctx, f.actor, home, filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ReadEntityView(ctx, f.actor, descriptor.Name, "", 1)
	if err != nil || len(page.Items) != 1 || page.NextPageToken == "" {
		t.Fatal("first page", err)
	}
	tail, err := service.ReadEntityView(ctx, f.actor, descriptor.Name, page.NextPageToken, 500)
	if err != nil || len(tail.Items) != 1 || tail.NextPageToken != "" {
		t.Fatal("tail", err)
	}
	watch, err := service.WatchEntityView(ctx, f.actor, descriptor.Name, descriptor.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	// Both events exist before Next. A fresh state read would collapse the first
	// observation into false; the native journal must preserve true then false.
	f.publish(t, 0, true)
	f.publish(t, 0, false)
	for _, want := range []bool{true, false} {
		change, err := watch.Next()
		if err != nil || change.Item == nil {
			t.Fatal("journal change", err)
		}
		state, present := change.Item.State()
		value, hasValue := state.CurrentValue()
		got, typed := value.Switch()
		if !present || !hasValue || !typed || got != want {
			t.Fatal("watch refreshed instead of projecting exact journal state", got, want)
		}
	}
	if err := service.DeleteEntityView(ctx, f.actor, descriptor.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Next(); err == nil {
		t.Fatal("deleted view kept streaming")
	}
	if _, err := service.GetEntityView(ctx, f.actor, descriptor.Name); !errors.Is(err, view.ErrSnapshotUnknown) {
		t.Fatal("deleted capture retained", err)
	}
}

func TestEntityViewResourceRevocationInvalidatesIdleWatch(t *testing.T) {
	f := newWatchFixture(t, 1)
	f.publish(t, 0, false)
	service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
		return projectPreparedItem(t, item)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
	filter, _ := entityfilter.Parse("")
	descriptor, err := service.CreateEntityView(ctx, f.actor, home, filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := service.WatchEntityView(ctx, f.actor, descriptor.Name, descriptor.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	f.auth.SetGrant(testGrant(t, f.actor, 2, nil))
	if _, err := watch.Next(); err == nil {
		t.Fatal("revoked view emitted data")
	}
	if _, err := service.GetEntityView(ctx, f.actor, descriptor.Name); err == nil {
		t.Fatal("revoked capture admitted")
	}
}

func TestEntityViewWatchAdmitsFirstObservationAfterEmptyCapture(t *testing.T) {
	f := newWatchFixture(t, 1)
	service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
		return projectPreparedItem(t, item)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
	filter, _ := entityfilter.Parse("")
	descriptor, err := service.CreateEntityView(ctx, f.actor, home, filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := service.WatchEntityView(ctx, f.actor, descriptor.Name, descriptor.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	f.publish(t, 0, true)
	change, err := watch.Next()
	if err != nil || change.Item == nil {
		t.Fatal("first observation suppressed", err)
	}
	if _, present := change.Item.State(); !present {
		t.Fatal("first observation remained absent")
	}
}

func TestEntityViewWatchExpiryWithdrawsCapture(t *testing.T) {
	f := newWatchFixture(t, 1)
	f.publish(t, 0, false)
	service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
		return projectPreparedItem(t, item)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
	filter, _ := entityfilter.Parse("")
	descriptor, err := service.CreateEntityView(ctx, f.actor, home, filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := service.WatchEntityView(ctx, f.actor, descriptor.Name, descriptor.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	f.clock.mu.Lock()
	f.clock.wall = f.clock.wall.Add(view.SnapshotTTL + time.Second)
	f.clock.mu.Unlock()
	if _, err := watch.Next(); err == nil {
		t.Fatal("expired watch retained readable capture")
	}
	if _, err := service.GetEntityView(ctx, f.actor, descriptor.Name); err == nil {
		t.Fatal("expired view remained discoverable")
	}
}
