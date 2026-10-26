package view_test

import (
	"context"
	"errors"
	"testing"

	environmentwire "github.com/damione1/Homeward/gen/home/environment/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

func TestNHF010DisplayProfileBelongsToTheAuthorizedSnapshotAndSignedResume(t *testing.T) {
	f := newWatchFixture(t, 2)
	original := f.auth.grant
	profiled, err := original.WithDisplayTimeZone("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	f.auth.SetGrant(profiled)
	page, err := f.service.OpenView(context.Background(), f.actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := view.EncodePage(page)
	if err != nil || encoded.GetDisplayProfile().GetTimeZone() != "America/Toronto" || encoded.NextPageToken == "" {
		t.Fatal("profiled first page", encoded, err)
	}
	cursor := proto.CloneOf(encoded.Cursor)
	// A caller cannot mutate the retained capture through either Page or wire.
	page.DisplayProfile.TimeZone = "Europe/Paris"
	encoded.DisplayProfile.TimeZone = "Europe/Paris"
	tail, err := f.service.ReadPage(context.Background(), f.actor, page.SnapshotToken, page.NextPageToken)
	if err != nil || tail.DisplayProfile.TimeZone != "America/Toronto" {
		t.Fatal("profile capture mutated", tail, err)
	}
	same, err := profiled.WithDisplayTimeZone("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	f.auth.SetGrant(same)
	if _, err := f.service.ReadPage(context.Background(), f.actor, page.SnapshotToken, page.NextPageToken); err != nil {
		t.Fatal("idempotent profile changed generation", err)
	}
	watch := openWatch(t, f, cursor)
	changed, err := profiled.WithDisplayTimeZone("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	f.auth.SetGrant(changed)
	if _, err := f.service.ReadPage(context.Background(), f.actor, page.SnapshotToken, page.NextPageToken); !errors.Is(err, view.ErrViewChanged) {
		t.Fatal("changed profile kept old page", err)
	}
	requireResync(t, nextFrame(t, watch), statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED)
	stale, err := f.service.WatchView(context.Background(), f.actor, watchRequest(cursor))
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	requireResync(t, nextFrame(t, stale), statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED)
	next := f.page(t, view.All())
	if next.GetDisplayProfile().GetTimeZone() != "Europe/Paris" || string(next.Cursor.GrantSha256) == string(cursor.GrantSha256) {
		t.Fatal("new profile not bound to new cursor", next)
	}
	f.auth.SetGrant(original)
	absent := f.page(t, view.All())
	if absent.DisplayProfile != nil {
		t.Fatal("absent profile inferred from machine", absent.DisplayProfile)
	}
}

func TestNHF010DisplayProfileRejectsUntrustedMachineFallbackAndMalformedMetadata(t *testing.T) {
	f := newWatchFixture(t, 1)
	for _, zone := range []string{"", "Local", "future/unknown", " America/Toronto"} {
		if _, err := f.auth.grant.WithDisplayTimeZone(zone); !errors.Is(err, view.ErrInvalidInput) {
			t.Fatalf("zone %q: %v", zone, err)
		}
	}
	if _, err := (view.Grant{}).WithDisplayTimeZone("UTC"); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal("invalid grant admitted", err)
	}
	page, err := f.service.OpenView(context.Background(), f.actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	page.DisplayProfile = &environmentwire.DisplayProfile{TimeZone: "Local"}
	if _, err := view.EncodePage(page); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal("machine timezone encoded", err)
	}
	page.DisplayProfile.TimeZone = "UTC"
	page.DisplayProfile.ProtoReflect().SetUnknown([]byte{0x10, 1})
	if _, err := view.EncodePage(page); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal("unknown profile fields encoded", err)
	}
}
