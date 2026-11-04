package view_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/entityfilter"
	"github.com/damione1/Homeward/internal/view"
)

func TestEntitySelectionPrecedesPagingAndNeverRefreshesCapture(t *testing.T) {
	f := newWatchFixture(t, 4)
	for i := range f.ids {
		f.publish(t, i, false)
	}
	ctx := context.Background()
	home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
	calls := 0
	service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
		calls++
		return projectPreparedItem(t, item)
	})
	if err != nil {
		t.Fatal(err)
	}
	excluded, _ := apitypes.EntityNameForIdentity(home, f.ids[0])
	filter, err := entityfilter.Parse("name != " + strconv.Quote(excluded.String()) + " AND trait(power) AND availability(AVAILABLE)")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.OpenEntityView(ctx, f.actor, home, filter, 1)
	if err != nil || len(first.Items) != 1 || first.NextPageToken == "" {
		t.Fatal("first selected page", err)
	}
	for i := range f.ids {
		f.publish(t, i, true)
	}
	tail, err := service.ReadPageSize(ctx, f.actor, first.SnapshotToken, first.NextPageToken, 500)
	if err != nil || len(tail.Items) != 2 || tail.NextPageToken != "" {
		t.Fatal("selection was applied after paging", err)
	}
	for _, page := range []view.Page{first, tail} {
		for _, item := range page.Items {
			_, id, _ := item.Registry().IDs()
			if id == f.ids[0] {
				t.Fatal("excluded identity escaped selection")
			}
			state, exists := item.State()
			value, present := state.CurrentValue()
			on, ok := value.Switch()
			if !exists || !present || !ok || on {
				t.Fatal("selection pagination read a later observation")
			}
		}
	}
	read, err := service.ReadFirstPage(ctx, f.actor, first.SnapshotToken, 2)
	if err != nil || len(read.Items) != 2 || calls != 4 {
		t.Fatal("first page recaptured provider data", calls, err)
	}
	// A first-page read is subject to exactly the same live authority fence as
	// a continuation. Losing rights destroys the old capture.
	f.auth.SetGrant(testGrant(t, f.actor, 2, nil))
	if _, err := service.ReadFirstPage(ctx, f.actor, first.SnapshotToken, 1); !errors.Is(err, view.ErrViewChanged) {
		t.Fatal("first page bypassed current rights", err)
	}
	if _, err := service.ReadFirstPage(ctx, f.actor, first.SnapshotToken, 1); !errors.Is(err, view.ErrSnapshotUnknown) {
		t.Fatal("revoked capture was retained", err)
	}
}

func TestEntitySelectionRequiresProjectionAndCannotDiscoverHiddenItems(t *testing.T) {
	f := newWatchFixture(t, 2)
	home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
	filter, _ := entityfilter.Parse("")
	if _, err := f.service.OpenEntityView(context.Background(), f.actor, home, filter, 1); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal("missing projector became empty capability catalogue", err)
	}
	f.auth.SetGrant(testGrant(t, f.actor, 2, f.ids[:1]))
	calls := 0
	service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
		calls++
		return projectPreparedItem(t, item)
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, _ := apitypes.EntityNameForIdentity(home, f.ids[1])
	filter, _ = entityfilter.Parse("name = " + strconv.Quote(hidden.String()))
	page, err := service.OpenEntityView(context.Background(), f.actor, home, filter, 1)
	if err != nil || len(page.Items) != 0 || page.NextPageToken != "" || calls != 1 {
		t.Fatal("filter inspected unreadable item", calls, err)
	}
	f.clock.mu.Lock()
	f.clock.wall = f.clock.wall.Add(view.SnapshotTTL + time.Second)
	f.clock.mu.Unlock()
	if _, err := service.ReadFirstPage(context.Background(), f.actor, page.SnapshotToken, 1); !errors.Is(err, view.ErrSnapshotExpired) {
		t.Fatal("first page bypassed expiry", err)
	}
}
