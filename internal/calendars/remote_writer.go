package calendars

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

// RemoteSource is admitted configuration, containing opaque authenticated
// credential revisions, never an URL or credential value.
type RemoteSource struct {
	scope storage.RemoteCalendarScope
	name  string
}

func NewRemoteSource(scope storage.RemoteCalendarScope, name string) (RemoteSource, error) {
	if !scope.Valid() || !textValid(name, 512, true) || strings.ContainsAny(name, "\r\n") {
		return RemoteSource{}, failure(InvalidInput)
	}
	return RemoteSource{scope: scope, name: strings.Clone(name)}, nil
}
func (s RemoteSource) Valid() bool {
	return s.scope.Valid() && textValid(s.name, 512, true) && !strings.ContainsAny(s.name, "\r\n")
}
func (s RemoteSource) Scope() storage.RemoteCalendarScope { return s.scope }
func (s RemoteSource) String() string                     { return "calendars: remote source [redacted]" }
func (s RemoteSource) GoString() string                   { return s.String() }
func (s RemoteSource) MarshalJSON() ([]byte, error)       { return []byte(`"[redacted]"`), nil }

// Trusted composition receives this restricted wrapper. Its concrete dynamic
// value exposes neither *Service nor Store/CRUD nor any network/action client.
type RemoteWriter interface {
	Read(context.Context) (RemoteCacheSnapshot, error)
	Replace(context.Context, Revision, storage.RemoteCalendarObservation, []byte, *Zone) (storage.RemoteCalendarCommit, error)
	Revalidate(context.Context, Revision, storage.RemoteCalendarObservation, storage.RemoteCalendarValidators, [32]byte) (storage.RemoteCalendarCommit, error)
	Degrade(context.Context, Revision, time.Time, RemoteReason) error
	Reject(context.Context, Revision, time.Time, RemoteReason, storage.RemoteCalendarConditionalDisposition) error
	Retire()
}
type remoteWriter struct {
	service *Service
	id      CalendarID
	source  RemoteSource
	binding storage.RemoteCalendarBinding
	process *remoteProcess
}
type RemoteCacheSnapshot struct {
	calendar   CalendarSnapshot
	validators storage.RemoteCalendarValidators
	digest     [32]byte
	hasPayload bool
	selection  storage.RemoteCalendarSelection
}

func (s RemoteCacheSnapshot) Selection() storage.RemoteCalendarSelection   { return s.selection }
func (s RemoteCacheSnapshot) Calendar() CalendarSnapshot                   { return s.calendar }
func (s RemoteCacheSnapshot) Validators() storage.RemoteCalendarValidators { return s.validators }
func (s RemoteCacheSnapshot) Digest() ([32]byte, bool) {
	return s.digest, s.hasPayload && s.calendar.Valid()
}
func (s RemoteCacheSnapshot) String() string               { return "calendars: remote cache snapshot [redacted]" }
func (s RemoteCacheSnapshot) GoString() string             { return s.String() }
func (s RemoteCacheSnapshot) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (w *remoteWriter) valid() bool {
	return w != nil && w.service != nil && w.service.repo != nil && w.id.Valid() && w.id.origin == RemoteICS && w.source.Valid() && w.binding.Valid() && w.process != nil
}
func (w *remoteWriter) current() bool {
	if !w.valid() {
		return false
	}
	r := w.service.repo
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	return r.remoteProcesses[w.id] == w.process && w.process.active
}
func (w *remoteWriter) Retire() {
	if !w.valid() {
		return
	}
	r := w.service.repo
	r.remoteMu.Lock()
	w.process.admission.kind = remoteFreshStale
	w.process.active = false
	w.process.cancel()
	r.remoteMu.Unlock()
}
func (w *remoteWriter) callContext(ctx context.Context) (context.Context, func(), error) {
	if !w.current() {
		return nil, nil, failure(Closed)
	}
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.process.ctx, cancel)
	return bounded, func() { stop(); cancel() }, nil
}
func (w *remoteWriter) markStale() bool {
	r := w.service.repo
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	if r.remoteProcesses[w.id] != w.process || !w.process.active {
		return false
	}
	w.process.admission.kind = remoteFreshStale
	return true
}
func (s *Service) ConfigureRemoteSource(ctx context.Context, id CalendarID, source RemoteSource) (RemoteWriter, error) {
	return serviceCall(s, ctx, true, func(ctx context.Context) (RemoteWriter, error) {
		if !id.Valid() || id.origin != RemoteICS || !source.Valid() {
			return nil, failure(InvalidInput)
		}
		r := s.repo
		var binding storage.RemoteCalendarBinding
		var beforeRevision int64
		err := r.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := r.identity(ctx, tx, id.entity, &id, false); err != nil {
				return err
			}
			var err error
			binding, err = storage.ReadRemoteCalendarBinding(ctx, tx, id.entity, false)
			if err != nil {
				return remoteStorageFailure(err)
			}
			h, e := storage.ReadRemoteCalendarHeader(ctx, tx, binding, false)
			if e != nil {
				return remoteStorageFailure(e)
			}
			beforeRevision = h.Revision()
			return nil
		})
		if err != nil {
			return nil, err
		}
		// Close the old process freshness before attempting SQL. A failed or
		// indeterminate reconfiguration cannot rearm the former process.
		r.remoteMu.Lock()
		if old := r.remoteProcesses[id]; old != nil {
			old.admission.kind = remoteFreshStale
			old.active = false
			old.cancel()
		}
		r.remoteMu.Unlock()
		header, err := r.store.ConfigureRemoteCalendar(ctx, binding, source.scope, source.name)
		if err != nil {
			return nil, remoteStorageFailure(err)
		}
		if err = contextValid(ctx); err != nil {
			return nil, err
		}
		processCtx, cancel := context.WithCancel(context.Background())
		key, _ := source.scope.Key()
		process := &remoteProcess{source: key, active: true, cancel: cancel, ctx: processCtx, owner: s}
		r.remoteMu.Lock()
		if r.remoteProcesses == nil {
			r.remoteProcesses = make(map[CalendarID]*remoteProcess)
		}
		r.remoteProcesses[id] = process
		r.remoteMu.Unlock()
		snapshot, err := r.remoteSnapshot(id, header)
		if err != nil {
			cancel()
			return nil, err
		}
		if header.Revision() != beforeRevision {
			s.publish(Change{kind: CalendarUpdated, calendar: id, revision: snapshot.revision})
		}
		return &remoteWriter{service: s, id: id, source: source, binding: binding, process: process}, nil
	})
}
func (w *remoteWriter) Read(ctx context.Context) (RemoteCacheSnapshot, error) {
	if !w.valid() {
		return RemoteCacheSnapshot{}, failure(InvalidInput)
	}
	return serviceCall(w.service, ctx, false, func(ctx context.Context) (RemoteCacheSnapshot, error) {
		bounded, done, err := w.callContext(ctx)
		if err != nil {
			return RemoteCacheSnapshot{}, err
		}
		defer done()
		image, err := w.service.repo.loadImage(bounded, w.id, nil)
		if err != nil {
			return RemoteCacheSnapshot{}, err
		}
		defer image.close()
		if err = validateImage(imageContext(bounded, image), image, imageBudget(bounded, image)); err != nil {
			return RemoteCacheSnapshot{}, err
		}
		if !w.current() {
			return RemoteCacheSnapshot{}, failure(Closed)
		}
		h := image.remoteHeader
		source, _ := h.SourceKey()
		key, _ := w.source.scope.Key()
		if source != key {
			return RemoteCacheSnapshot{}, failure(Conflict)
		}
		return RemoteCacheSnapshot{calendar: image.calendar, validators: h.Validators(), digest: h.Digest(), hasPayload: h.State() != storage.RemoteCalendarNeverValidated, selection: h.Selection()}, nil
	})
}
func (w *remoteWriter) expected(ctx context.Context, expected Revision) (storage.RemoteCalendarExpected, error) {
	if !expected.validFor(w.id, EventUID{}) || expected.presence != Present {
		return storage.RemoteCalendarExpected{}, failure(InvalidInput)
	}
	var value storage.RemoteCalendarExpected
	err := w.service.repo.transaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.service.repo.identity(ctx, tx, w.id.entity, &w.id, false); err != nil {
			return err
		}
		h, err := storage.ReadRemoteCalendarHeader(ctx, tx, w.binding, false)
		if err != nil {
			return remoteStorageFailure(err)
		}
		if h.Revision() != expected.number {
			return failure(Conflict)
		}
		value, err = storage.ExpectRemoteCalendar(h, w.source.scope)
		return remoteStorageFailure(err)
	})
	return value, err
}
func (w *remoteWriter) observation(o storage.RemoteCalendarObservation) bool {
	a, ok := o.Source().Key()
	b, _ := w.source.scope.Key()
	return o.Valid() && ok && a == b
}
func (w *remoteWriter) admitKnown(commit storage.RemoteCalendarCommit, h storage.RemoteCalendarHeader, start remoteObservationClock) bool {
	if !commit.Valid() || !h.Valid() {
		return false
	}
	entity, ok := commit.Entity()
	rev, rok := commit.Revision()
	digest, dok := commit.Digest()
	source, _ := h.SourceKey()
	key, _ := w.source.scope.Key()
	if !ok || !rok || !dok || entity != w.id.entity || rev != h.Revision() || digest != h.Digest() || source != key || start.source != source || !commit.Matches(entity, source, start.observation) {
		return false
	}
	r := w.service.repo
	r.remoteMu.Lock()
	defer r.remoteMu.Unlock()
	if r.remoteProcesses[w.id] != w.process || !w.process.active {
		return false
	}
	now, mono := r.clock.Now().UTC(), r.clock.MonoNow()
	until, uok := h.FreshUntil()
	at, aok := h.ValidatedAt()
	if !start.until.IsZero() && start.until.Before(until) {
		until = start.until
	}
	remaining := until.Sub(start.at)
	if !uok || !aok || start.at.Before(at) || now.Before(start.at) || !now.Before(until) || remaining <= 0 || mono < start.mono || mono-start.mono >= remaining {
		w.process.admission = remoteFreshAdmission{kind: remoteFreshStale}
		return true
	}
	if lifetime := until.Sub(at); remaining > lifetime {
		remaining = lifetime
	}
	if remaining > 24*time.Hour {
		remaining = 24 * time.Hour
	}
	w.process.admission = remoteFreshAdmission{kind: remoteFreshKnown, entity: w.id, source: source, digest: digest, revision: rev, at: start.at, until: until, mono: start.mono, remaining: remaining}
	return true
}
func (w *remoteWriter) finish(ctx context.Context, commit storage.RemoteCalendarCommit, h storage.RemoteCalendarHeader, start remoteObservationClock) (storage.RemoteCalendarCommit, error) {
	if err := contextValid(ctx); err != nil {
		w.markStale()
		return storage.RemoteCalendarCommit{}, err
	}
	if w.service.repo.beforeRemoteFreshAdmission != nil {
		w.service.repo.beforeRemoteFreshAdmission()
	}
	if !w.admitKnown(commit, h, start) {
		return storage.RemoteCalendarCommit{}, failure(Closed)
	}
	snapshot, err := w.service.repo.remoteSnapshot(w.id, h)
	if err != nil {
		w.markStale()
		return storage.RemoteCalendarCommit{}, err
	}
	w.service.publish(Change{kind: CalendarUpdated, calendar: w.id, revision: snapshot.revision})
	return commit, nil
}
func (w *remoteWriter) Replace(ctx context.Context, expected Revision, o storage.RemoteCalendarObservation, body []byte, projection *Zone) (storage.RemoteCalendarCommit, error) {
	if !w.valid() || !w.observation(o) {
		return storage.RemoteCalendarCommit{}, failure(InvalidInput)
	}
	start, err := remoteWriterObservationClock(ctx, o, w.service.repo.clock)
	if err != nil {
		return storage.RemoteCalendarCommit{}, err
	}
	return serviceCall(w.service, ctx, true, func(ctx context.Context) (storage.RemoteCalendarCommit, error) {
		bounded, done, err := w.callContext(ctx)
		if err != nil {
			return storage.RemoteCalendarCommit{}, err
		}
		defer done()
		// Reuse the ICS wire bound before hashing untrusted bytes. The HTTP
		// admission binds exact bytes without an unbounded pre-parser scan.
		if len(body) > maxBody {
			return storage.RemoteCalendarCommit{}, guardFailure{BodyTooLarge}
		}
		if !o.AdmitsRepresentation(sha256.Sum256(body)) {
			return storage.RemoteCalendarCommit{}, failure(InvalidInput)
		}
		cas, err := w.expected(bounded, expected)
		if err != nil {
			return storage.RemoteCalendarCommit{}, err
		}
		image, err := admitRemoteICS(bounded, w.id, body, projection)
		if err != nil {
			w.markStale()
			return storage.RemoteCalendarCommit{}, err
		}
		defer image.close()
		image.observe = w.service.repo.afterRemoteMutation
		payload, err := encodeRemoteImage(image.ctx, image)
		if err != nil {
			w.markStale()
			return storage.RemoteCalendarCommit{}, err
		}
		counts, err := remoteImageCounts(image)
		if err != nil {
			return storage.RemoteCalendarCommit{}, err
		}
		// Header preflight includes deterministic decode collections. Retain
		// any additional credit before handing the payload to pgx.
		minimum := imageFixedCharge + 3*int64(len(payload.data)) + counts.Zones*256 + counts.Events*(512+eventBaseCharge) + counts.Points*pointCharge + counts.Exceptions*exceptionCharge
		credit := image.lease.creditObservation().ReservedBytes
		if int64(credit) < minimum {
			if err = image.lease.grow(minimum - int64(credit)); err != nil {
				return storage.RemoteCalendarCommit{}, err
			}
		}
		stored, err := storage.BorrowRemoteCalendarPayload(payload.data, payload.digest, counts, int64(image.lease.creditObservation().ReservedBytes))
		if err != nil {
			return storage.RemoteCalendarCommit{}, remoteStorageFailure(err)
		}
		if !w.current() {
			return storage.RemoteCalendarCommit{}, failure(Closed)
		}
		commit, header, err := w.service.repo.store.ReplaceRemoteCalendar(bounded, cas, o, stored)
		if err != nil {
			w.markStale()
			return storage.RemoteCalendarCommit{}, remoteStorageFailure(err)
		}
		return w.finish(bounded, commit, header, start)
	})
}
func (w *remoteWriter) Revalidate(ctx context.Context, expected Revision, o storage.RemoteCalendarObservation, sent storage.RemoteCalendarValidators, digest [32]byte) (storage.RemoteCalendarCommit, error) {
	if !w.valid() || !w.observation(o) {
		return storage.RemoteCalendarCommit{}, failure(InvalidInput)
	}
	start, err := remoteWriterObservationClock(ctx, o, w.service.repo.clock)
	if err != nil {
		return storage.RemoteCalendarCommit{}, err
	}
	return serviceCall(w.service, ctx, true, func(ctx context.Context) (storage.RemoteCalendarCommit, error) {
		bounded, done, err := w.callContext(ctx)
		if err != nil {
			return storage.RemoteCalendarCommit{}, err
		}
		defer done()
		cas, err := w.expected(bounded, expected)
		if err != nil {
			return storage.RemoteCalendarCommit{}, err
		}
		commit, header, err := w.service.repo.store.RevalidateRemoteCalendar(bounded, cas, o, sent, digest)
		if err != nil {
			w.markStale()
			return storage.RemoteCalendarCommit{}, remoteStorageFailure(err)
		}
		return w.finish(bounded, commit, header, start)
	})
}
func (w *remoteWriter) Degrade(ctx context.Context, expected Revision, at time.Time, reason RemoteReason) error {
	return w.Reject(ctx, expected, at, reason, storage.PreserveAdmittedValidators)
}
func (w *remoteWriter) Reject(ctx context.Context, expected Revision, at time.Time, reason RemoteReason, disposition storage.RemoteCalendarConditionalDisposition) error {
	if !w.valid() || !disposition.Valid() {
		return failure(InvalidInput)
	}
	sqlReason, err := reasonToStorage(reason)
	if err != nil || reason == RemoteReasonNone {
		return failure(InvalidInput)
	}
	w.markStale()
	_, err = serviceCall(w.service, ctx, true, func(ctx context.Context) (struct{}, error) {
		bounded, done, err := w.callContext(ctx)
		if err != nil {
			return struct{}{}, err
		}
		defer done()
		cas, err := w.expected(bounded, expected)
		if err != nil {
			return struct{}{}, err
		}
		h, err := w.service.repo.store.RejectRemoteCalendar(bounded, cas, w.source.scope, at, sqlReason, disposition)
		if err != nil {
			return struct{}{}, remoteStorageFailure(err)
		}
		if !w.current() {
			return struct{}{}, failure(Closed)
		}
		snapshot, err := w.service.repo.remoteSnapshot(w.id, h)
		if err != nil {
			return struct{}{}, err
		}
		w.service.publish(Change{kind: CalendarUpdated, calendar: w.id, revision: snapshot.revision})
		return struct{}{}, nil
	})
	return err
}

var _ RemoteWriter = (*remoteWriter)(nil)
