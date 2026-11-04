package apitypes

import "time"

// HomeMetadata is the registry-owned installation identity, independent of
// hostnames, credentials, discovery names and the physical household.
type HomeMetadata struct {
	name    HomeName
	created time.Time
}

func NewHomeMetadata(name HomeName, created time.Time) (HomeMetadata, error) {
	if !name.Valid() || created.IsZero() || created.Year() < 1 || created.Year() > 9999 {
		return HomeMetadata{}, ErrMetadata
	}
	return HomeMetadata{name: name, created: created.UTC()}, nil
}
func (h HomeMetadata) Name() HomeName        { return h.name }
func (h HomeMetadata) CreateTime() time.Time { return h.created }
