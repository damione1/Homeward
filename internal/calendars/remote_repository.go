package calendars

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

type remoteProcess struct {
	source    [32]byte
	admission remoteFreshAdmission
	active    bool
	cancel    context.CancelFunc
	ctx       context.Context
	owner     *Service
}

type remoteFreshKind uint8

const (
	remoteFreshUnadmitted remoteFreshKind = iota
	remoteFreshKnown
	remoteFreshStale
)

type remoteFreshAdmission struct {
	kind            remoteFreshKind
	entity          CalendarID
	source, digest  [32]byte
	revision        int64
	at, until       time.Time
	mono, remaining time.Duration
}

func (a *remoteFreshAdmission) admits(id CalendarID, h storage.RemoteCalendarHeader, now time.Time, mono time.Duration) bool {
	if a.kind != remoteFreshKnown {
		return false
	}
	source, _ := h.SourceKey()
	if a.entity != id || a.source != source || a.revision != h.Revision() || a.digest != h.Digest() || now.Before(a.at) || !now.Before(a.until) || mono < a.mono || mono-a.mono >= a.remaining {
		a.kind = remoteFreshStale
		return false
	}
	return true
}
func (r *Repository) remoteSnapshot(id CalendarID, h storage.RemoteCalendarHeader) (CalendarSnapshot, error) {
	if !h.Valid() || h.Binding().Entity() != id.entity || id.origin != RemoteICS {
		return CalendarSnapshot{}, failure(Corrupt)
	}
	reason, err := reasonFromStorage(h.Reason())
	if err != nil {
		return CalendarSnapshot{}, err
	}
	state := RemoteState{reason: reason}
	state.source, _ = h.SourceKey()
	state.representation, _ = h.RepresentationKey()
	state.validated, _ = h.ValidatedAt()
	state.attempted, _ = h.AttemptedAt()
	state.until, _ = h.FreshUntil()
	switch h.State() {
	case storage.RemoteCalendarNeverValidated:
		state.kind = RemoteNeverValidated
	case storage.RemoteCalendarValidatedFresh, storage.RemoteCalendarValidatedStale:
		state.kind = RemoteValidated
		state.freshness = RemoteStale
		r.remoteMu.Lock()
		process := r.remoteProcesses[id]
		admitted := process != nil && process.active && process.source == state.source && h.State() == storage.RemoteCalendarValidatedFresh && process.admission.admits(id, h, r.clock.Now(), r.clock.MonoNow())
		r.remoteMu.Unlock()
		if admitted {
			process.owner.mu.Lock()
			admitted = !process.owner.closed
			process.owner.mu.Unlock()
		}
		if h.State() == storage.RemoteCalendarValidatedFresh && admitted {
			state.freshness = RemoteFresh
		}
	default:
		return CalendarSnapshot{}, failure(Corrupt)
	}
	snapshot := CalendarSnapshot{id: id, name: h.Name(), revision: Revision{calendar: id, presence: Present, number: h.Revision()}, updatedAt: h.UpdatedAt(), remote: state}
	if !snapshot.Valid() {
		return CalendarSnapshot{}, failure(Corrupt)
	}
	return snapshot, nil
}
func (r *Repository) readRemoteHead(ctx context.Context, tx pgx.Tx, id CalendarID) (storage.RemoteCalendarHeader, error) {
	b, err := storage.ReadRemoteCalendarBinding(ctx, tx, id.entity, false)
	if err != nil {
		return storage.RemoteCalendarHeader{}, remoteStorageFailure(err)
	}
	h, err := storage.ReadRemoteCalendarHeader(ctx, tx, b, false)
	return h, remoteStorageFailure(err)
}
func (r *Repository) ensureRemoteBound(ctx context.Context, tx pgx.Tx, id CalendarID) error {
	b, err := storage.ReadRemoteCalendarBinding(ctx, tx, id.entity, true)
	if err != nil {
		return remoteStorageFailure(err)
	}
	name, err := id.alias.Value()
	if err != nil {
		return failure(InvalidInput)
	}
	_, err = storage.EnsureRemoteCalendar(ctx, tx, b, name)
	return remoteStorageFailure(err)
}
func (r *Repository) readCalendar(ctx context.Context, tx pgx.Tx, id CalendarID, write bool) (CalendarSnapshot, error) {
	if id.origin != RemoteICS {
		return readCalendar(ctx, tx, id, write)
	}
	h, err := r.readRemoteHead(ctx, tx, id)
	if err != nil {
		return CalendarSnapshot{}, err
	}
	return r.remoteSnapshot(id, h)
}

func remoteReadOwner(ctx context.Context, id CalendarID, h storage.RemoteCalendarHeader) (*remoteImage, error) {
	if !h.Valid() || h.State() == storage.RemoteCalendarNeverValidated {
		return nil, failure(Corrupt)
	}
	version, profile := h.Version()
	if version != 1 || profile != 1 {
		return nil, failure(Incompatible)
	}
	image, err := newRemoteImage(ctx, id)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			image.close()
		}
	}()
	if err = image.lease.grow(h.ImageCharge() - imageFixedCharge); err != nil {
		return nil, err
	}
	image.lease.owner.mu.Lock()
	image.lease.prepaid = h.ImageCharge() - imageFixedCharge
	image.lease.owner.mu.Unlock()
	if err = image.parse.reserveData(uint64(3 * h.PayloadLength())); err != nil {
		return nil, err
	}
	image.sqlPreflight = true
	ok = true
	return image, nil
}
func (r *Repository) loadRemoteImage(ctx context.Context, tx pgx.Tx, id CalendarID, target *EventKey, image *calendarImage, operationContext context.Context) error {
	h, err := r.readRemoteHead(ctx, tx, id)
	if err != nil {
		return err
	}
	image.remoteHeader = h
	image.calendar, err = r.remoteSnapshot(id, h)
	if err != nil {
		return err
	}
	if target != nil {
		image.lookup = EventSnapshot{key: *target, revision: Revision{calendar: id, uid: target.uid, presence: Absent}, calendarRevision: image.calendar.revision}
	}
	if h.State() == storage.RemoteCalendarNeverValidated {
		return nil
	}
	owner, err := remoteReadOwner(operationContext, id, h)
	if err != nil {
		return err
	}
	image.remote = owner
	image.lease = owner.lease
	owner.observe = r.afterRemoteRead
	if r.beforeImageLoad != nil {
		r.beforeImageLoad()
	}
	data, err := storage.ReadRemoteCalendarPayload(ctx, tx, h)
	if err != nil {
		return remoteStorageFailure(err)
	}
	decoded, _, err := decodeRemoteImageOwned(owner.ctx, id, remotePayload{data: data, digest: h.Digest()}, owner)
	if err != nil {
		return err
	}
	counts, err := remoteImageCounts(decoded)
	if err != nil {
		return err
	}
	if counts != h.Counts() {
		return failure(Corrupt)
	}
	for i := range decoded.events {
		event := &decoded.events[i]
		event.revision = Revision{calendar: id, uid: event.key.uid, presence: Present, number: h.Revision()}
		event.calendarRevision = image.calendar.revision
		event.updatedAt = image.calendar.updatedAt
	}
	image.events = decoded.events
	return nil
}
func imageContext(ctx context.Context, image *calendarImage) context.Context {
	if image.remote != nil {
		return image.remote.ctx
	}
	return ctx
}
func imageBudget(ctx context.Context, image *calendarImage) *calculationBudget {
	if image.remote != nil {
		return zoneBudget(image.remote.ctx)
	}
	return newBudget(ctx)
}
