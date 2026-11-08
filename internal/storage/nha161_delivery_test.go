package storage

import (
	"context"
	"testing"
	"time"
)

func TestNHA161CalendarClaimsAndCoverageSurviveReopen(t *testing.T) {
	store, settings := nha051Store(t)
	ctx := context.Background()
	key := [32]byte{1}
	now := time.Now().UTC().Truncate(time.Second)
	if claimed, err := store.ClaimCalendarDelivery(ctx, key, now); err != nil || !claimed {
		t.Fatalf("first claim %v %v", claimed, err)
	}
	if err := store.CommitCalendarCoverage(ctx, "calendar/synthetic", now, [][32]byte{{2}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if claimed, err := reopened.ClaimCalendarDelivery(ctx, key, now.Add(time.Hour)); err != nil || claimed {
		t.Fatalf("replayed claim %v %v", claimed, err)
	}
	if claimed, err := reopened.ClaimCalendarDelivery(ctx, [32]byte{2}, now.Add(time.Hour)); err != nil || claimed {
		t.Fatal("missed batch identity lost on reopen", claimed, err)
	}
	if previous, err := reopened.ReadCalendarCoverage(ctx, "calendar/synthetic"); err != nil || !previous.Equal(now) {
		t.Fatalf("lost coverage %v %v", previous, err)
	}
	if _, err := reopened.AdvanceCalendarCoverage(ctx, "calendar/synthetic", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if previous, err := reopened.ReadCalendarCoverage(ctx, "calendar/synthetic"); err != nil || !previous.Equal(now) {
		t.Fatal("backwards checkpoint", previous, err)
	}
}
