package configactivation

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/view"
	"testing"
)

func TestNHA223DisplayProfilePortRejectsUnavailableOwnerAndInvalidActor(t *testing.T) {
	actor, _ := view.ParseActorID("nha223-synthetic")
	ctx := context.Background()
	var absent *Owner
	if _, err := absent.DisplayTimeZone(ctx, actor); !errors.Is(err, ErrInvalidStartup) {
		t.Fatal(err)
	}
	closed := &Owner{phase: ownerClosed}
	if _, err := closed.DisplayTimeZone(ctx, actor); !errors.Is(err, ErrActivationUnavailable) {
		t.Fatal(err)
	}
	if _, _, err := closed.SubscribeDisplayProfileChanges(ctx, actor); !errors.Is(err, ErrActivationUnavailable) {
		t.Fatal(err)
	}
	if _, err := closed.DisplayTimeZone(ctx, view.ActorID{}); !errors.Is(err, ErrInvalidStartup) {
		t.Fatal(err)
	}
	if _, _, err := closed.SubscribeDisplayProfileChanges(ctx, view.ActorID{}); !errors.Is(err, ErrInvalidStartup) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := closed.DisplayTimeZone(canceled, actor); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
