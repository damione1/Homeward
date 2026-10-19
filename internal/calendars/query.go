package calendars

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"slices"
	"strings"
	"time"
)

const MaxPageSize = 1000

// Cursor is an admitted continuation, bound to the calendar, committed image
// revision and exact query window. Its Go zero explicitly means first page.
// Encoding/authentication on a network boundary belongs to NHA-161.
type Cursor struct {
	set      bool
	calendar CalendarID
	revision Revision
	window   [32]byte
	after    occurrenceOrder
}
type occurrenceOrder struct{ start, uid, point string }

func compareOrder(a, b occurrenceOrder) int {
	if n := strings.Compare(a.start, b.start); n != 0 {
		return n
	}
	if n := strings.Compare(a.uid, b.uid); n != 0 {
		return n
	}
	return strings.Compare(a.point, b.point)
}

type Occurrence struct {
	id                             RecurrenceID
	span                           Span
	summary, description, location string
	order                          occurrenceOrder
}

func (o Occurrence) ID() RecurrenceID    { return o.id }
func (o Occurrence) Span() Span          { return o.span }
func (o Occurrence) Summary() string     { return o.summary }
func (o Occurrence) Description() string { return o.description }
func (o Occurrence) Location() string    { return o.location }
func (o Occurrence) Valid() bool {
	return o.id.Valid() && o.span.Valid() && textValid(o.summary, 512, true) && o.id.point.kind == o.span.kind
}

type Page struct {
	items    []Occurrence
	next     Cursor
	more     bool
	revision Revision
}

func (p Page) Items() []Occurrence  { return slices.Clone(p.items) }
func (p Page) Next() (Cursor, bool) { return p.next, p.more }
func (p Page) Revision() Revision   { return p.revision }

// Keep only the smallest limit+1 results, even when an infinite series or many
// events overlap. A max-heap avoids collecting all intermediate occurrences.
type occurrenceHeap []Occurrence

func (h occurrenceHeap) Len() int           { return len(h) }
func (h occurrenceHeap) Less(i, j int) bool { return compareOrder(h[i].order, h[j].order) > 0 }
func (h occurrenceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *occurrenceHeap) Push(x any)        { *h = append(*h, x.(Occurrence)) }
func (h *occurrenceHeap) Pop() any {
	old := *h
	n := len(old)
	v := old[n-1]
	old[n-1] = Occurrence{}
	*h = old[:n-1]
	return v
}

// selection applies effective exclusive overlap and supplies its deterministic
// sort key. A DATE in a completely skipped civil day has no positive projected
// interval in TimeWindow; it remains a civil DATE in DateWindow.
func (window Window) selection(ctx context.Context, span Span, budget *calculationBudget) (string, bool, error) {
	switch span.kind {
	case Timed:
		if window.kind != Timed {
			return "", false, nil
		}
		return span.start.String(), span.start.value.Before(window.to.value) && span.end.value.After(window.from.value), nil
	case AllDay:
		if window.kind == AllDay {
			return span.startDate.String(), span.startDate.ordinal() < window.toDate.ordinal() && span.endDate.ordinal() > window.fromDate.ordinal(), nil
		}
		startWall := LocalDateTime{date: span.startDate}.wallUTC()
		endWall := LocalDateTime{date: span.endDate}.wallUTC()
		if !startWall.Add(-26*time.Hour).Before(window.to.value) || !endWall.Add(26*time.Hour).After(window.from.value) {
			return "", false, nil
		}
		if err := budget.charge(256, false); err != nil {
			return "", false, err
		}
		start, present, err := resolveWallTime(ctx, LocalDateTime{date: span.startDate}, window.zone, true)
		if err != nil || !present {
			return "", false, err
		}
		end, present, err := resolveWallTime(ctx, LocalDateTime{date: span.endDate}, window.zone, true)
		if err != nil || !present {
			return "", false, err
		}
		return start.UTC().Format(time.RFC3339), end.After(start) && start.Before(window.to.value) && end.After(window.from.value), nil
	default:
		return "", false, failure(InvalidInput)
	}
}
func expandEvent(ctx context.Context, snapshot EventSnapshot, window Window, budget *calculationBudget, emit func(Occurrence) error) error {
	event := snapshot.definition
	if window.kind == AllDay && event.span.kind != AllDay {
		return nil
	}
	excluded := make(map[string]bool, len(event.exclusions)+len(event.exceptions))
	additions := make(map[string]Point, len(event.additions))
	for _, point := range event.exclusions {
		span, _, err := occurrenceSpan(ctx, event.span, point, true, budget)
		if err != nil {
			return err
		}
		excluded[startKey(span)] = true
	}
	for _, exception := range event.exceptions {
		span, _, err := occurrenceSpan(ctx, event.span, exception.original, true, budget)
		if err != nil {
			return err
		}
		excluded[startKey(span)] = true
	}
	for _, point := range event.additions {
		span, _, err := occurrenceSpan(ctx, event.span, point, true, budget)
		if err != nil {
			return err
		}
		key := startKey(span)
		if _, exists := additions[key]; !exists {
			additions[key] = point
		}
	}
	output := func(point Point, span Span) error {
		if err := budget.charge(1, false); err != nil {
			return err
		}
		start, present, err := window.selection(ctx, span, budget)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		return emit(Occurrence{id: RecurrenceID{event: snapshot.key, point: point}, span: span, summary: event.summary, description: event.description, location: event.location, order: occurrenceOrder{start: start, uid: snapshot.key.uid.value, point: point.String()}})
	}
	if err := walkBase(ctx, event, window.ceiling(event.span.kind), budget, func(point Point, span Span) error {
		key := startKey(span)
		delete(additions, key)
		if excluded[key] {
			return nil
		}
		return output(point, span)
	}); err != nil {
		return err
	}
	// Remaining RDATEs may lie before DTSTART or outside the RRULE query walk.
	// Sorting makes budget/error behavior independent of map iteration order.
	keys := make([]string, 0, len(additions))
	for key := range additions {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if excluded[key] {
			continue
		}
		point := additions[key]
		span, _, err := occurrenceSpan(ctx, event.span, point, true, budget)
		if err != nil {
			return err
		}
		if err := output(point, span); err != nil {
			return err
		}
	}
	// Moved occurrences are selected by EFFECTIVE overlap. Their original
	// RECURRENCE-ID may be entirely outside the query's base generation window.
	for _, exception := range event.exceptions {
		if exception.kind == MoveOccurrence {
			if err := output(exception.original, exception.replacement); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *Repository) Query(ctx context.Context, id CalendarID, window Window, limit int, cursor Cursor) (Page, error) {
	if !id.Valid() || !window.Valid() || limit < 1 || limit > MaxPageSize {
		return Page{}, failure(InvalidInput)
	}
	if err := contextValid(ctx); err != nil {
		return Page{}, err
	}
	hash := sha256.Sum256([]byte(window.identity()))
	if cursor.set && (cursor.calendar != id || cursor.window != hash || !cursor.revision.validFor(id, EventUID{}) || cursor.revision.presence != Present) {
		return Page{}, failure(InvalidInput)
	}
	image, err := r.loadImage(ctx, id, nil)
	if err != nil {
		return Page{}, err
	}
	defer image.close()
	if cursor.set && cursor.revision != image.calendar.revision {
		return Page{}, failure(StaleCursor)
	}
	if r.beforeExpansion != nil {
		if err := r.beforeExpansion(ctx); err != nil {
			return Page{}, err
		}
	}
	budget := newBudget(ctx)
	if err := validateImage(ctx, image, budget); err != nil {
		return Page{}, err
	}
	found := make(occurrenceHeap, 0, limit+1)
	emit := func(o Occurrence) error {
		if cursor.set && compareOrder(o.order, cursor.after) <= 0 {
			return nil
		}
		if len(found) < limit+1 {
			heap.Push(&found, o)
		} else if compareOrder(o.order, found[0].order) < 0 {
			heap.Pop(&found)
			heap.Push(&found, o)
		}
		return nil
	}
	for _, snapshot := range image.events {
		if snapshot.revision.presence == Present {
			if err := expandEvent(ctx, snapshot, window, budget, emit); err != nil {
				return Page{}, err
			}
		}
	}
	slices.SortFunc(found, func(a, b Occurrence) int { return compareOrder(a.order, b.order) })
	page := Page{revision: image.calendar.revision, more: len(found) > limit}
	if page.more {
		found = found[:limit]
		page.next = Cursor{set: true, calendar: id, revision: image.calendar.revision, window: hash, after: found[len(found)-1].order}
	}
	page.items = found
	return page, nil
}
