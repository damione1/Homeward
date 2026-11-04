package apitypes

import (
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxMediaPositionMillis int64 = 7 * 24 * 60 * 60 * 1000

// MediaRoute is the exact observed topology/configuration precondition. It
// proves representation only; the native owner rechecks it before dispatch.
type MediaRoute struct {
	entity, coordinator     EntityName
	endpoint, epoch         string
	topology, configuration int64
}

func mediaIdentifier(raw string) bool {
	if len(raw) < 1 || len(raw) > 256 || !utf8.ValidString(raw) {
		return false
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func NewMediaRoute(entity EntityName, endpoint string, coordinator EntityName, topology int64, epoch string, configuration int64) (MediaRoute, error) {
	if !entity.Valid() || !coordinator.Valid() || entity.Home() != coordinator.Home() || !mediaIdentifier(endpoint) || topology < 1 || configuration < 1 || len(epoch) != 32 {
		return MediaRoute{}, ErrIntent
	}
	decoded, err := hex.DecodeString(epoch)
	if err != nil || len(decoded) != 16 {
		return MediaRoute{}, ErrIntent
	}
	nonzero := false
	for _, b := range decoded {
		nonzero = nonzero || b != 0
	}
	if !nonzero {
		return MediaRoute{}, ErrIntent
	}
	return MediaRoute{entity, coordinator, strings.Clone(endpoint), strings.Clone(epoch), topology, configuration}, nil
}
func (v MediaRoute) Valid() bool {
	_, err := NewMediaRoute(v.entity, v.endpoint, v.coordinator, v.topology, v.epoch, v.configuration)
	return err == nil
}
func (v MediaRoute) Entity() EntityName      { return v.entity }
func (v MediaRoute) Endpoint() string        { return v.endpoint }
func (v MediaRoute) Coordinator() EntityName { return v.coordinator }
func (v MediaRoute) TopologyRevision() int64 { return v.topology }
func (v MediaRoute) AdmissionEpoch() string  { return v.epoch }
func (v MediaRoute) ConfigRevision() int64   { return v.configuration }

// MediaOperation mirrors the closed wire vocabulary through explicit checked
// conversion in contracts/apiv2. Public Go casts still require Valid admission.
type MediaOperation uint8

const (
	MediaOperationInvalid MediaOperation = iota
	MediaPlay
	MediaPause
	MediaStop
	MediaPrevious
	MediaNext
)

func (v MediaOperation) Valid() bool { return v >= MediaPlay && v <= MediaNext }

type MediaCommandKind uint8

const (
	MediaCommandInvalid MediaCommandKind = iota
	MediaTransportKind
	MediaVolumeKind
	MediaPositionKind
	MediaSourceKind
	MediaGroupKind
)

// MediaCommand is a sealed intention sum with privately constructed payloads.
type MediaCommand struct {
	route     MediaRoute
	kind      MediaCommandKind
	operation MediaOperation
	volume    int32
	position  int64
	option    string
}

func (MediaCommand) apiIntent() {}
func MediaTransport(route MediaRoute, operation MediaOperation) (MediaCommand, error) {
	if !route.Valid() || !operation.Valid() {
		return MediaCommand{}, ErrIntent
	}
	return MediaCommand{route: route, kind: MediaTransportKind, operation: operation}, nil
}
func MediaVolume(route MediaRoute, value int32) (MediaCommand, error) {
	if !route.Valid() || value < 0 || value > 10000 {
		return MediaCommand{}, ErrIntent
	}
	return MediaCommand{route: route, kind: MediaVolumeKind, volume: value}, nil
}
func MediaPosition(route MediaRoute, millis int64) (MediaCommand, error) {
	if !route.Valid() || millis < 0 || millis > MaxMediaPositionMillis {
		return MediaCommand{}, ErrIntent
	}
	return MediaCommand{route: route, kind: MediaPositionKind, position: millis}, nil
}
func MediaSource(route MediaRoute, id string) (MediaCommand, error) {
	if !route.Valid() || !mediaIdentifier(id) {
		return MediaCommand{}, ErrIntent
	}
	return MediaCommand{route: route, kind: MediaSourceKind, option: strings.Clone(id)}, nil
}
func MediaGroup(route MediaRoute, id string) (MediaCommand, error) {
	if !route.Valid() || !mediaIdentifier(id) {
		return MediaCommand{}, ErrIntent
	}
	return MediaCommand{route: route, kind: MediaGroupKind, option: strings.Clone(id)}, nil
}
func (v MediaCommand) Kind() MediaCommandKind { return v.kind }
func (v MediaCommand) Route() MediaRoute      { return v.route }
func (v MediaCommand) Operation() (MediaOperation, bool) {
	return v.operation, v.kind == MediaTransportKind && v.Valid()
}
func (v MediaCommand) VolumeBasisPoints() (int32, bool) {
	return v.volume, v.kind == MediaVolumeKind && v.Valid()
}
func (v MediaCommand) PositionMillis() (int64, bool) {
	return v.position, v.kind == MediaPositionKind && v.Valid()
}
func (v MediaCommand) OptionID() (string, bool) {
	return v.option, (v.kind == MediaSourceKind || v.kind == MediaGroupKind) && v.Valid()
}
func (v MediaCommand) Valid() bool {
	if !v.route.Valid() {
		return false
	}
	switch v.kind {
	case MediaTransportKind:
		return v.operation.Valid() && v.volume == 0 && v.position == 0 && v.option == ""
	case MediaVolumeKind:
		return v.operation == 0 && v.volume >= 0 && v.volume <= 10000 && v.position == 0 && v.option == ""
	case MediaPositionKind:
		return v.operation == 0 && v.volume == 0 && v.position >= 0 && v.position <= MaxMediaPositionMillis && v.option == ""
	case MediaSourceKind, MediaGroupKind:
		return v.operation == 0 && v.volume == 0 && v.position == 0 && mediaIdentifier(v.option)
	case MediaCommandInvalid:
		return false
	default:
		return false
	}
}

// ValidMediaIdentifier applies the exact open catalogue key grammar at the wire boundary.
func ValidMediaIdentifier(value string) bool { return mediaIdentifier(value) }
