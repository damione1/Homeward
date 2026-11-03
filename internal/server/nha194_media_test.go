package server

import (
	"errors"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/androidtv"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sonos"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA194MediaProjectionScopesProviderAndPreservesObservedOff(t *testing.T) {
	id, _ := domain.ParseEntityID("native-player")
	value := &media.Snapshot{Route: &media.Route{EntityId: "native-player", CoordinatorEntityId: "native-player", Endpoint: "player"}, PoweredOn: proto.Bool(false), Members: []*media.Member{{EntityId: "native-player", Label: "Player"}, {EntityId: "private-member", Label: "Hidden"}}}
	sonosProbed := false
	sources := []nativeMediaSource{
		{sonos.Schema, sonos.Endpoint, func(domain.EntityID) (*media.Snapshot, bool) { sonosProbed = true; return nil, false }},
		{androidtv.Schema, androidtv.Endpoint, func(domain.EntityID) (*media.Snapshot, bool) { return proto.Clone(value).(*media.Snapshot), true }},
	}
	authorize := func(entity domain.EntityID, schema, endpoint string) error {
		text, _ := entity.Value()
		if text != "native-player" || schema != androidtv.Schema || endpoint != androidtv.Endpoint {
			return authorization.ErrDenied
		}
		return nil
	}
	got, err := projectNativeMedia(id, sources, 42, authorize)
	if err != nil {
		t.Fatal(err)
	}
	if sonosProbed || got.PoweredOn == nil || *got.PoweredOn || got.Playback != media.PlaybackState_PLAYBACK_STATE_UNSPECIFIED || got.Route.ConfigRevision != 42 || len(got.Members) != 1 || got.Members[0].EntityId != "native-player" {
		t.Fatalf("projection crossed its authorized provider or lost off: %v", got)
	}
	value.Route.CoordinatorEntityId = "private-member"
	if _, err := projectNativeMedia(id, sources, 42, authorize); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("unreadable coordinator leaked", err)
	}
}
func TestNHA194MediaProjectionDeniesBeforeProbeAndRejectsAmbiguousOwners(t *testing.T) {
	id, _ := domain.ParseEntityID("native-player")
	calls := 0
	source := nativeMediaSource{androidtv.Schema, androidtv.Endpoint, func(domain.EntityID) (*media.Snapshot, bool) {
		calls++
		return &media.Snapshot{Route: &media.Route{EntityId: "native-player", CoordinatorEntityId: "native-player", Endpoint: "player"}}, true
	}}
	deny := func(domain.EntityID, string, string) error { return authorization.ErrDenied }
	if _, err := projectNativeMedia(id, []nativeMediaSource{source}, 1, deny); !errors.Is(err, authorization.ErrDenied) || calls != 0 {
		t.Fatal("unreadable source was probed")
	}
	allow := func(domain.EntityID, string, string) error { return nil }
	if _, err := projectNativeMedia(id, []nativeMediaSource{source, source}, 1, allow); !errors.Is(err, sonos.ErrUnavailable) {
		t.Fatal("ambiguous ownership admitted", err)
	}
	source.snapshot = func(domain.EntityID) (*media.Snapshot, bool) { return nil, true }
	if _, err := projectNativeMedia(id, []nativeMediaSource{source}, 1, allow); !errors.Is(err, sonos.ErrUnavailable) {
		t.Fatal("nil provider result admitted", err)
	}
}
