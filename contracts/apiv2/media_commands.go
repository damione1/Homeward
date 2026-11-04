package apiv2

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
)

func ParseMediaOperation(value wire.MediaOperation) (apitypes.MediaOperation, error) {
	switch value {
	case wire.MediaOperation_MEDIA_OPERATION_PLAY:
		return apitypes.MediaPlay, nil
	case wire.MediaOperation_MEDIA_OPERATION_PAUSE:
		return apitypes.MediaPause, nil
	case wire.MediaOperation_MEDIA_OPERATION_STOP:
		return apitypes.MediaStop, nil
	case wire.MediaOperation_MEDIA_OPERATION_PREVIOUS:
		return apitypes.MediaPrevious, nil
	case wire.MediaOperation_MEDIA_OPERATION_NEXT:
		return apitypes.MediaNext, nil
	case wire.MediaOperation_MEDIA_OPERATION_UNSPECIFIED:
		return 0, reject("media.operation", MissingIntent)
	default:
		return 0, reject("media.operation", UnknownValue)
	}
}

// ParseMediaCommand is pure, exhaustive and called only after object admission.
func ParseMediaCommand(raw *wire.MediaCommand) (apitypes.MediaCommand, error) {
	if raw == nil {
		return apitypes.MediaCommand{}, reject("media", MissingIntent)
	}
	if !knownFields(raw.ProtoReflect()) {
		return apitypes.MediaCommand{}, reject("media", UnknownField)
	}
	if raw.Route == nil {
		return apitypes.MediaCommand{}, reject("media.route", MissingIntent)
	}
	entity, err := apitypes.ParseEntityName(raw.Route.Entity)
	if err != nil {
		return apitypes.MediaCommand{}, reject("media.route.entity", InvalidName)
	}
	coordinator, err := apitypes.ParseEntityName(raw.Route.Coordinator)
	if err != nil {
		return apitypes.MediaCommand{}, reject("media.route.coordinator", InvalidName)
	}
	if !apitypes.ValidMediaIdentifier(raw.Route.Endpoint) {
		return apitypes.MediaCommand{}, reject("media.route.endpoint", OutOfRange)
	}
	route, err := apitypes.NewMediaRoute(entity, raw.Route.Endpoint, coordinator, raw.Route.TopologyRevision, raw.Route.AdmissionEpoch, raw.Route.ConfigRevision)
	if err != nil {
		return apitypes.MediaCommand{}, reject("media.route", OutOfRange)
	}
	switch command := raw.Command.(type) {
	case *wire.MediaCommand_Operation:
		if command == nil {
			return apitypes.MediaCommand{}, reject("media.operation", MissingIntent)
		}
		operation, err := ParseMediaOperation(command.Operation)
		if err != nil {
			return apitypes.MediaCommand{}, err
		}
		return apitypes.MediaTransport(route, operation)
	case *wire.MediaCommand_VolumeBasisPoints:
		if command == nil {
			return apitypes.MediaCommand{}, reject("media.volume_basis_points", MissingIntent)
		}
		value, err := apitypes.MediaVolume(route, command.VolumeBasisPoints)
		if err != nil {
			return apitypes.MediaCommand{}, reject("media.volume_basis_points", OutOfRange)
		}
		return value, nil
	case *wire.MediaCommand_Position:
		if command == nil || command.Position == nil {
			return apitypes.MediaCommand{}, reject("media.position", MissingIntent)
		}
		duration := command.Position
		if duration.CheckValid() != nil || duration.Seconds < 0 || duration.Seconds > apitypes.MaxMediaPositionMillis/1000 || duration.Nanos < 0 || duration.Nanos%1000000 != 0 {
			return apitypes.MediaCommand{}, reject("media.position", OutOfRange)
		}
		value, err := apitypes.MediaPosition(route, duration.Seconds*1000+int64(duration.Nanos)/1000000)
		if err != nil {
			return apitypes.MediaCommand{}, reject("media.position", OutOfRange)
		}
		return value, nil
	case *wire.MediaCommand_SourceId:
		if command == nil {
			return apitypes.MediaCommand{}, reject("media.source_id", MissingIntent)
		}
		value, err := apitypes.MediaSource(route, command.SourceId)
		if err != nil {
			return apitypes.MediaCommand{}, reject("media.source_id", OutOfRange)
		}
		return value, nil
	case *wire.MediaCommand_GroupId:
		if command == nil {
			return apitypes.MediaCommand{}, reject("media.group_id", MissingIntent)
		}
		value, err := apitypes.MediaGroup(route, command.GroupId)
		if err != nil {
			return apitypes.MediaCommand{}, reject("media.group_id", OutOfRange)
		}
		return value, nil
	default:
		return apitypes.MediaCommand{}, reject("media", MissingIntent)
	}
}
