package calendars

import (
	"context"
	"testing"
)

func TestNHA101DomainWorkRefusalPrecedesMaskCredit(t *testing.T) {
	owned, release, err := admissionScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner := admissionFrom(owned)
	anchor := nha100Local(t, "2007-03-11T02:00:00")
	rule, err := parseObservanceRule("FREQ=YEARLY;BYMONTH=3;BYDAY=2SU", anchor)
	if err != nil {
		t.Fatal(err)
	}
	zone := &embeddedZone{observances: []embeddedObservance{{anchor: anchor, rule: rule}}}
	budget := newBudget(owned)
	budget.work = 31900000 // fewer units left than one admitted Next charge
	owned = withCalendarBudget(owned, budget)
	err = zone.ensure(owned, 9999)
	t.Logf("work-refusal observation=%+v error=%v", owner.observation, err)
	nha100Code(t, err, BudgetExceeded)
	if owner.observation.MaskReservedBytes != 0 || owner.observation.RuleConstructors != 0 || owner.observation.NextCalls != 0 {
		t.Fatal("unreachable mask/constructor/Next charged before work refusal")
	}
	if zone.throughYear != 0 || len(zone.transitions) != 0 {
		t.Fatal("refused compilation published state")
	}
}

func TestNHA101DomainMaskCreditMonotoneAndRefusal(t *testing.T) {
	owned, release, err := admissionScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner := admissionFrom(owned)
	credit, err := newObservanceMaskCredit(owner, annualOrdinalWeekday, 2224)
	if err != nil {
		t.Fatal(err)
	}
	if err = credit.beforeNext(owner, 2224); err != nil {
		t.Fatal(err)
	}
	first := owner.observation.MaskReservedBytes
	if credit.throughYear != 2625 {
		t.Fatal("cycle400 and anticipatory rebuild not covered")
	}
	if err = credit.beforeNext(owner, 2224); err != nil {
		t.Fatal(err)
	}
	if owner.observation.MaskReservedBytes != first {
		t.Fatal("same reachable prefix paid twice")
	}
	if err = credit.beforeNext(owner, 2230); err != nil {
		t.Fatal(err)
	}
	if owner.observation.MaskReservedBytes-first != 6*4096 {
		t.Fatal("extension repaid an entire horizon instead of six source-audited yearly allocations")
	}
	// A rejected extension neither advances the owned prefix nor acquires
	// the requested bytes. This is a capacity failure, never a budget alias.
	if err = owner.reserveTo(parseReservationLimit); err != nil {
		t.Fatal(err)
	}
	previous := credit.throughYear
	err = credit.beforeNext(owner, 2231)
	t.Logf("credit-refusal observation=%+v error=%v", owner.observation, err)
	nha100Code(t, err, CapacityExceeded)
	if credit.throughYear != previous {
		t.Fatal("rejected prefix marked owned")
	}
}

func TestNHA101DomainMaskCreditStopsAt9999AndRepaysCompilation(t *testing.T) {
	owned, release, err := admissionScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner := admissionFrom(owned)
	a, err := newObservanceMaskCredit(owner, annualOrdinalWeekday, 9999)
	if err != nil {
		t.Fatal(err)
	}
	before := owner.observation.MaskReservedBytes
	if err = a.beforeNext(owner, 9999); err != nil {
		t.Fatal(err)
	}
	if a.throughYear != 9999 || owner.observation.MaskReservedBytes != before {
		t.Fatal("credited inaccessible year10000")
	}
	_, err = newObservanceMaskCredit(owner, annualOrdinalWeekday, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if owner.observation.MaskReservedBytes != 2*before {
		t.Fatal("recompilation did not repay original allocations")
	}
}
