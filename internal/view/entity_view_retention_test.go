package view

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
	"time"
)

func TestClosedEntityWatchKeepsChargeUntilProjectionDrainsThenDropsCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capture := &capture{retired: true, watchers: 1, bytes: 1024, catalogue: make(map[domain.EntityID]Item)}
	owner := &Service{used: 1024}
	watch := &EntityWatch{service: owner, capture: capture, ctx: ctx, cancel: cancel}
	// Model an admitted projection that has not relinquished its frozen seed.
	watch.next.Lock()
	if err := watch.Close(); err != nil {
		t.Fatal(err)
	}
	owner.mu.Lock()
	used := owner.used
	owner.mu.Unlock()
	if used != 1024 {
		t.Fatal("active projection storage was released early", used)
	}
	watch.next.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		watch.next.Lock()
		retained := watch.capture
		watch.next.Unlock()
		owner.mu.Lock()
		used = owner.used
		owner.mu.Unlock()
		if retained == nil && used == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed watch retained uncharged capture", used, retained != nil)
		}
		time.Sleep(time.Millisecond)
	}
	// Keeping a closed watch handle alive must no longer keep its capture alive.
	if _, err := watch.Next(); err != context.Canceled {
		t.Fatal("closed watch became readable", err)
	}
}
