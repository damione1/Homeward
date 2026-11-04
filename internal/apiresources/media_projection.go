package apiresources

import (
	"bytes"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/media/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func mediaLabel(raw string) bool {
	if len(raw) > 1024 || !utf8.ValidString(raw) {
		return false
	}
	for _, r := range raw {
		if unicode.IsControl(r) && r != '\n' {
			return false
		}
	}
	return true
}
func mediaEntityName(home apitypes.HomeName, raw string) (string, error) {
	id, err := domain.ParseEntityID(raw)
	if err != nil {
		return "", ErrProjection
	}
	name, err := apitypes.EntityNameForIdentity(home, id)
	return name.String(), err
}
func mediaDuration(millis uint64) (*durationpb.Duration, error) {
	if millis > uint64(apitypes.MaxMediaPositionMillis) {
		return nil, ErrProjection
	}
	return &durationpb.Duration{Seconds: int64(millis / 1000), Nanos: int32(millis%1000) * 1000000}, nil
}
func mediaOperation(value native.Operation) (api.MediaOperation, error) {
	switch value {
	case native.Operation_OPERATION_PLAY:
		return api.MediaOperation_MEDIA_OPERATION_PLAY, nil
	case native.Operation_OPERATION_PAUSE:
		return api.MediaOperation_MEDIA_OPERATION_PAUSE, nil
	case native.Operation_OPERATION_STOP:
		return api.MediaOperation_MEDIA_OPERATION_STOP, nil
	case native.Operation_OPERATION_PREVIOUS:
		return api.MediaOperation_MEDIA_OPERATION_PREVIOUS, nil
	case native.Operation_OPERATION_NEXT:
		return api.MediaOperation_MEDIA_OPERATION_NEXT, nil
	case native.Operation_OPERATION_UNSPECIFIED:
		return 0, ErrProjection
	default:
		return 0, ErrProjection
	}
}
func mediaOptions(raw []*native.Option) ([]*api.MediaOption, error) {
	if len(raw) > 16 {
		return nil, ErrProjection
	}
	out := make([]*api.MediaOption, 0, len(raw))
	seen := map[string]bool{}
	for _, value := range raw {
		if value == nil || !apitypes.ValidMediaIdentifier(value.Id) || !mediaLabel(value.Label) || value.Label == "" || seen[value.Id] {
			return nil, ErrProjection
		}
		seen[value.Id] = true
		out = append(out, &api.MediaOption{Id: strings.Clone(value.Id), DisplayName: strings.Clone(value.Label)})
	}
	return out, nil
}

// ProjectMediaPlayback converts a bounded, already authorized native source
// capture. It never probes providers, resolves aliases or grants action access.
// The caller keeps the existing configuration/authority capture fences alive.
func ProjectMediaPlayback(home apitypes.HomeName, raw *native.Snapshot, provenance api.MediaDataProvenance) (*api.MediaPlaybackCapability, error) {
	if provenance != api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_OBSERVED && provenance != api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC {
		return nil, ErrProjection
	}
	if !home.Valid() || raw == nil || proto.Size(raw) > 64<<10 || raw.Route == nil || raw.Capabilities == nil || raw.ReceivedAt == nil || raw.ReceivedAt.CheckValid() != nil {
		return nil, ErrProjection
	}
	fingerprint, err := compiledMediaFingerprint()
	if err != nil || !bytes.Equal(fingerprint, raw.DescriptorSha256) {
		return nil, ErrProjection
	}
	r := raw.Route
	if r.TopologyRevision > math.MaxInt64 || r.ConfigRevision > math.MaxInt64 {
		return nil, ErrProjection
	}
	entity, err := mediaEntityName(home, r.EntityId)
	if err != nil {
		return nil, err
	}
	coordinator, err := mediaEntityName(home, r.CoordinatorEntityId)
	if err != nil {
		return nil, err
	}
	route := &api.MediaRoute{Entity: entity, Endpoint: r.Endpoint, Coordinator: coordinator, TopologyRevision: int64(r.TopologyRevision), AdmissionEpoch: r.AdmissionEpoch, ConfigRevision: int64(r.ConfigRevision)}
	q, err := quality(raw.Quality)
	if err != nil {
		return nil, err
	}
	out := &api.MediaPlaybackCapability{DataProvenance: provenance, Definition: &api.MediaPlaybackDefinition{}, Observation: &api.MediaPlaybackObservation{Route: route, Quality: q, ObservedTime: proto.CloneOf(raw.ReceivedAt), PoweredOn: raw.PoweredOn}}
	if raw.PoweredOn != nil {
		value := *raw.PoweredOn
		out.Observation.PoweredOn = &value
	}
	switch raw.ActionAccess {
	case state.ActionAccess_ACTION_ACCESS_ALLOWED:
		out.ActionAccess = api.ActionAccess_ACTION_ACCESS_ALLOWED
	case state.ActionAccess_ACTION_ACCESS_DENIED:
		out.ActionAccess = api.ActionAccess_ACTION_ACCESS_DENIED
	case state.ActionAccess_ACTION_ACCESS_UNSPECIFIED:
		out.ActionAccess = api.ActionAccess_ACTION_ACCESS_UNSPECIFIED
	default:
		return nil, ErrProjection
	}
	switch raw.Playback {
	case native.PlaybackState_PLAYBACK_STATE_UNSPECIFIED:
		out.Observation.Playback = api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_UNSPECIFIED
	case native.PlaybackState_PLAYBACK_STATE_STOPPED:
		out.Observation.Playback = api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_STOPPED
	case native.PlaybackState_PLAYBACK_STATE_PAUSED:
		out.Observation.Playback = api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_PAUSED
	case native.PlaybackState_PLAYBACK_STATE_PLAYING:
		out.Observation.Playback = api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_PLAYING
	case native.PlaybackState_PLAYBACK_STATE_BUFFERING:
		out.Observation.Playback = api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_BUFFERING
	default:
		return nil, ErrProjection
	}
	labels := []*string{raw.Title, raw.Artist, raw.Collection}
	copied := []**string{&out.Observation.Title, &out.Observation.Artist, &out.Observation.Collection}
	for i, text := range labels {
		if text != nil {
			if !mediaLabel(*text) {
				return nil, ErrProjection
			}
			value := strings.Clone(*text)
			*copied[i] = &value
		}
	}
	if raw.VolumeBasisPoints != nil {
		if *raw.VolumeBasisPoints > 10000 {
			return nil, ErrProjection
		}
		value := int32(*raw.VolumeBasisPoints)
		out.Observation.VolumeBasisPoints = &value
	}
	if raw.PositionMillis != nil {
		out.Observation.Position, err = mediaDuration(*raw.PositionMillis)
		if err != nil {
			return nil, err
		}
	}
	if raw.DurationMillis != nil {
		out.Observation.Duration, err = mediaDuration(*raw.DurationMillis)
		if err != nil {
			return nil, err
		}
	}
	if raw.PositionMillis != nil && raw.DurationMillis != nil && *raw.PositionMillis > *raw.DurationMillis {
		return nil, ErrProjection
	}
	if raw.SourceId != nil {
		if !apitypes.ValidMediaIdentifier(*raw.SourceId) {
			return nil, ErrProjection
		}
		value := strings.Clone(*raw.SourceId)
		out.Observation.SourceId = &value
	}
	if raw.GroupId != nil {
		if !apitypes.ValidMediaIdentifier(*raw.GroupId) {
			return nil, ErrProjection
		}
		value := strings.Clone(*raw.GroupId)
		out.Observation.GroupId = &value
	}
	if len(raw.Members) > 16 {
		return nil, ErrProjection
	}
	seenMembers := map[string]bool{}
	for _, member := range raw.Members {
		if member == nil || seenMembers[member.EntityId] || !mediaLabel(member.Label) {
			return nil, ErrProjection
		}
		seenMembers[member.EntityId] = true
		name, err := mediaEntityName(home, member.EntityId)
		if err != nil {
			return nil, err
		}
		out.Observation.Members = append(out.Observation.Members, &api.MediaMember{Entity: name, DisplayName: strings.Clone(member.Label)})
	}
	if raw.Artwork != nil {
		if len(raw.Artwork.Reference) < 1 || len(raw.Artwork.Reference) > 512 || raw.Artwork.ExpiresAt == nil || raw.Artwork.ExpiresAt.CheckValid() != nil {
			return nil, ErrProjection
		}
		out.Observation.Artwork = &api.MediaArtworkReference{Reference: bytes.Clone(raw.Artwork.Reference), ExpireTime: proto.CloneOf(raw.Artwork.ExpiresAt)}
	}
	c := raw.Capabilities
	if len(c.Operations) > 5 {
		return nil, ErrProjection
	}
	seenOperations := map[native.Operation]bool{}
	for _, operation := range c.Operations {
		if seenOperations[operation] {
			return nil, ErrProjection
		}
		seenOperations[operation] = true
		value, err := mediaOperation(operation)
		if err != nil {
			return nil, err
		}
		out.Definition.Operations = append(out.Definition.Operations, value)
	}
	if b := c.VolumeBasisPoints; b != nil {
		if b.Minimum > b.Maximum || b.Maximum > 10000 || b.Step == 0 || b.Step > 10000 {
			return nil, ErrProjection
		}
		out.Definition.VolumeBasisPoints = &api.MediaVolumeBounds{Minimum: int32(b.Minimum), Maximum: int32(b.Maximum), Step: int32(b.Step)}
	}
	if b := c.PositionMillis; b != nil {
		if b.Minimum > b.Maximum || b.Step == 0 || b.Maximum > uint64(apitypes.MaxMediaPositionMillis) || b.Step > uint64(apitypes.MaxMediaPositionMillis) || raw.DurationMillis == nil || b.Maximum > *raw.DurationMillis {
			return nil, ErrProjection
		}
		minimum, err := mediaDuration(b.Minimum)
		if err != nil {
			return nil, err
		}
		maximum, err := mediaDuration(b.Maximum)
		if err != nil {
			return nil, err
		}
		step, err := mediaDuration(b.Step)
		if err != nil {
			return nil, err
		}
		out.Definition.Position = &api.MediaPositionBounds{Minimum: minimum, Maximum: maximum, Step: step}
	}
	out.Definition.Sources, err = mediaOptions(c.Sources)
	if err != nil {
		return nil, err
	}
	out.Definition.Groups, err = mediaOptions(c.Groups)
	if err != nil {
		return nil, err
	}
	return out, nil
}
