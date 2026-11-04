package server

import (
	"context"
	"errors"
	"testing"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sonos"
)

func TestNHA202NativeMediaAbsenceIsQualifiedOnlyAfterExactReadAuthority(t *testing.T) {
	for _, readable := range []bool{false, true} {
		authority, principal := nha195Authority(t, nha195Policy(t, "", readable, readable))
		entity, _ := domain.ParseEntityID("garage-player")
		probes := 0
		source := nativeMediaSource{schema: sonos.Schema, endpoint: sonos.Endpoint, snapshot: func(domain.EntityID) (*media.Snapshot, bool) { probes++; return nil, false }}
		err := authority.WithCurrent(context.Background(), principal, func(current authorization.Current) error {
			_, err := projectNativeMediaCurrent(current, entity, []nativeMediaSource{source}, 42, nha195Epoch{value: "native-epoch"})
			return err
		})
		if !readable {
			if !errors.Is(err, authorization.ErrDenied) || errors.Is(err, errNativeMediaNotObserved) || probes != 0 {
				t.Fatal("absence bypassed source rights", err)
			}
			continue
		}
		if !errors.Is(err, errNativeMediaNotObserved) || !errors.Is(err, sonos.ErrUnavailable) || probes != 1 {
			t.Fatal("qualified absence lost distinction", err)
		}
		value := nativeMediaUnavailablePlayback()
		if value.DataProvenance != api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_OBSERVED || value.ActionAccess != api.ActionAccess_ACTION_ACCESS_DENIED || value.Definition != nil || value.Observation == nil || value.Observation.Route != nil || value.Observation.PoweredOn != nil || value.Observation.VolumeBasisPoints != nil || value.Observation.Position != nil || value.Observation.ObservedTime != nil || value.Observation.Playback != api.MediaPlaybackState_MEDIA_PLAYBACK_STATE_UNSPECIFIED || value.Observation.Quality.Availability != api.Availability_AVAILABILITY_UNAVAILABLE {
			t.Fatal("absence invented actionable state")
		}
	}
}
