package server

import (
	"crypto/sha256"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nhf014AdmissionFixture(t *testing.T, value *media.MediaAction) (*media.Snapshot, *actions.CommandRequest, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	descriptor := sha256.Sum256([]byte("synthetic-admission-test-descriptor"))
	route := &media.Route{EntityId: "synthetic-reader", Endpoint: "playback", CoordinatorEntityId: "synthetic-coordinator-a", TopologyRevision: 1, AdmissionEpoch: "00000000000000000000000000000114", ConfigRevision: 7}
	snapshot := &media.Snapshot{Route: route, DescriptorSha256: descriptor[:], DurationMillis: proto.Uint64(120_000), Capabilities: &media.Capabilities{Operations: []media.Operation{media.Operation_OPERATION_PLAY}, VolumeBasisPoints: &media.QuantityBounds{Maximum: 10_000, Step: 100}, PositionMillis: &media.QuantityBounds{Maximum: 120_000, Step: 1000}}}
	value.Route = proto.Clone(route).(*media.Route)
	payload, err := proto.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	command := &actions.CommandRequest{CommandId: "synthetic-command", AdmissionEpoch: route.AdmissionEpoch, ConfigRevision: route.ConfigRevision, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: route.EntityId}}, CapabilitySchemaId: nhf014MediaSchema, Endpoint: route.Endpoint, ExpiresAt: timestamppb.New(now.Add(20 * time.Second)), Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: nhf014MediaSchema, Major: 1, DescriptorSha256: append([]byte(nil), snapshot.DescriptorSha256...), ProtobufPayload: payload}}}}
	return snapshot, command, now
}

func TestNHF014MediaRouteChangesAndWithdrawnCapabilitiesRejectUnsentIntent(t *testing.T) {
	snapshot, command, now := nhf014AdmissionFixture(t, &media.MediaAction{Command: &media.MediaAction_Operation{Operation: media.Operation_OPERATION_PLAY}})
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err != nil {
		t.Fatal(err)
	}
	snapshot.Route.CoordinatorEntityId = "synthetic-coordinator-b"
	snapshot.Route.TopologyRevision++
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("old media intent was retargeted")
	}
	var original media.MediaAction
	if err := proto.Unmarshal(command.GetArguments().GetExtension().ProtobufPayload, &original); err != nil || original.Route.CoordinatorEntityId != "synthetic-coordinator-a" {
		t.Fatal("original route changed", err)
	}
	snapshot, command, now = nhf014AdmissionFixture(t, &media.MediaAction{Command: &media.MediaAction_Operation{Operation: media.Operation_OPERATION_PLAY}})
	snapshot.Capabilities.Operations = nil
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("withdrawn operation accepted")
	}
}

func TestNHF014MediaCanonicalZeroAndBounds(t *testing.T) {
	for _, quantity := range []uint32{0, 100, 10_000} {
		snapshot, command, now := nhf014AdmissionFixture(t, &media.MediaAction{Command: &media.MediaAction_VolumeBasisPoints{VolumeBasisPoints: quantity}})
		if _, err := nhf014AdmitMediaAction(command, snapshot, now); err != nil {
			t.Fatal(quantity, err)
		}
	}
	for _, quantity := range []uint32{1, 10_001} {
		snapshot, command, now := nhf014AdmissionFixture(t, &media.MediaAction{Command: &media.MediaAction_VolumeBasisPoints{VolumeBasisPoints: quantity}})
		if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
			t.Fatal("invalid volume accepted", quantity)
		}
	}
	snapshot, command, now := nhf014AdmissionFixture(t, &media.MediaAction{Command: &media.MediaAction_PositionMillis{PositionMillis: 0}})
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err != nil {
		t.Fatal(err)
	}
	snapshot.DurationMillis = nil
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("seek without duration accepted")
	}
}

func TestNHF014MediaDescriptorAndExpiryRejectMalformedOrUntrustedPayload(t *testing.T) {
	snapshot, command, now := nhf014AdmissionFixture(t, &media.MediaAction{Command: &media.MediaAction_Operation{Operation: media.Operation_OPERATION_PLAY}})
	original := command.Arguments.GetExtension().DescriptorSha256[0]
	command.Arguments.GetExtension().DescriptorSha256[0] ^= 0xff
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("different descriptor accepted")
	}
	command.Arguments.GetExtension().DescriptorSha256[0] = original
	command.ExpiresAt = timestamppb.New(now.Add(-time.Second))
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("expired command accepted")
	}
	command.ExpiresAt = timestamppb.New(now.Add(time.Hour))
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("unbounded lifetime accepted")
	}
	command.ExpiresAt = timestamppb.New(now.Add(time.Second))
	command.Arguments.GetExtension().ProtobufPayload = []byte{0xff}
	if _, err := nhf014AdmitMediaAction(command, snapshot, now); err == nil {
		t.Fatal("malformed payload accepted")
	}
}
