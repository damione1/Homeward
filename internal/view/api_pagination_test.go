package view_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHA201PaginationChangesSizeWithinImmutableAuthorizedCut(t *testing.T) {
	f := newWatchFixture(t, 6)
	for i := range f.ids {
		f.publish(t, i, false)
	}
	ctx := context.Background()
	first, err := f.service.OpenView(ctx, f.actor, view.All(), 1)
	if err != nil || len(first.Items) != 1 || first.NextPageToken == "" {
		t.Fatal(err)
	}
	// Later observations cannot alter already captured pages. Continuations may
	// change their size and can be retried without retaining a replay map.
	for i := range f.ids {
		f.publish(t, i, true)
	}
	second, err := f.service.ReadPageSize(ctx, f.actor, first.SnapshotToken, first.NextPageToken, 3)
	if err != nil || len(second.Items) != 3 || second.NextPageToken == "" {
		t.Fatal("changed page size", err)
	}
	retry, err := f.service.ReadPageSize(ctx, f.actor, first.SnapshotToken, first.NextPageToken, 2)
	if err != nil || len(retry.Items) != 2 {
		t.Fatal("same token with different page size", err)
	}
	_, secondID, _ := second.Items[0].Registry().IDs()
	_, retryID, _ := retry.Items[0].Registry().IDs()
	if secondID != retryID {
		t.Fatal("continuation changed its offset")
	}
	third, err := f.service.ReadPageSize(ctx, f.actor, first.SnapshotToken, second.NextPageToken, 500)
	if err != nil || len(third.Items) != 2 || third.NextPageToken != "" {
		t.Fatal("final page", err)
	}
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	previous := ""
	for _, page := range []view.Page{first, second, third} {
		for _, item := range page.Items {
			_, id, _ := item.Registry().IDs()
			name, _ := apitypes.EntityNameForIdentity(home, id)
			if name.String() <= previous {
				t.Fatal("resources not in canonical name order")
			}
			previous = name.String()
			state, present := item.State()
			if !present {
				t.Fatal("lost observed state")
			}
			value, hasValue := state.CurrentValue()
			power, isSwitch := value.Switch()
			if !hasValue || !isSwitch || power {
				t.Fatal("page mixed observations from another cut")
			}
		}
	}
	if _, err := f.service.ReadPageSize(ctx, f.actor, first.SnapshotToken, strings.Repeat("f", 64), 1); !errors.Is(err, view.ErrPageToken) {
		t.Fatal("forged continuation", err)
	}
	grant, err := view.NewGrant(f.actor, 2, f.ids)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.SetGrant(grant)
	if _, err := f.service.ReadPageSize(ctx, f.actor, first.SnapshotToken, first.NextPageToken, 2); !errors.Is(err, view.ErrViewChanged) {
		t.Fatal("stale rights returned cached data", err)
	}
}
