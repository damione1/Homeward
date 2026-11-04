package apiresources

import (
	"bytes"
	"crypto/sha256"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"math"
	"sync"
)

var cachedMediaFingerprint = sync.OnceValues(func() ([]byte, error) {
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(protoreflect.FileDescriptor)
	add = func(file protoreflect.FileDescriptor) {
		if seen[file.Path()] {
			return
		}
		seen[file.Path()] = true
		for i := 0; i < file.Imports().Len(); i++ {
			add(file.Imports().Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(file))
	}
	add(native.File_home_media_v1_media_proto)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		return nil, ErrProjection
	}
	hash := sha256.Sum256(encoded)
	return append([]byte(nil), hash[:]...), nil
})

// Callers receive an owned copy; provider snapshots cannot mutate the cached
// descriptor authority used by subsequent admission checks.
func compiledMediaFingerprint() ([]byte, error) {
	value, err := cachedMediaFingerprint()
	return bytes.Clone(value), err
}

// WithMediaPlayback replaces a registered extension's metadata-only projection
// after the caller has obtained a coherent authorized native media capture. It
// accepts only the two current native codecs and their exact compiled descriptor.
// It does no I/O, grants no authority and never makes an unknown codec actionable.
// The exact trusted native route is retained privately after checking its native
// IDs against the canonical public identity hashes and all route preconditions.
// Compose the replacement before the runtime assigns the capture definition etag.
func WithMediaPlayback(c Capability, playback *api.MediaPlaybackCapability, nativeRoute *native.Route) (Capability, error) {
	if c.trait != Opaque || c.value == nil || playback == nil {
		return Capability{}, ErrUnsupportedTrait
	}
	switch playback.DataProvenance {
	case api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_OBSERVED, api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC:
	default:
		return Capability{}, ErrProjection
	}
	source := c.value.GetOpaque()
	schema, _, err := c.source.Values()
	if err != nil || source == nil || source.SchemaId != schema || source.MajorVersion != 1 {
		return Capability{}, ErrProjection
	}
	switch schema {
	case "home.media.v1", "androidtv.media.v1":
	default:
		return Capability{}, ErrUnsupportedTrait
	}
	fingerprint, err := compiledMediaFingerprint()
	if err != nil || !bytes.Equal(fingerprint, source.DescriptorSha256) {
		return Capability{}, ErrUnsupportedTrait
	}
	definition := playback.Definition
	switch c.actionMessage {
	case "":
		if playback.ActionAccess == api.ActionAccess_ACTION_ACCESS_ALLOWED || definition != nil && (len(definition.Operations) > 0 || definition.VolumeBasisPoints != nil || definition.Position != nil || len(definition.Sources) > 0 || len(definition.Groups) > 0) {
			return Capability{}, ErrProjection
		}
	case "home.media.v1.MediaAction":
		if schema != "home.media.v1" {
			return Capability{}, ErrUnsupportedTrait
		}
	default:
		return Capability{}, ErrUnsupportedTrait
	}
	original, err := apitypes.ParseCapabilityName(c.value.Name)
	if err != nil {
		return Capability{}, ErrProjection
	}
	publicRoute := playback.GetObservation().GetRoute()
	if nativeRoute == nil {
		if publicRoute != nil || playback.ActionAccess == api.ActionAccess_ACTION_ACCESS_ALLOWED {
			return Capability{}, ErrProjection
		}
	} else {
		if publicRoute == nil || nativeRoute.TopologyRevision > math.MaxInt64 || nativeRoute.ConfigRevision > math.MaxInt64 {
			return Capability{}, ErrProjection
		}
		nativeEntity, err := domain.ParseEntityID(nativeRoute.EntityId)
		if err != nil {
			return Capability{}, ErrProjection
		}
		nativeCoordinator, err := domain.ParseEntityID(nativeRoute.CoordinatorEntityId)
		if err != nil {
			return Capability{}, ErrProjection
		}
		home := original.Entity().Home()
		publicEntity, err := apitypes.EntityNameForIdentity(home, nativeEntity)
		if err != nil || publicEntity != original.Entity() {
			return Capability{}, ErrProjection
		}
		publicCoordinator, err := apitypes.EntityNameForIdentity(home, nativeCoordinator)
		if err != nil {
			return Capability{}, ErrProjection
		}
		_, endpoint, err := c.source.Values()
		if err != nil || endpoint != nativeRoute.Endpoint {
			return Capability{}, ErrProjection
		}
		admitted, err := apitypes.NewMediaRoute(publicEntity, endpoint, publicCoordinator, int64(nativeRoute.TopologyRevision), nativeRoute.AdmissionEpoch, int64(nativeRoute.ConfigRevision))
		if err != nil {
			return Capability{}, ErrProjection
		}
		if publicRoute.Entity != admitted.Entity().String() || publicRoute.Coordinator != admitted.Coordinator().String() || publicRoute.Endpoint != admitted.Endpoint() || publicRoute.TopologyRevision != admitted.TopologyRevision() || publicRoute.ConfigRevision != admitted.ConfigRevision() || publicRoute.AdmissionEpoch != admitted.AdmissionEpoch() {
			return Capability{}, ErrProjection
		}
	}
	c.nativeMediaRoute = proto.CloneOf(nativeRoute)
	name, err := capabilityName(original.Entity(), c.source, Media)
	if err != nil {
		return Capability{}, err
	}
	c.trait = Media
	c.value = &api.Capability{Name: name.String(), Etag: c.value.Etag, Trait: &api.Capability_MediaPlayback{MediaPlayback: proto.CloneOf(playback)}}
	return c, nil
}
