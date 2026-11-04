package server

import (
	"context"
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

// PlaybackCapability reads one exact registered source from the native cached
// owner. Root composition resolves public names/slots first and retains the
// surrounding caller permit and registry/configuration publication fence.
// Both public playback and private route come from the same capture; neither
// this method nor the projection performs provider/network I/O.
func (h *nativeMediaHandler) PlaybackCapability(ctx context.Context, home apitypes.HomeName, entity domain.EntityID, slot authorization.CapabilitySlot) (*api.MediaPlaybackCapability, *native.Route, error) {
	if h == nil || h.owner == nil || h.authority == nil || h.epoch == nil || !home.Valid() {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	schema, endpoint, err := slot.Values()
	if err != nil {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CAPABILITY_UNAVAILABLE)
	}
	var selected []nativeMediaSource
	for _, source := range h.sources {
		if source.schema == schema && source.endpoint == endpoint {
			selected = append(selected, source)
		}
	}
	if len(selected) != 1 {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CAPABILITY_UNAVAILABLE)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	expected, err := principal.Actor()
	actual, ok := ActorFromContext(ctx)
	if err != nil || !ok || expected != actual {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	stamp, err := h.owner.CurrentConfig(ctx)
	if err != nil {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	revision, _, err := stamp.Components()
	if err != nil {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	expectedEpoch, err := h.epoch.CurrentAdmissionEpoch()
	if err != nil || expectedEpoch == "" {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	var snapshot *native.Snapshot
	err = h.authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		var err error
		snapshot, err = projectNativeMediaCurrent(current, entity, selected, revision, h.epoch)
		return err
	})
	absent := errors.Is(err, errNativeMediaNotObserved)
	if err != nil && !absent {
		if errors.Is(err, authorization.ErrDenied) {
			return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
		}
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	current, err := h.owner.CurrentConfig(ctx)
	if err != nil || current != stamp {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	}
	currentEpoch, epochErr := h.epoch.CurrentAdmissionEpoch()
	if epochErr != nil || currentEpoch != expectedEpoch || !absent && (snapshot == nil || snapshot.Route == nil || snapshot.Route.AdmissionEpoch != expectedEpoch) {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	}
	if absent {
		// The exact source's MEDIA.READ and registry read were proved above. No
		// route/value/time/command is synthesized in the absence of an observation.
		return nativeMediaUnavailablePlayback(), nil, nil
	}
	if !selected[0].commands {
		snapshot.Capabilities = &native.Capabilities{}
	}
	playback, err := apiresources.ProjectMediaPlayback(home, snapshot, api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_OBSERVED)
	if err != nil {
		return nil, nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	return playback, proto.CloneOf(snapshot.Route), nil
}

// Only the real native factory calls this after proving exact read rights and
// configuration/epoch coherence. It never synthesizes an observation value.
func nativeMediaUnavailablePlayback() *api.MediaPlaybackCapability {
	return &api.MediaPlaybackCapability{ActionAccess: api.ActionAccess_ACTION_ACCESS_DENIED, DataProvenance: api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_OBSERVED, Observation: &api.MediaPlaybackObservation{Quality: &api.ObservationQuality{Availability: api.Availability_AVAILABILITY_UNAVAILABLE, Freshness: api.Freshness_FRESHNESS_STALE, Provenance: api.Provenance_PROVENANCE_OBSERVED}}}
}
