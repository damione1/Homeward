package calendars

import (
	"context"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

func (s *Service) CollectRemoteWindow(ctx context.Context, id CalendarID, q AutomationWindowRequest) (AutomationWindow, error) {
	return serviceCall(s, ctx, false, func(ctx context.Context) (AutomationWindow, error) { return s.repo.collectRemoteWindow(ctx, id, q) })
}

// A scalar fence never decodes another image or resets the owner's credit.
func (r *Repository) remoteWindowFence(ctx context.Context, id CalendarID, expected storage.RemoteCalendarHeader) (CalendarSnapshot, error) {
	var head storage.RemoteCalendarHeader
	err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := r.identity(ctx, tx, id.entity, &id, false); e != nil {
			return e
		}
		var e error
		head, e = r.readRemoteHead(ctx, tx, id)
		return e
	})
	if err != nil {
		return CalendarSnapshot{}, err
	}
	a, _ := head.SourceKey()
	b, _ := expected.SourceKey()
	x, _ := head.RepresentationKey()
	y, _ := expected.RepresentationKey()
	if head.Revision() != expected.Revision() || head.Digest() != expected.Digest() || a != b || x != y {
		return CalendarSnapshot{}, failure(StaleCursor)
	}
	return r.remoteSnapshot(id, head)
}
func (r *Repository) collectRemoteWindow(ctx context.Context, id CalendarID, q AutomationWindowRequest) (AutomationWindow, error) {
	if !id.Valid() || id.origin != RemoteICS || !q.valid() {
		return AutomationWindow{}, failure(InvalidInput)
	}
	if e := contextValid(ctx); e != nil {
		return AutomationWindow{}, e
	}
	image, e := r.loadImage(ctx, id, nil)
	if e != nil {
		return AutomationWindow{}, e
	}
	defer image.close()
	ctx = imageContext(ctx, image)
	out := AutomationWindow{calendar: id, revision: image.calendar.revision, remote: image.calendar.remote, digest: image.remoteHeader.Digest(), from: q.from.value, to: q.to.value}
	if out.remote.kind == RemoteNeverValidated {
		if !out.Valid() {
			return AutomationWindow{}, failure(Corrupt)
		}
		return out, nil
	}
	view, e := newRemoteResultCopies(image).zone(q.zone)
	if e != nil {
		return AutomationWindow{}, e
	}
	q.zone = view
	window, e := q.window(ctx)
	if e != nil {
		return AutomationWindow{}, e
	}
	budget := imageBudget(ctx, image)
	ctx = withCalendarBudget(ctx, budget)
	if r.beforeExpansion != nil {
		if e = r.beforeExpansion(ctx); e != nil {
			return AutomationWindow{}, e
		}
	}
	if e = validateImage(ctx, image, budget); e != nil {
		return AutomationWindow{}, e
	}
	borrowed, e := prepareImageOccurrences(ctx, image, window, MaxAutomationOccurrences, Cursor{}, budget)
	if e != nil {
		return AutomationWindow{}, e
	}
	if len(borrowed) > MaxAutomationOccurrences {
		return AutomationWindow{}, failure(CapacityExceeded)
	}
	charge := uint64(len(borrowed)) * automationOccurrenceCharge
	if e = image.remote.parse.reserveData(charge); e != nil {
		return AutomationWindow{}, e
	}
	if e = image.lease.grow(int64(charge)); e != nil {
		return AutomationWindow{}, e
	}
	out.items = make([]AutomationOccurrence, 0, len(borrowed))
	for offset, page := 0, 0; offset < len(borrowed); offset, page = offset+automationPageSize, page+1 {
		if r.beforeRemoteWindowPage != nil {
			if e = r.beforeRemoteWindowPage(page); e != nil {
				return AutomationWindow{}, e
			}
		}
		if _, e = r.remoteWindowFence(ctx, id, image.remoteHeader); e != nil {
			return AutomationWindow{}, e
		}
		end := min(offset+automationPageSize, len(borrowed))
		for _, o := range borrowed[offset:end] {
			projected, e := projectAutomationOccurrence(ctx, o, q.zone, out.remote.freshness)
			if e != nil {
				return AutomationWindow{}, e
			}
			out.items = append(out.items, projected)
		}
	}
	current, e := r.remoteWindowFence(ctx, id, image.remoteHeader)
	if e != nil {
		return AutomationWindow{}, e
	}
	// Expiry may win while resolving dates. Preserve the version, downgrade all items.
	out.remote = current.remote
	for i := range out.items {
		out.items[i].freshness = out.remote.freshness
	}
	if !out.Valid() {
		return AutomationWindow{}, failure(Corrupt)
	}
	return out, nil
}

// CheckRemoteWindow checks only durable scalars after the image owner closed.
// This transaction is not atomic with a later040 publication.
func (s *Service) CheckRemoteWindow(ctx context.Context, w AutomationWindow) error {
	_, err := serviceCall(s, ctx, false, func(ctx context.Context) (struct{}, error) {
		if !w.Valid() {
			return struct{}{}, failure(InvalidInput)
		}
		var head storage.RemoteCalendarHeader
		e := s.repo.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, e := s.repo.identity(ctx, tx, w.calendar.entity, &w.calendar, false); e != nil {
				return e
			}
			var e error
			head, e = s.repo.readRemoteHead(ctx, tx, w.calendar)
			return e
		})
		if e != nil {
			return struct{}{}, e
		}
		snap, e := s.repo.remoteSnapshot(w.calendar, head)
		if e != nil {
			return struct{}{}, e
		}
		source, _ := head.SourceKey()
		representation, _ := head.RepresentationKey()
		expectedSource, _ := w.remote.SourceKey()
		expectedRepresentation, _ := w.remote.RepresentationKey()
		if snap.revision != w.revision || source != expectedSource || representation != expectedRepresentation || head.Digest() != w.digest || snap.remote != w.remote {
			return struct{}{}, failure(StaleCursor)
		}
		return struct{}{}, nil
	})
	return err
}
