package calendars

import (
	"context"
	"fmt"
	"time"

	rrule "github.com/teambition/rrule-go"
)

type calculationBudget struct {
	ctx              context.Context
	deadline         time.Time
	work, candidates int64
}

func newBudget(ctx context.Context) *calculationBudget {
	return &calculationBudget{ctx: ctx, deadline: time.Now().Add(500 * time.Millisecond)}
}
func (b *calculationBudget) charge(work int64, candidate bool) error {
	if err := canceled(b.ctx); err != nil {
		return err
	}
	if work < 0 || b.work+work > 32000000 || candidate && b.candidates >= 100000 || !time.Now().Before(b.deadline) {
		return failure(BudgetExceeded)
	}
	b.work += work
	if candidate {
		b.candidates++
	}
	return nil
}

// NewRRule/Iterator at v1.8.2 (MIT), commit429348ca4477691b08edb0200b7f837edd32414c.
// Profile admission excludes every cross-filter and subdaily loop. Gregorian
// oracle P0 proves ≤8 periods per Next; each dayset has ≤366 days and one time.
// No Set.Iterator, All, Between, timeout goroutine, or shared library values.
func ruleIterator(rule Rule, ceiling time.Time) (rrule.Next, error) {
	options := rrule.ROption{Dtstart: rule.anchor.wall().wallUTC(), Interval: rule.interval, Wkst: rrule.MO}
	hardEnd := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if ceiling.After(hardEnd) {
		ceiling = hardEnd
	}
	options.Until = ceiling
	switch rule.frequency {
	case Daily:
		options.Freq = rrule.DAILY
	case Weekly:
		options.Freq = rrule.WEEKLY
		days := [7]rrule.Weekday{rrule.MO, rrule.TU, rrule.WE, rrule.TH, rrule.FR, rrule.SA, rrule.SU}
		for i, day := range days {
			if rule.weekdays&(1<<uint(i)) != 0 {
				options.Byweekday = append(options.Byweekday, day)
			}
		}
	case Monthly:
		options.Freq = rrule.MONTHLY
		options.Bymonthday = append([]int(nil), rule.monthDays...)
	case Yearly:
		options.Freq = rrule.YEARLY
	default:
		return nil, failure(InvalidInput)
	}
	lib, err := rrule.NewRRule(options)
	if err != nil {
		return nil, failure(InvalidInput)
	}
	return lib.Iterator(), nil
}

func occurrenceSpan(ctx context.Context, base Span, point Point, initial bool, budget *calculationBudget) (Span, bool, error) {
	if !base.Valid() || !point.Valid() || base.kind != point.kind {
		return Span{}, false, failure(InvalidOccurrence)
	}
	if err := budget.charge(1, false); err != nil {
		return Span{}, false, err
	}
	if point == base.point() {
		return base, true, nil
	}
	if base.kind == AllDay {
		end, err := point.date.add(base.endDate.ordinal() - base.startDate.ordinal())
		if err != nil {
			if !initial {
				return Span{}, false, nil
			}
			return Span{}, false, err
		}
		span, err := AllDaySpan(point.date, end)
		return span, err == nil, err
	}
	if err := budget.charge(128, false); err != nil {
		return Span{}, false, err
	}
	start, present, err := resolveWall(ctx, point.local, base.zone, initial)
	if err != nil || !present {
		return Span{}, false, err
	}
	end, err := NewInstant(start.value.Add(base.end.value.Sub(base.start.value)))
	if err != nil {
		if !initial {
			return Span{}, false, nil
		}
		return Span{}, false, err
	}
	return Span{kind: Timed, anchor: point.local, start: start, end: end, zone: base.zone}, true, nil
}
func startKey(span Span) string {
	if span.kind == AllDay {
		return span.startDate.String()
	}
	return span.start.String()
}

// walkBase applies business COUNT after skipping generated gaps and before any
// exclusion/exception. DTSTART retains its original span and declared anchor.
func walkBase(ctx context.Context, event Event, ceiling Point, budget *calculationBudget, visit func(Point, Span) error) error {
	if !event.Valid() || !ceiling.Valid() || ceiling.kind != event.span.kind || visit == nil {
		return failure(InvalidInput)
	}
	if event.rule.state == ruleNone {
		if err := budget.charge(1, true); err != nil {
			return err
		}
		return visit(event.span.point(), event.span)
	}
	if int(ceiling.datePart().year)-int(event.span.point().datePart().year) > 100 {
		return failure(BudgetExceeded)
	}
	next, err := ruleIterator(event.rule, ceiling.wall().wallUTC())
	if err != nil {
		return err
	}
	count := 0
	previousStart := ""
	for {
		if event.rule.end == ruleCount && count >= event.rule.count {
			return nil
		}
		if err := budget.charge(event.rule.workCharge(), true); err != nil {
			return err
		}
		candidate, present := next() // synchronous, internally bounded by profile.
		if err := canceled(ctx); err != nil {
			return err
		}
		if !present {
			return nil
		}
		local := localFromTime(candidate)
		if !local.Valid() {
			return failure(InvalidDate)
		}
		point := Point{kind: Timed, local: local}
		if event.span.kind == AllDay {
			point = Point{kind: AllDay, date: local.date}
		}
		span, present, err := occurrenceSpan(ctx, event.span, point, false, budget)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if event.rule.end == ruleUntil {
			if span.kind == AllDay && span.startDate.ordinal() > event.rule.untilDate.ordinal() || span.kind == Timed && span.start.value.After(event.rule.untilInstant.value) {
				return nil
			}
		}
		key := startKey(span)
		if key == previousStart {
			continue
		}
		previousStart = key
		count++
		if err := visit(point, span); err != nil {
			return err
		}
	}
}

// validateEvent checks all target identities in one shared walk, not once per
// exception. Added dates are explicit DATE/DATE-TIME values, not COUNT members.
func validateEvent(ctx context.Context, event Event, budget *calculationBudget) error {
	if !event.Valid() {
		return failure(InvalidInput)
	}
	known := map[Point]bool{event.span.point(): true}
	pointKeys := make(map[Point]string, len(event.additions)+len(event.exclusions)+len(event.exceptions))
	for _, point := range event.additions {
		span, present, err := occurrenceSpan(ctx, event.span, point, true, budget)
		if err != nil {
			return err
		}
		if !present {
			return failure(InvalidOccurrence)
		}
		key := startKey(span)
		known[point] = true
		pointKeys[point] = key
	}
	targets := make(map[Point]bool, len(event.exclusions)+len(event.exceptions))
	ceiling := event.span.point()
	register := func(point Point) error {
		span, present, err := occurrenceSpan(ctx, event.span, point, true, budget)
		if err != nil {
			return err
		}
		if !present {
			return failure(InvalidOccurrence)
		}
		key := startKey(span)
		pointKeys[point] = key
		if !known[point] {
			targets[point] = true
			if point.wall().compare(ceiling.wall()) > 0 {
				ceiling = point
			}
		}
		return nil
	}
	for _, point := range event.exclusions {
		if err := register(point); err != nil {
			return err
		}
	}
	for _, exception := range event.exceptions {
		if err := register(exception.original); err != nil {
			return err
		}
	}
	// An explicit exclusion and an override of the same instant are ambiguous
	// in this profile; reject rather than silently discard either request.
	excluded := make(map[string]bool, len(event.exclusions))
	for _, point := range event.exclusions {
		excluded[pointKeys[point]] = true
	}
	for _, exception := range event.exceptions {
		if excluded[pointKeys[exception.original]] {
			return failure(InvalidOccurrence)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	if err := walkBase(ctx, event, ceiling, budget, func(point Point, _ Span) error { delete(targets, point); return nil }); err != nil {
		return err
	}
	if len(targets) != 0 {
		return failure(InvalidOccurrence)
	}
	return nil
}

// Window carries an explicit interpretation for mixed instant/date queries.
// Its all-day projection is only for overlap/order; stored dates stay civil.
type Window struct {
	kind             TemporalKind
	fromDate, toDate CivilDate
	from, to         Instant
	zone             Zone
}

func DateWindow(from, to CivilDate) (Window, error) {
	if !from.Valid() || !to.Valid() || to.ordinal() <= from.ordinal() || to.ordinal()-from.ordinal() > 366 {
		return Window{}, failure(InvalidInput)
	}
	return Window{kind: AllDay, fromDate: from, toDate: to}, nil
}
func TimeWindow(from, to Instant, zone Zone) (Window, error) {
	if !from.Valid() || !to.Valid() || !zone.Valid() || !to.value.After(from.value) || to.value.Sub(from.value) > 366*24*time.Hour {
		return Window{}, failure(InvalidInput)
	}
	a, b := localFromTime(from.value.In(zone.location)), localFromTime(to.value.In(zone.location))
	if !a.Valid() || !b.Valid() {
		return Window{}, failure(InvalidDate)
	}
	return Window{kind: Timed, fromDate: a.date, toDate: b.date, from: from, to: to, zone: zone}, nil
}

func (w Window) Valid() bool {
	if !w.fromDate.Valid() || !w.toDate.Valid() {
		return false
	}
	switch w.kind {
	case AllDay:
		return !w.from.Valid() && !w.to.Valid() && !w.zone.Valid() && w.toDate.ordinal() > w.fromDate.ordinal() && w.toDate.ordinal()-w.fromDate.ordinal() <= 366
	case Timed:
		return w.from.Valid() && w.to.Valid() && w.zone.Valid() && w.to.value.After(w.from.value) && w.to.value.Sub(w.from.value) <= 366*24*time.Hour
	default:
		return false
	}
}
func (w Window) ceiling(kind TemporalKind) Point {
	if kind == AllDay && w.kind == AllDay {
		date, _ := w.toDate.add(-1)
		return Point{kind: AllDay, date: date}
	}
	// resolveWall admits only offsets within±26h. Any start before to must have
	// civil wallUTC < toUTC+26h; the actual zone rule is still resolved separately.
	value := w.to.value.Add(26 * time.Hour)
	hard := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if value.After(hard) {
		value = hard
	}
	if kind == AllDay {
		return Point{kind: AllDay, date: localFromTime(value).date}
	}
	return Point{kind: Timed, local: localFromTime(value)}
}
func (w Window) identity() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s", w.kind, w.fromDate.String(), w.toDate.String(), w.from.String(), w.to.String(), w.zone.Name())
}
