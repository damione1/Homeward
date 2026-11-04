package apiresources

import (
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/media/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func nha202MediaSnapshot(t *testing.T) (apitypes.HomeName, *native.Snapshot) {
	t.Helper()
	home, err := apitypes.ParseHomeName("homes/synthetic-media")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := compiledMediaFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return home, &native.Snapshot{Route: &native.Route{EntityId: "native_player_id", CoordinatorEntityId: "native_player_id", Endpoint: "player", TopologyRevision: 7, ConfigRevision: 9, AdmissionEpoch: "0123456789abcdef0123456789abcdef"}, Capabilities: &native.Capabilities{Operations: []native.Operation{native.Operation_OPERATION_PLAY}, VolumeBasisPoints: &native.QuantityBounds{Minimum: 0, Maximum: 10000, Step: 100}, PositionMillis: &native.QuantityBounds{Minimum: 0, Maximum: 120000, Step: 1000}}, ActionAccess: state.ActionAccess_ACTION_ACCESS_ALLOWED, Playback: native.PlaybackState_PLAYBACK_STATE_PLAYING, VolumeBasisPoints: proto.Uint32(0), PositionMillis: proto.Uint64(0), DurationMillis: proto.Uint64(120000), PoweredOn: proto.Bool(false), Members: []*native.Member{{EntityId: "native_player_id", Label: "Synthetic player"}}, Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, ReceivedAt: timestamppb.New(time.Now()), DescriptorSha256: fingerprint}
}
func TestNHA202MediaProjectionPreservesZeroIndependentPowerAndPrivateIdentity(t *testing.T) {
	home, snapshot := nha202MediaSnapshot(t)
	projection, err := ProjectMediaPlayback(home, snapshot, api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC)
	if err != nil {
		t.Fatal(err)
	}
	observation := projection.Observation
	if observation.VolumeBasisPoints == nil || *observation.VolumeBasisPoints != 0 || observation.Position == nil || observation.Position.Seconds != 0 || observation.Position.Nanos != 0 || observation.PoweredOn == nil || *observation.PoweredOn || observation.Playback != api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_PLAYING {
		t.Fatal("explicit zero or independent power/playback was coerced")
	}
	if observation.Route.Entity == snapshot.Route.EntityId || observation.Route.Coordinator == snapshot.Route.CoordinatorEntityId || observation.Members[0].Entity == snapshot.Members[0].EntityId {
		t.Fatal("native identity escaped as a public resource name")
	}
	snapshot.Members[0].Label = "mutated"
	*snapshot.PoweredOn = true
	*snapshot.VolumeBasisPoints = 9000
	if observation.Members[0].DisplayName != "Synthetic player" || *observation.PoweredOn || *observation.VolumeBasisPoints != 0 {
		t.Fatal("projection retained mutable provider-owned content")
	}
}
func TestNHA202MediaProjectionRejectsDescriptorQualityAndAdvertisedBounds(t *testing.T) {
	cases := map[string]func(*native.Snapshot){"descriptor": func(s *native.Snapshot) { s.DescriptorSha256[0] ^= 1 },
		"invalid-utf8": func(s *native.Snapshot) { s.Title = proto.String(string([]byte{0xff})) }, "quality": func(s *native.Snapshot) { s.Quality.Provenance = state.Provenance(999) }, "operation": func(s *native.Snapshot) { s.Capabilities.Operations = []native.Operation{native.Operation(999)} }, "volume": func(s *native.Snapshot) { s.Capabilities.VolumeBasisPoints.Maximum = 10001 }, "zero-step": func(s *native.Snapshot) { s.Capabilities.VolumeBasisPoints.Step = 0 }, "position": func(s *native.Snapshot) { s.PositionMillis = proto.Uint64(uint64(apitypes.MaxMediaPositionMillis) + 1) }, "duration": func(s *native.Snapshot) { s.Capabilities.PositionMillis.Maximum = 120001 }, "duplicate-member": func(s *native.Snapshot) { s.Members = append(s.Members, proto.CloneOf(s.Members[0])) }, "duplicate-option": func(s *native.Snapshot) {
			s.Capabilities.Sources = []*native.Option{{Id: "same", Label: "A"}, {Id: "same", Label: "B"}}
		}}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			home, snapshot := nha202MediaSnapshot(t)
			mutate(snapshot)
			if _, err := ProjectMediaPlayback(home, snapshot, api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC); err == nil {
				t.Fatal("invalid native projection was admitted")
			}
		})
	}
}
func TestNHA202MediaNativeBridgeUsesExactRetainedRouteAndCurrentPermissions(t *testing.T) {
	home, snapshot := nha202MediaSnapshot(t)
	playback, err := ProjectMediaPlayback(home, snapshot, api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := authorization.NewCapabilitySlot("home.media.v1", "player")
	if err != nil {
		t.Fatal(err)
	}
	c := Capability{source: slot, trait: Media, actionMessage: "home.media.v1.MediaAction", nativeMediaRoute: proto.CloneOf(snapshot.Route), value: &api.Capability{Trait: &api.Capability_MediaPlayback{MediaPlayback: playback}}}
	entity, _ := apitypes.ParseEntityName(playback.Observation.Route.Entity)
	coordinator, _ := apitypes.ParseEntityName(playback.Observation.Route.Coordinator)
	route, err := apitypes.NewMediaRoute(entity, "player", coordinator, 7, snapshot.Route.AdmissionEpoch, 9)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := apitypes.MediaVolume(route, 0)
	if err != nil {
		t.Fatal(err)
	}
	args, err := NativeArguments(c, intent)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &native.MediaAction{}
	if err := proto.Unmarshal(args.GetExtension().ProtobufPayload, decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Route.EntityId != "native_player_id" || decoded.Route.CoordinatorEntityId != "native_player_id" || decoded.GetVolumeBasisPoints() != 0 {
		t.Fatal("bridge guessed/reversed a public ID or lost explicit zero")
	}
	if _, ok := decoded.Command.(*native.MediaAction_VolumeBasisPoints); !ok {
		t.Fatal("zero volume intention became absent")
	}
	changed, err := apitypes.NewMediaRoute(entity, "player", coordinator, 8, snapshot.Route.AdmissionEpoch, 9)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := apitypes.MediaVolume(changed, 0)
	if _, err := NativeArguments(c, stale); !errors.Is(err, ErrMediaTopologyChanged) {
		t.Fatal("stale route did not fail before native command construction")
	}
	playback.ActionAccess = api.ActionAccess_ACTION_ACCESS_DENIED
	if _, err := NativeArguments(c, intent); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("current access denial ignored")
	}
	playback.ActionAccess = api.ActionAccess_ACTION_ACCESS_ALLOWED
	c.actionMessage = ""
	if _, err := NativeArguments(c, intent); err == nil {
		t.Fatal("read-only native codec became executable")
	}
}
