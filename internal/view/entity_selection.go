package view

import (
	"context"
	"crypto/sha256"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/entityfilter"
)

// entitySelection is admitted syntax plus an actual installation identity.
// Selection happens after native entity/slot authorization and materialization,
// before pagination. It cannot grant access or inspect hidden source slots.
type entitySelection struct {
	exposed   bool
	requested *apitypes.EntityViewName
	home      apitypes.HomeName
	filter    entityfilter.Filter
	digest    [32]byte
}

func (s entitySelection) hash(native [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("homeward.entity-selection/v2\x00"))
	h.Write(native[:])
	h.Write(s.digest[:])
	h.Write([]byte(s.home.String()))
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func (s entitySelection) matches(item Item) (bool, error) {
	if !item.hasProjection {
		return false, ErrInvalidInput
	}
	deviceID, _, err := item.record.IDs()
	if err != nil {
		return false, err
	}
	device, err := apitypes.DeviceNameForIdentity(s.home, deviceID)
	if err != nil {
		return false, err
	}
	candidate, err := entityfilter.NewCandidate(s.home, item.record.Metadata(), &device, item.projected)
	if err != nil {
		return false, err
	}
	return s.filter.Match(candidate), nil
}

// OpenEntityView applies a typed AIP-160 selection to one authorized immutable
// capture. The projector is mandatory: absence is never mistaken for an empty
// trait catalogue or an unknown availability observation.
func (s *Service) OpenEntityView(ctx context.Context, actor ActorID, home apitypes.HomeName, filter entityfilter.Filter, pageSize int) (Page, error) {
	if s == nil || s.projector == nil || !home.Valid() || !filter.Valid() {
		return Page{}, ErrInvalidInput
	}
	digest, err := filter.Digest()
	if err != nil {
		return Page{}, err
	}
	selection := entitySelection{home: home, filter: filter, digest: digest}
	return s.openView(ctx, actor, All(), pageSize, &selection)
}
