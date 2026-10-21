package calendars

import (
	"context"
	rrule "github.com/teambition/rrule-go"
	"time"
)

// pinnedNext owns the upstream object and never exports Options/OrigOptions.
// Its ceiling is canonical civil UTC, not an absent time.Time sentinel.
type pinnedNext struct {
	next    rrule.Next
	ceiling time.Time
	done    bool
}

func newPinnedNext(ctx context.Context, options rrule.ROption, ceiling time.Time) (*pinnedNext, error) {
	if err := contextValid(ctx); err != nil {
		return nil, err
	}
	anchor := options.Dtstart
	if anchor.Year() < 1 || anchor.Year() > 9999 || anchor.Nanosecond() != 0 || ceiling.Nanosecond() != 0 {
		return nil, failure(InvalidDate)
	}
	hard := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if ceiling.After(hard) {
		ceiling = hard
	}
	if ceiling.Before(anchor) {
		return &pinnedNext{ceiling: ceiling, done: true}, nil
	}
	// v1.8.2 checks IsZero before truncation. This is a private constructor
	// encoding only; the admitted anchor, identity and persistence stay exact.
	if anchor.IsZero() {
		options.Dtstart = anchor.Add(time.Nanosecond)
	}
	options.Until = ceiling
	// generate ignores an Until equal to Go zero after truncation. Keep that
	// canonical ceiling in this wrapper and give upstream an explicit9999.
	if ceiling.IsZero() {
		options.Until = hard
	}
	if a := admissionFrom(ctx); a != nil {
		a.observation.RuleConstructors++
	}
	rule, err := rrule.NewRRule(options)
	if err != nil {
		return nil, failure(InvalidInput)
	}
	if !rule.GetDTStart().Equal(anchor) || rule.GetDTStart().Nanosecond() != 0 {
		return nil, failure(Incompatible)
	}
	return &pinnedNext{next: rule.Iterator(), ceiling: ceiling}, nil
}
func (n *pinnedNext) take(ctx context.Context) (time.Time, bool, error) {
	if err := contextValid(ctx); err != nil {
		return time.Time{}, false, err
	}
	if n == nil || n.next == nil && !n.done {
		return time.Time{}, false, failure(InvalidInput)
	}
	if n.done {
		return time.Time{}, false, nil
	}
	if a := admissionFrom(ctx); a != nil {
		a.observation.NextCalls++
	}
	value, present := n.next()
	if err := canceled(ctx); err != nil {
		return time.Time{}, false, err
	}
	if !present || value.After(n.ceiling) {
		n.done = true
		return time.Time{}, false, nil
	}
	if value.Nanosecond() != 0 {
		return time.Time{}, false, failure(Incompatible)
	}
	return value, true, nil
}
