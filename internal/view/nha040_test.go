package view_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

func TestNHA040WatchSkipsNativeReplayAndLiveWithoutLosingStateCursor(t *testing.T) {
	f := newWatchFixture(t, 1)
	page := f.page(t, view.All())
	instance, _ := domain.ParseIntegrationInstanceID("fixture-watch")
	publishNative := func(sequence uint64) {
		t.Helper()
		stamp, err := domain.NewSourceStamp("nha040-watch", &sequence)
		if err != nil {
			t.Fatal(err)
		}
		edge, err := integrations.NewNativeMQTTEdge("buttons/press", []byte("pressed"), false, sequence)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.seq.AcceptNativeMQTTEdge(context.Background(), instance, stamp, edge); err != nil {
			t.Fatal(err)
		}
	}
	publishNative(1)
	f.publish(t, 0, true)
	watch := openWatch(t, f, page.Cursor)
	first := nextFrame(t, watch)
	if first.GetDelta() == nil || len(first.GetDelta().GetStates()) != 1 || first.GetDelta().GetStates()[0].GetEntity() == nil {
		t.Fatalf("state replay was displaced by native edge: %+v", first)
	}
	firstWire, err := proto.Marshal(first)
	if err != nil || bytes.Contains(firstWire, []byte("buttons/press")) || bytes.Contains(firstWire, []byte("pressed")) {
		t.Fatal("native replay topic or payload leaked into the view frame")
	}
	publishNative(2)
	shortCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if frame, err := watch.Next(shortCtx); !errors.Is(err, context.DeadlineExceeded) || frame != nil {
		t.Fatalf("native live edge became a view frame: %+v %v", frame, err)
	}
	f.publish(t, 0, false)
	second := nextFrame(t, watch)
	if second.GetDelta() == nil || len(second.GetDelta().GetStates()) != 1 || second.StreamSequence != first.StreamSequence+1 {
		t.Fatalf("state after native live edge was lost: %+v", second)
	}
	secondWire, err := proto.Marshal(second)
	if err != nil || bytes.Contains(secondWire, []byte("buttons/press")) || bytes.Contains(secondWire, []byte("pressed")) {
		t.Fatal("native live topic or payload leaked into the view frame")
	}
}
