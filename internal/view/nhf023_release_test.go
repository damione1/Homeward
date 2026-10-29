package view_test

import (
	"context"
	"errors"
	"testing"

	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHF023ReleaseRestoresCapacityWithoutEvictingAnotherActor(t *testing.T) {
	f := newWatchFixture(t, 2)
	ctx := context.Background()
	pages := make([]*statewire.OpenViewResponse, 0, view.MaxSnapshots)
	for range view.MaxSnapshots {
		pages = append(pages, f.page(t, view.All()))
	}
	if _, err := f.service.OpenView(ctx, f.actor, view.All(), 1); !errors.Is(err, view.ErrResourceExhausted) {
		t.Fatalf("snapshot bound changed: %v", err)
	}
	first := pages[0]
	other := testActor(t, "other-reader")
	if err := f.service.ReleaseView(ctx, other, first.SnapshotToken); err != nil {
		t.Fatal("foreign release must not disclose token ownership:", err)
	}
	if _, err := f.service.ReadPage(ctx, f.actor, first.SnapshotToken, first.NextPageToken); err != nil {
		t.Fatal("foreign release retired the owner's capture:", err)
	}
	if _, err := f.service.OpenView(ctx, f.actor, view.All(), 1); !errors.Is(err, view.ErrResourceExhausted) {
		t.Fatal("foreign release changed capacity")
	}
	if err := f.service.ReleaseView(ctx, f.actor, first.SnapshotToken); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ReadPage(ctx, f.actor, first.SnapshotToken, first.NextPageToken); !errors.Is(err, view.ErrSnapshotUnknown) {
		t.Fatal("released pagination token remained usable:", err)
	}
	// Fifteen unrelated captures remain reserved. A single explicitly released
	// slot supports100fresh reads without increasing either global budget.
	for range 100 {
		page, err := f.service.OpenView(ctx, f.actor, view.All(), 1)
		if err != nil {
			t.Fatal("consumed snapshot still occupied its slot:", err)
		}
		if err := f.service.ReleaseView(ctx, f.actor, page.SnapshotToken); err != nil {
			t.Fatal(err)
		}
		if err := f.service.ReleaseView(ctx, f.actor, page.SnapshotToken); err != nil {
			t.Fatal("release is not idempotent:", err)
		}
	}
	if _, err := f.service.ReadPage(ctx, f.actor, pages[1].SnapshotToken, pages[1].NextPageToken); err != nil {
		t.Fatal("release evicted an unrelated capture:", err)
	}
}

func TestNHF023ReleaseDoesNotRevokeTheIndependentWatchCursor(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	if err := f.service.ReleaseView(context.Background(), f.actor, page.SnapshotToken); err != nil {
		t.Fatal(err)
	}
	watch := openWatch(t, f, page.Cursor)
	f.publish(t, 0, true)
	frame := nextFrame(t, watch)
	if frame.GetDelta() == nil || len(frame.GetDelta().States) != 1 {
		t.Fatal("releasing pagination invalidated the authorized watch cursor")
	}
}
