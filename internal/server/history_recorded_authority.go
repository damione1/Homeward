package server

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/recorderevidence"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
)

// No policy copy or result cache: all recorded slots are evaluated under the
// native authority at read time. The request's permit holds that same policy
// binding through SQL and response publication.
type historyRecordedAuthority struct {
	handler  *historyHandler
	query    historyquery.Query
	admitted historyAdmission
}

func (a historyRecordedAuthority) AuthorizeRecorderEvidence(ctx context.Context, e recorderevidence.Evidence) (storage.RecorderVisibility, error) {
	if !e.Present() || e.Entity() != a.query.Entity() {
		return storage.RecorderHidden, nil
	}
	visible := storage.RecorderVisible
	err := a.handler.runtime.authority.WithCurrent(ctx, a.admitted.principal, func(current authorization.Current) error {
		for _, slot := range e.Slots() {
			object, err := authorization.EntityObject(e.Entity(), slot)
			if err != nil {
				return err
			}
			if err = current.Authorize(a.admitted.permission, object); err != nil {
				if errors.Is(err, authorization.ErrDenied) {
					visible = storage.RecorderHidden
					return nil
				}
				return err
			}
		}
		return nil
	})
	return visible, err
}
