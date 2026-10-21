package remotecalendar

import (
	"context"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"time"
)

func (s Source) request(clock automationtriggers.Clock) (calendars.AutomationWindowRequest, error) {
	if !s.valid() || nilPort(clock) {
		return calendars.AutomationWindowRequest{}, failure(Invalid)
	}
	now := clock.Now().UTC().Truncate(time.Second)
	a, e := calendars.NewInstant(now)
	if e != nil {
		return calendars.AutomationWindowRequest{}, failure(Invalid)
	}
	b, e := calendars.NewInstant(now.Add(calendars.MaxAutomationHorizon))
	if e != nil {
		return calendars.AutomationWindowRequest{}, failure(Invalid)
	}
	return calendars.NewAutomationWindowRequest(a, b, s.settings.view)
}
func (r *sourceRun) collect(ctx context.Context, refresh bool) error {
	r.mu.Lock()
	if r.closed || !r.fresh {
		r.mu.Unlock()
		return failure(Closed)
	}
	epoch := r.epoch
	r.mu.Unlock()
	request, e := r.source.request(r.clock)
	if e != nil {
		return e
	}
	window, e := r.source.reader.CollectRemoteWindow(ctx, r.source.id, request)
	if e != nil {
		return e
	}
	if e = r.source.reader.CheckRemoteWindow(ctx, window); e != nil {
		return e
	}
	return r.sendEvent(workerEvent{window: window, refresh: refresh, epoch: epoch})
}
func (r *sourceRun) publish(ctx context.Context, w calendars.AutomationWindow, refresh bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validContext(ctx) {
		return failure(Invalid)
	}
	return r.publishLocked(w, refresh)
}
func (r *sourceRun) publishLocked(w calendars.AutomationWindow, refresh bool) error {
	if r.closed || r.ctx == nil || r.ctx.Err() != nil || !r.fresh || !w.Valid() || w.Calendar() != r.source.id || versionOf(w) != r.known {
		return failure(Closed)
	}
	fresh, ok := w.RemoteState().Freshness()
	now, mono := r.clock.Now().UTC(), r.clock.MonoNow()
	until, hasDeadline := w.RemoteState().FreshUntil()
	if !ok || !hasDeadline || !now.Before(until) || fresh != calendars.RemoteFresh || r.cadence.expired(now, mono) {
		return failure(Expired)
	}
	if refresh && (r.commit == nil || !w.MatchesCommit(r.commit.receipt, r.commit.store)) {
		return failure(Invalid)
	}
	if r.source.publication.kind == publicationEnabled {
		publication := PublicationWindow{window: w, wall: now, until: until, mono: mono, untilMono: r.cadence.expiryMono}
		if _, _, _, _, ok := publication.Deadline(); !ok {
			return failure(Expired)
		}
		if e := r.source.publication.port.Replace(r.ctx, publication); e != nil {
			return failure(PublicationFailed)
		}
	}
	if r.cadence.expired(r.clock.Now().UTC(), r.clock.MonoNow()) {
		return failure(Expired)
	}
	if refresh {
		if e := r.http.ConfirmConditionalAdmitted(r.ctx, r.commit.observation, r.commit.receipt); e != nil {
			return failure(PublicationFailed)
		}
		r.commit = nil
	}
	r.cadence.windowSucceeded(now, mono)
	if r.pending == intentRenewWindow {
		r.pending = intentNone
	}
	return nil
}
