package remotecalendar

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"sync"
	"time"
)

type WriterFactory interface {
	Open(context.Context, calendars.RemoteSource) (calendars.RemoteWriter, error)
}
type WindowReader interface {
	CollectRemoteWindow(context.Context, calendars.CalendarID, calendars.AutomationWindowRequest) (calendars.AutomationWindow, error)
	CheckRemoteWindow(context.Context, calendars.AutomationWindow) error
}
type WindowPublisher interface {
	Replace(context.Context, PublicationWindow) error
	Degrade(context.Context, calendars.RemoteReason) error
}
type publicationKind uint8

const (
	publicationDisabled publicationKind = iota + 1
	publicationEnabled
)

type Publication struct {
	kind publicationKind
	port WindowPublisher
}

func WithoutAutomationPublication() Publication { return Publication{kind: publicationDisabled} }
func WithAutomationPublication(port WindowPublisher) (Publication, error) {
	if nilPort(port) {
		return Publication{}, failure(Invalid)
	}
	return Publication{kind: publicationEnabled, port: port}, nil
}
func (p Publication) valid() bool {
	return p.kind == publicationDisabled && p.port == nil || p.kind == publicationEnabled && !nilPort(p.port)
}

type Source struct {
	settings    Settings
	id          calendars.CalendarID
	writers     WriterFactory
	reader      WindowReader
	publication Publication
	http        integrations.ConditionalFactory
	admitted    bool
}

func NewSource(s Settings, id calendars.CalendarID, writers WriterFactory, reader WindowReader, publication Publication, policy *integrations.CloudPolicy) (Source, error) {
	alias, ok := id.Alias()
	name, e := alias.Value()
	if !s.Valid() || !id.Valid() || id.Origin() != calendars.RemoteICS || !ok || e != nil || name != "calendar.famille" || nilPort(writers) || nilPort(reader) || !publication.valid() {
		return Source{}, failure(Invalid)
	}
	factory, e := integrations.NewConditionalFactory(s.conditional, policy)
	if e != nil {
		return Source{}, failure(Invalid)
	}
	return Source{settings: s, id: id, writers: writers, reader: reader, publication: publication, http: factory, admitted: true}, nil
}
func (s Source) valid() bool {
	return s.admitted && s.settings.Valid() && s.id.Valid() && !nilPort(s.writers) && !nilPort(s.reader) && s.publication.valid()
}
func (s Source) String() string               { return "remote calendar source [redacted]" }
func (s Source) GoString() string             { return s.String() }
func (s Source) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type intent uint8

const (
	intentNone intent = iota
	intentRenewWindow
	intentPersistStale
	intentRefresh
	intentStop
)

type version struct {
	revision      uint64
	digest, scope [32]byte
}

func versionOf(w calendars.AutomationWindow) version {
	d, _ := w.Digest()
	s, _ := w.RemoteState().SourceKey()
	n, _ := w.Revision().Number()
	return version{revision: n, digest: d, scope: s}
}

type knownCommit struct {
	observation integrations.ConditionalObservation
	store       storage.RemoteCalendarObservation
	receipt     storage.RemoteCalendarCommit
	at, until   time.Time
	mono        time.Duration
}
type workerEvent struct {
	commit  *knownCommit
	window  calendars.AutomationWindow
	refresh bool
	epoch   uint64
	ack     chan error
}
type sourceRun struct {
	mu           sync.Mutex
	source       Source
	clock        automationtriggers.Clock
	ctx          context.Context
	cancel       context.CancelFunc
	http         integrations.ConditionalHTTP
	writer       calendars.RemoteWriter
	scope        storage.RemoteCalendarScope
	epoch        uint64
	fresh        bool
	known        version
	commit       *knownCommit
	pending      intent
	wake         chan struct{}
	events       chan workerEvent
	cadence      cadence
	workerCancel context.CancelFunc
	lastWall     time.Time
	lastMono     time.Duration
	closed       bool
	done         chan struct{}
	tasks        int
	reject       storage.RemoteCalendarConditionalDisposition
	reason       calendars.RemoteReason
	lastFailure  *Error
}

func (r *sourceRun) enqueueLocked(i intent) {
	if r.closed {
		return
	}
	if i > r.pending {
		r.pending = i
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *sourceRun) enqueue(i intent) { r.mu.Lock(); defer r.mu.Unlock(); r.enqueueLocked(i) }

// The publication mutex protects only local gates and the bounded040 wrapper;
// no HTTP, SQL, parser or collection runs under it.
func (r *sourceRun) invalidateLocked(reason calendars.RemoteReason, disposition storage.RemoteCalendarConditionalDisposition) {
	r.fresh = false
	r.epoch++
	r.reason = reason
	r.reject = disposition
	if r.writer != nil {
		r.writer.Retire()
		r.writer = nil
	}
	if r.workerCancel != nil {
		r.workerCancel()
	}
	if r.http != nil {
		_ = r.http.Invalidate(r.ctx, integrations.ConditionalRejectPublication)
	}
	if r.source.publication.kind == publicationEnabled {
		_ = r.source.publication.port.Degrade(context.WithoutCancel(r.ctx), reason)
	}
}
func (r *sourceRun) fail(reason calendars.RemoteReason, disposition storage.RemoteCalendarConditionalDisposition, kind Failure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.lastFailure = &Error{kind: kind}
	r.invalidateLocked(reason, disposition)
	r.enqueueLocked(intentPersistStale)
}
func (r *sourceRun) ensureWriter(ctx context.Context, scope storage.RemoteCalendarScope) (calendars.RemoteWriter, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, failure(Closed)
	}
	a, _ := r.scope.Key()
	b, _ := scope.Key()
	if a != b {
		// Prepare already advanced the096 generation; preserve that new attempt.
		r.fresh = false
		r.epoch++
		if r.writer != nil {
			r.writer.Retire()
			r.writer = nil
		}
		if r.source.publication.kind == publicationEnabled {
			_ = r.source.publication.port.Degrade(ctx, calendars.RemoteReasonReconfigured)
		}
		r.scope = scope
	}
	if r.writer != nil {
		w := r.writer
		r.mu.Unlock()
		return w, nil
	}
	epoch := r.epoch
	r.mu.Unlock()
	source, e := calendars.NewRemoteSource(scope, r.source.settings.label)
	if e != nil {
		return nil, failure(Invalid)
	}
	w, e := r.source.writers.Open(ctx, source)
	if e != nil {
		return nil, e
	}
	if nilPort(w) {
		return nil, failure(Invalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.epoch != epoch {
		w.Retire()
		return nil, failure(Closed)
	}
	r.writer = w
	return w, nil
}
func (r *sourceRun) sendEvent(e workerEvent) error {
	e.ack = make(chan error, 1)
	select {
	case r.events <- e:
	case <-r.ctx.Done():
		return failure(Closed)
	}
	select {
	case err := <-e.ack:
		return err
	case <-r.ctx.Done():
		return failure(Closed)
	}
}
func (r *sourceRun) refresh(ctx context.Context) error {
	entity, _ := r.source.id.Entity()
	attempt, e := r.http.Prepare(ctx, entity)
	if e != nil {
		return e
	}
	scope, e := attempt.Scope()
	if e != nil {
		return e
	}
	writer, e := r.ensureWriter(ctx, scope)
	if e != nil {
		return e
	}
	r.mu.Lock()
	epoch := r.epoch
	r.mu.Unlock()
	before, e := writer.Read(ctx)
	if e != nil {
		return e
	}
	observation, e := r.http.ObserveConditional(ctx, attempt, before.Selection())
	if e != nil {
		return e
	}
	observedMono := r.clock.MonoNow()
	observedAt, e := observation.ObservationTime()
	if e != nil {
		return e
	}
	store, e := observation.StoreObservation()
	if e != nil {
		return e
	}
	until, e := observation.FreshUntil()
	if e != nil {
		return e
	}
	writeCtx, e := calendars.WithRemoteObservationClock(ctx, store, observedAt, until, observedMono)
	if e != nil {
		return e
	}
	var receipt storage.RemoteCalendarCommit
	switch observation.Kind() {
	case integrations.ConditionalRepresentation200:
		// This frame owns the transferred body only until Replace returns.
		receipt, e = func() (storage.RemoteCalendarCommit, error) {
			body, e := observation.TakeRepresentationBytes()
			if e != nil {
				return storage.RemoteCalendarCommit{}, e
			}
			var projection *calendars.Zone
			if r.source.settings.projection.kind == projectionExplicit {
				z := r.source.settings.projection.zone
				projection = &z
			}
			return writer.Replace(writeCtx, before.Calendar().Revision(), store, body, projection)
		}()
	case integrations.ConditionalNotModified304:
		digest, ok := before.Digest()
		if !ok {
			return failure(Invalid)
		}
		receipt, e = writer.Revalidate(writeCtx, before.Calendar().Revision(), store, before.Validators(), digest)
	default:
		return failure(Invalid)
	}
	if e != nil {
		return e
	}
	if !receipt.Valid() {
		return failure(CommitUnknown)
	}
	if e = r.sendEvent(workerEvent{epoch: epoch, commit: &knownCommit{observation: observation, store: store, receipt: receipt, at: observedAt, until: until, mono: observedMono}}); e != nil {
		return e
	}
	if r.source.publication.kind == publicationDisabled {
		return nil
	}
	return r.collect(ctx, true)
}
func (r *sourceRun) persistStale(ctx context.Context) error {
	r.mu.Lock()
	scope, reason, disposition := r.scope, r.reason, r.reject
	r.mu.Unlock()
	if !scope.Valid() {
		return nil
	}
	w, e := r.ensureWriter(ctx, scope)
	if e != nil {
		return e
	}
	before, e := w.Read(ctx)
	if e != nil {
		return e
	}
	return w.Reject(ctx, before.Calendar().Revision(), r.clock.Now().UTC(), reason, disposition)
}
func classify(err error) (calendars.RemoteReason, storage.RemoteCalendarConditionalDisposition) {
	disposition := storage.PreserveAdmittedValidators
	var ce *integrations.ConditionalError
	if errors.As(err, &ce) {
		disposition = ce.Disposition()
		if ce.Kind() == integrations.ConditionalCachePolicy {
			return calendars.RemoteReasonCachePolicy, disposition
		}
	}
	var e *calendars.Error
	if errors.As(err, &e) {
		switch e.Code() {
		case calendars.CommitUnknown:
			return calendars.RemoteReasonUnavailable, disposition
		case calendars.StaleCursor, calendars.Conflict:
			return calendars.RemoteReasonReconfigured, disposition
		case calendars.Canceled, calendars.Closed:
			return calendars.RemoteReasonUnavailable, disposition
		default:
			return calendars.RemoteReasonContent, disposition
		}
	}
	return calendars.RemoteReasonTransport, disposition
}

func sourceFailureKind(err error) Failure {
	var own *Error
	if errors.As(err, &own) {
		return own.Kind()
	}
	var ce *calendars.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case calendars.CommitUnknown:
			return CommitUnknown
		case calendars.StaleCursor, calendars.Conflict:
			return RevisionChanged
		case calendars.Closed, calendars.Canceled:
			return Closed
		default:
			return Content
		}
	}
	var http *integrations.ConditionalError
	if errors.As(err, &http) && http.Kind() == integrations.ConditionalCachePolicy {
		return CachePolicy
	}
	return Transport
}
