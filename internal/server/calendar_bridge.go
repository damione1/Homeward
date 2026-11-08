package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/workday"
)

// calendarBridge is the sole local/calculated projection owner. Remote ICS
// retains its independent publisher and monotonic freshness deadline.
type calendarBridge struct {
	mu            sync.Mutex
	epochs        map[automationtriggers.CalendarSourceID]uint64
	recovering    map[automationtriggers.CalendarSourceID]bool
	armed         map[automationtriggers.CalendarSourceID]bool
	beforePublish func()

	handler  *calendarResourceHandler
	engine   *automationtriggers.Service
	store    *storage.Store
	clock    automationtriggers.Clock
	cancel   context.CancelFunc
	done     chan struct{}
	coverage map[automationtriggers.CalendarSourceID]time.Time
}

func startCalendarBridge(ctx context.Context, h *calendarResourceHandler, engine *automationtriggers.Service, store *storage.Store, clock automationtriggers.Clock) *calendarBridge {
	if engine == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &calendarBridge{handler: h, engine: engine, store: store, clock: clock, cancel: cancel, done: make(chan struct{}), coverage: map[automationtriggers.CalendarSourceID]time.Time{}}
	go b.run(ctx)
	return b
}
func (b *calendarBridge) Close() {
	if b != nil {
		b.cancel()
		<-b.done
	}
}

func (b *calendarBridge) run(ctx context.Context) {
	defer close(b.done)
	// One read owner; bounded notification coalescing always causes a full refresh.
	changes := make(chan struct{}, 1)
	stopped := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(stopped)
		initial := true
		for ctx.Err() == nil {
			sub, err := b.handler.service.Subscribe()
			if initial {
				close(ready)
				initial = false
			}
			if err == nil {
				for {
					change, readErr := sub.Next(ctx)
					if readErr != nil {
						break
					}
					if change.Calendar().Origin() != calendars.RemoteICS {
						entity, _ := change.Calendar().Entity()
						value, _ := entity.Value()
						source, _ := automationtriggers.ParseCalendarSourceID("calendar/" + value)
						b.invalidate(ctx, source)
						select {
						case changes <- struct{}{}:
						default:
						}
					}
				}
				sub.Close()
			}
			for _, d := range b.engine.CalendarDemands() {
				if raw, _ := d.Source.Value(); strings.HasPrefix(raw, "calendar/") {
					b.invalidate(ctx, d.Source)
				}
			}
			select {
			case changes <- struct{}{}:
			default:
			}
			timer := b.clock.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C():
			}
		}
	}()
	defer func() { b.cancel(); <-stopped }()
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}
	for {
		b.refresh(ctx)
		timer := b.clock.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changes:
			timer.Stop()
		case <-timer.C():
		}
	}
}

func (b *calendarBridge) refresh(ctx context.Context) {
	for _, d := range b.engine.CalendarDemands() {
		raw, _ := d.Source.Value()
		// This namespace is generated only from native local/calculated registry IDs.
		if !strings.HasPrefix(raw, "calendar/") {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		err := b.refreshSource(bounded, d)
		cancel()
		if err != nil {
			b.invalidate(ctx, d.Source)
		}
		if ctx.Err() != nil {
			return
		}
	}
}
func (b *calendarBridge) refreshSource(ctx context.Context, d automationtriggers.CalendarDemand) error {
	raw, _ := d.Source.Value()
	b.mu.Lock()
	epoch := b.epochs[d.Source]
	previous, loaded := b.coverage[d.Source]
	recoverGap := !loaded || b.recovering[d.Source]
	b.mu.Unlock()
	entity, err := domain.ParseEntityID(strings.TrimPrefix(raw, "calendar/"))
	if err != nil {
		return err
	}
	id, err := b.handler.service.Bind(ctx, entity)
	if err != nil {
		return err
	}
	if id.Origin() != calendars.Local && id.Origin() != calendars.Calculated {
		return ErrInvalidTriggerComposition
	}
	now := b.clock.Now().UTC().Truncate(time.Second)
	zone, err := calendars.ParseZone(d.Zone)
	if err != nil {
		return err
	}
	if !loaded {
		previous, err = b.store.ReadCalendarCoverage(ctx, raw)
		if err != nil {
			return err
		}
	}
	// Preserve a backwards-clock fence: no alarm earlier than committed coverage
	// can become newly executable after restart.
	if previous.After(now) {
		return ErrInvalidTriggerComposition
	}
	until := now.Add(time.Hour)
	historyFrom := now
	truncated := false
	if !previous.IsZero() && previous.Before(now) {
		historyFrom = previous
		if historyFrom.Before(now.Add(-48 * time.Hour)) {
			historyFrom = now.Add(-48 * time.Hour)
			truncated = true
		}
	}
	queryFrom := historyFrom.Add(-max(d.MaximumOffset, 0)).Add(-time.Second)
	queryUntil := until.Add(-min(d.MinimumOffset, 0))
	first, err := calendars.NewInstant(queryFrom)
	if err != nil {
		return err
	}
	last, err := calendars.NewInstant(queryUntil)
	if err != nil {
		return err
	}
	window, err := calendars.TimeWindow(first, last, zone)
	if err != nil {
		return err
	}
	var items []automationtriggers.CalendarOccurrence
	revision := uint64(1)
	appendSpan := func(uid, point string, span calendars.Span) error {
		start, end, name, allDay, err := calendars.ProjectSpan(ctx, span, zone)
		if err != nil {
			return err
		}
		// Query overlap includes long events. Preserve original span and identity;
		// the engine selects the requested beginning/end and its admitted offset.
		value, err := automationtriggers.NewCalendarOccurrence(d.Source, uid, point, start, end, name, allDay, domain.FreshnessFresh)
		if err != nil {
			return err
		}
		items = append(items, value)
		if len(items) > automationtriggers.MaxCalendarOccurrences {
			return automationtriggers.ErrCapacity
		}
		return nil
	}
	switch id.Origin() {
	case calendars.Local:
		current, err := b.handler.service.GetCalendar(ctx, id)
		if err != nil {
			return err
		}
		if current.Revision().Presence() == calendars.Present {
			page, err := b.handler.service.Query(ctx, id, window, automationtriggers.MaxCalendarOccurrences, calendars.Cursor{})
			if err != nil {
				return err
			}
			if _, more := page.Next(); more {
				return automationtriggers.ErrCapacity
			}
			revision, _ = page.Revision().Number()
			for _, o := range page.Items() {
				uid, _ := o.ID().Event().UID().Value()
				if err := appendSpan(uid, o.ID().Point().String(), o.Span()); err != nil {
					return err
				}
			}
			latest, err := b.handler.service.GetCalendar(ctx, id)
			if err != nil {
				return err
			}
			if latest.Revision() != page.Revision() {
				return ErrInvalidTriggerComposition
			}
		} else if number, ok := current.Revision().Number(); ok && number > 0 {
			revision = number
		}
	case calendars.Calculated:
		if workday.ValidateReader(b.handler.workday) != nil {
			return ErrInvalidTriggerComposition
		}
		bound, err := b.handler.workday.CalendarID()
		if err != nil || bound != id {
			return ErrInvalidTriggerComposition
		}
		from, err := zone.LocalDateContext(ctx, queryFrom)
		if err != nil {
			return err
		}
		to, err := zone.LocalDateContext(ctx, queryUntil)
		if err != nil {
			return err
		}
		to, err = to.AddDays(1)
		if err != nil {
			return err
		}
		spans, err := b.handler.workday.Dates(ctx, from, to)
		if err != nil {
			return err
		}
		for _, span := range spans {
			inside, err := window.ContainsSpan(ctx, span)
			if err != nil {
				return err
			}
			if !inside {
				continue
			}
			date, _, _ := span.Dates()
			if err := appendSpan("workday:"+workday.ProfileVersion+":"+date.String(), date.String(), span); err != nil {
				return err
			}
		}
	}
	active := make([]automationtriggers.CalendarOccurrence, 0, len(items))
	for _, item := range items {
		_, _, _, start, end, _, _, _, err := item.Components()
		if err != nil {
			return err
		}
		if end.After(now.Add(-24*time.Hour)) && start.Before(until.Add(24*time.Hour)) {
			active = append(active, item)
		}
	}
	projected, err := automationtriggers.NewProjectedCalendarWindow(d.Source, revision, now, until, nil, active)
	if err != nil {
		return err
	}
	if b.beforePublish != nil {
		b.beforePublish()
	}
	through, keys, err := b.publish(ctx, d.Source, epoch, projected, historyFrom, now, items, recoverGap, truncated)
	if err != nil {
		return err
	}
	if err = b.store.CommitCalendarCoverage(ctx, raw, through, keys); err != nil {
		return err
	}
	b.mu.Lock()
	if b.epochs[d.Source] == epoch {
		if b.coverage == nil {
			b.coverage = map[automationtriggers.CalendarSourceID]time.Time{}
		}
		b.coverage[d.Source] = through
		if b.recovering != nil {
			b.recovering[d.Source] = false
		}
	}
	b.mu.Unlock()
	return nil
}

func (b *calendarBridge) invalidate(ctx context.Context, source automationtriggers.CalendarSourceID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.epochs == nil {
		b.epochs = map[automationtriggers.CalendarSourceID]uint64{}
	}
	if b.recovering == nil {
		b.recovering = map[automationtriggers.CalendarSourceID]bool{}
	}
	b.epochs[source]++
	if !b.recovering[source] || b.armed[source] {
		_ = b.engine.DegradeCalendarSource(ctx, source)
	}
	b.recovering[source] = true
	if b.armed != nil {
		b.armed[source] = false
	}
}

// Publication and invalidation share this lock. A completed SQL read cannot
// overwrite a newer watch invalidation; SQL never runs under either mutex.
func (b *calendarBridge) publish(ctx context.Context, source automationtriggers.CalendarSourceID, epoch uint64, projected automationtriggers.CalendarWindow, from, to time.Time, items []automationtriggers.CalendarOccurrence, recoverGap, truncated bool) (time.Time, [][32]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.epochs[source] != epoch {
		return time.Time{}, nil, ErrInvalidTriggerComposition
	}
	if recoverGap && from.Before(to) {
		if err := b.engine.ReconcileCalendarGap(ctx, source, from, to, items, truncated); err != nil {
			return time.Time{}, nil, err
		}
	}
	if err := b.engine.ReplaceCalendarWindow(ctx, projected); err != nil {
		return time.Time{}, nil, err
	}
	if b.armed == nil {
		b.armed = map[automationtriggers.CalendarSourceID]bool{}
	}
	b.armed[source] = true
	through, keys := b.engine.CalendarCheckpoint(source, to)
	return through, keys, nil
}
