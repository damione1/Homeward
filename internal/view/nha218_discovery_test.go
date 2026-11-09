package view_test

import (
	"context"
	"testing"

	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHA218IdenticalDiscoveryKeepsWatchAndRealChangeResyncs(t *testing.T) {
	f := newWatchFixture(t, 2)
	page := f.page(t, view.All())
	watch := openWatch(t, f, page.Cursor)
	defer watch.Close()
	before, err := f.seq.Capture()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := f.seq.CataloguePosition()
	if err != nil {
		t.Fatal(err)
	}
	// Reverse batch order: identity, not arrival order, determines equivalence.
	candidates := make([]integrations.Candidate, 0, 2)
	for _, device := range []string{"device-b", "device-a"} {
		key, _ := integrations.NewSourceKey(device, "relay")
		candidates = append(candidates, testCandidate(t, key, "Read only relay"))
	}
	for i := 0; i < 2; i++ {
		if _, err := f.session.Discover(context.Background(), candidates); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.seq.Capture()
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.seq.CataloguePosition()
	if err != nil {
		t.Fatal(err)
	}
	if after.Cursor != before.Cursor || current != catalogue {
		t.Fatal("unchanged discovery advanced journal/catalogue")
	}
	if _, err := f.service.ReadPage(context.Background(), f.actor, page.SnapshotToken, page.NextPageToken); err != nil {
		t.Fatal("unchanged discovery invalidated frozen page", err)
	}
	f.publish(t, 0, true)
	if frame := nextFrame(t, watch); frame.GetDelta() == nil || len(frame.GetDelta().States) != 1 {
		t.Fatal("observation after identical discovery invalidated watch", frame)
	}
	key, _ := integrations.NewSourceKey("device-a", "relay")
	if _, err := f.session.Discover(context.Background(), []integrations.Candidate{testCandidate(t, key, "Changed declaration")}); err != nil {
		t.Fatal(err)
	}
	requireResync(t, nextFrame(t, watch), statewire.ResyncReason_RESYNC_REASON_CAPABILITIES_CHANGED)
}
