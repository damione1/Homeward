package apiresources

import (
	"errors"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var ErrMediaTopologyChanged = errors.New("media route precondition changed")

func mediaPublicRoute(route apitypes.MediaRoute) *api.MediaRoute {
	return &api.MediaRoute{Entity: route.Entity().String(), Endpoint: route.Endpoint(), Coordinator: route.Coordinator().String(), TopologyRevision: route.TopologyRevision(), AdmissionEpoch: route.AdmissionEpoch(), ConfigRevision: route.ConfigRevision()}
}
func mediaNativeOperation(operation apitypes.MediaOperation) (native.Operation, error) {
	switch operation {
	case apitypes.MediaPlay:
		return native.Operation_OPERATION_PLAY, nil
	case apitypes.MediaPause:
		return native.Operation_OPERATION_PAUSE, nil
	case apitypes.MediaStop:
		return native.Operation_OPERATION_STOP, nil
	case apitypes.MediaPrevious:
		return native.Operation_OPERATION_PREVIOUS, nil
	case apitypes.MediaNext:
		return native.Operation_OPERATION_NEXT, nil
	case apitypes.MediaOperationInvalid:
		return 0, ErrUnsupportedTrait
	default:
		return 0, ErrUnsupportedTrait
	}
}
func mediaMillis(value *durationpb.Duration) (int64, bool) {
	if value == nil || value.CheckValid() != nil || value.Seconds < 0 || value.Seconds > apitypes.MaxMediaPositionMillis/1000 || value.Nanos < 0 || value.Nanos%1000000 != 0 {
		return 0, false
	}
	millis := value.Seconds*1000 + int64(value.Nanos)/1000000
	return millis, millis <= apitypes.MaxMediaPositionMillis
}

// mediaNativeArguments consumes only the current, authorized compiled source
// binding and its retained private route. Public names never reverse-map native
// IDs. No I/O, ledger reservation, coordinator selection or retry occurs here.
func mediaNativeArguments(c Capability, intent apitypes.MediaCommand) (*actions.ActionArgs, error) {
	if c.trait != Media || !intent.Valid() || c.value == nil || c.nativeMediaRoute == nil || c.actionMessage != "home.media.v1.MediaAction" {
		return nil, ErrUnsupportedTrait
	}
	schema, endpoint, err := c.source.Values()
	if err != nil || schema != "home.media.v1" || c.nativeMediaRoute.Endpoint != endpoint {
		return nil, ErrUnsupportedTrait
	}
	playback := c.value.GetMediaPlayback()
	if playback == nil || playback.Definition == nil || playback.Observation == nil {
		return nil, ErrProjection
	}
	if playback.ActionAccess != api.ActionAccess_ACTION_ACCESS_ALLOWED {
		return nil, authorization.ErrDenied
	}
	if !proto.Equal(mediaPublicRoute(intent.Route()), playback.Observation.Route) {
		return nil, ErrMediaTopologyChanged
	}
	action := &native.MediaAction{Route: proto.CloneOf(c.nativeMediaRoute)}
	definition := playback.Definition
	switch intent.Kind() {
	case apitypes.MediaTransportKind:
		operation, ok := intent.Operation()
		if !ok {
			return nil, ErrUnsupportedTrait
		}
		converted, err := mediaNativeOperation(operation)
		if err != nil {
			return nil, err
		}
		public, err := mediaOperation(converted)
		if err != nil {
			return nil, err
		}
		supported := false
		for _, allowed := range definition.Operations {
			supported = supported || allowed == public
		}
		if !supported {
			return nil, ErrUnsupportedTrait
		}
		action.Command = &native.MediaAction_Operation{Operation: converted}
	case apitypes.MediaVolumeKind:
		volume, ok := intent.VolumeBasisPoints()
		bounds := definition.VolumeBasisPoints
		if !ok || bounds == nil || bounds.Minimum < 0 || bounds.Maximum > 10000 || bounds.Step < 1 || volume < bounds.Minimum || volume > bounds.Maximum || (volume-bounds.Minimum)%bounds.Step != 0 {
			return nil, ErrUnsupportedTrait
		}
		action.Command = &native.MediaAction_VolumeBasisPoints{VolumeBasisPoints: uint32(volume)}
	case apitypes.MediaPositionKind:
		position, ok := intent.PositionMillis()
		bounds := definition.Position
		if !ok || bounds == nil {
			return nil, ErrUnsupportedTrait
		}
		minimum, a := mediaMillis(bounds.Minimum)
		maximum, b := mediaMillis(bounds.Maximum)
		step, d := mediaMillis(bounds.Step)
		if !a || !b || !d || step < 1 || position < minimum || position > maximum || (position-minimum)%step != 0 {
			return nil, ErrUnsupportedTrait
		}
		action.Command = &native.MediaAction_PositionMillis{PositionMillis: uint64(position)}
	case apitypes.MediaSourceKind, apitypes.MediaGroupKind:
		option, ok := intent.OptionID()
		if !ok {
			return nil, ErrUnsupportedTrait
		}
		options := definition.Sources
		if intent.Kind() == apitypes.MediaGroupKind {
			options = definition.Groups
		}
		supported := false
		for _, allowed := range options {
			supported = supported || allowed != nil && allowed.Id == option
		}
		if !supported {
			return nil, ErrUnsupportedTrait
		}
		if intent.Kind() == apitypes.MediaGroupKind {
			action.Command = &native.MediaAction_GroupId{GroupId: option}
		} else {
			action.Command = &native.MediaAction_SourceId{SourceId: option}
		}
	case apitypes.MediaCommandInvalid:
		return nil, ErrUnsupportedTrait
	default:
		return nil, ErrUnsupportedTrait
	}
	fingerprint, err := compiledMediaFingerprint()
	if err != nil {
		return nil, err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(action)
	if err != nil || len(payload) > 64<<10 {
		return nil, ErrProjection
	}
	return &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: schema, Major: 1, DescriptorSha256: append([]byte(nil), fingerprint...), ProtobufPayload: payload}}}, nil
}
