package automationtriggers

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"sync"
	"testing"
	"time"
)

type nha161Journal struct {
	mu               sync.Mutex
	keys             map[[32]byte]bool
	err              error
	entered, release chan struct{}
}

func (j *nha161Journal) ClaimCalendarDelivery(ctx context.Context, key [32]byte, _ time.Time) (bool, error) {
	if j.entered != nil {
		close(j.entered)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-j.release:
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return false, j.err
	}
	if j.keys[key] {
		return false, nil
	}
	j.keys[key] = true
	return true, nil
}
func nha161Engine(t *testing.T, clock *nha040Clock, j CalendarDeliveryJournal, edge string) *Service {
	t.Helper()
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := domain.ParseEntityID("Host:unused")
	plan, err := NewPlan(PlanInput{Automations: []ir.Automation{nha040Automation(t, "- trigger: calendar\n  entity_id: calendar.fixture\n  event: "+edge)}, Zone: "America/Toronto", Resolver: nha040Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AttachCalendarDeliveryJournal(j); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func nha161Publish(t *testing.T, s *Service, c *nha040Clock, start, end time.Time) {
	t.Helper()
	source, _ := ParseCalendarSourceID("fixture-calendar")
	item, err := NewCalendarOccurrence(source, "uid", "original", start, end, "America/Toronto", false, domain.FreshnessFresh)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewProjectedCalendarWindow(source, 1, c.Now(), c.Now().Add(time.Hour), nil, []CalendarOccurrence{item})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReplaceCalendarWindow(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}
func nha161Read(t *testing.T, s *Service) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, e := s.NextResult(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestNHA161DeliveryRestartMovedOriginalDoesNotReplay(t *testing.T) {
	j := &nha161Journal{keys: map[[32]byte]bool{}}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for run := 0; run < 2; run++ {
		c := &nha040Clock{wall: now.Add(time.Duration(run) * time.Hour)}
		s := nha161Engine(t, c, j, "start")
		nha161Publish(t, s, c, c.Now().Add(time.Minute), c.Now().Add(2*time.Minute))
		c.Advance(time.Minute, time.Minute)
		result := nha161Read(t, s)
		want := ResultFired
		if run == 1 {
			want = ResultMissed
		}
		if result.Kind() != want {
			t.Fatalf("run %d got %v want %v", run, result.Kind(), want)
		}
		_ = s.Close()
	}
}
func TestNHA161EndedEventKeepsFutureOffsetAfterRefresh(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	s := nha161Engine(t, c, &nha161Journal{keys: map[[32]byte]bool{}}, "end\n  offset: '+01:00:00'")
	start, end := c.Now().Add(-2*time.Hour), c.Now().Add(-30*time.Minute)
	nha161Publish(t, s, c, start, end)
	c.Advance(10*time.Minute, 10*time.Minute)
	nha161Publish(t, s, c, start, end)
	c.Advance(20*time.Minute, 20*time.Minute)
	if r := nha161Read(t, s); r.Kind() != ResultFired {
		t.Fatalf("end+offset lost: %v", r.Kind())
	}
}
func TestNHA161ClaimFailureCannotFireOrHoldEngineMutex(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	j := &nha161Journal{entered: make(chan struct{}), release: make(chan struct{}), err: errors.New("uncertain commit")}
	s := nha161Engine(t, c, j, "start")
	nha161Publish(t, s, c, c.Now().Add(time.Minute), c.Now().Add(2*time.Minute))
	c.Advance(time.Minute, time.Minute)
	result := make(chan Result, 1)
	go func() { r, _ := s.NextResult(context.Background()); result <- r }()
	select {
	case <-j.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("claim absent")
	}
	free := make(chan struct{})
	go func() { s.CalendarDemands(); close(free) }()
	select {
	case <-free:
	case <-time.After(time.Second):
		t.Fatal("SQL claim held engine lock")
	}
	close(j.release)
	select {
	case r := <-result:
		if r.Kind() != ResultMissed {
			t.Fatal("uncertain claim fired")
		}
	case <-time.After(time.Second):
		t.Fatal("claim did not return")
	}
}

func TestNHA161HistoricalGapIsExplicitAndCannotFire(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	s := nha161Engine(t, c, &nha161Journal{keys: map[[32]byte]bool{}}, "start")
	source, _ := ParseCalendarSourceID("fixture-calendar")
	item, err := NewCalendarOccurrence(source, "downtime", "original", c.Now().Add(-time.Hour), c.Now().Add(-45*time.Minute), "UTC", false, domain.FreshnessFresh)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileCalendarGap(context.Background(), source, c.Now().Add(-48*time.Hour), c.Now(), []CalendarOccurrence{item}, true); err != nil {
		t.Fatal(err)
	}
	if r := nha161Read(t, s); r.Kind() != ResultMissed || r.Automation() == "" {
		t.Fatal("elapsed identity not reported", r)
	}
	if r := nha161Read(t, s); r.Kind() != ResultMissed || r.Reason() != DegradedGap {
		t.Fatal("truncated interval hidden", r)
	}
}

func TestNHA161ClaimRevalidatesSourceAfterSQL(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	j := &nha161Journal{keys: map[[32]byte]bool{}, entered: make(chan struct{}), release: make(chan struct{})}
	s := nha161Engine(t, c, j, "start")
	nha161Publish(t, s, c, c.Now().Add(time.Minute), c.Now().Add(2*time.Minute))
	c.Advance(time.Minute, time.Minute)
	result := make(chan Result, 1)
	go func() { r, _ := s.NextResult(context.Background()); result <- r }()
	select {
	case <-j.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("claim absent")
	}
	source, _ := ParseCalendarSourceID("fixture-calendar")
	if err := s.DegradeCalendarSource(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	close(j.release)
	select {
	case r := <-result:
		if r.Kind() != ResultMissed {
			t.Fatal("source invalidated during claim still fired")
		}
	case <-time.After(time.Second):
		t.Fatal("claim did not return")
	}
}

func TestNHA161HealthyRenewalPreservesDueTimer(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	s := nha161Engine(t, c, &nha161Journal{keys: map[[32]byte]bool{}}, "start")
	start, end := c.Now().Add(time.Minute), c.Now().Add(2*time.Minute)
	nha161Publish(t, s, c, start, end)
	source, _ := ParseCalendarSourceID("fixture-calendar")
	item, _ := NewCalendarOccurrence(source, "uid", "original", start, end, "America/Toronto", false, domain.FreshnessFresh)
	// Hold the engine mutex while advancing: the timer is runnable but cannot
	// win before this healthy publication. No sleeps or scheduler luck.
	s.mu.Lock()
	c.Advance(time.Minute, time.Minute)
	window, err := NewProjectedCalendarWindow(source, 2, c.Now(), c.Now().Add(time.Hour), nil, []CalendarOccurrence{item})
	if err == nil {
		err = s.replaceCalendarWindowLocked(context.Background(), window)
	}
	premature := len(s.results)
	fence, _ := s.calendarCheckpointLocked(source, c.Now())
	s.mu.Unlock()
	if !fence.Before(start) || fence.Nanosecond() != 0 {
		t.Fatal("coverage overtook preserved due alarm", fence)
	}
	if err != nil || premature != 0 {
		t.Fatalf("healthy renewal consumed due edge: %d %v", premature, err)
	}
	if result := nha161Read(t, s); result.Kind() != ResultFired {
		t.Fatal("healthy boundary missed", result.Kind())
	}
}

func TestNHA161CheckpointFencesPendingClaimAndPersistsMissed(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	j := &nha161Journal{keys: map[[32]byte]bool{}, entered: make(chan struct{}), release: make(chan struct{})}
	s := nha161Engine(t, c, j, "start")
	nha161Publish(t, s, c, c.Now().Add(time.Minute), c.Now().Add(2*time.Minute))
	c.Advance(time.Minute, time.Minute)
	result := make(chan Result, 1)
	go func() { r, _ := s.NextResult(context.Background()); result <- r }()
	select {
	case <-j.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("claim absent")
	}
	source, _ := ParseCalendarSourceID("fixture-calendar")
	through, _ := s.CalendarCheckpoint(source, c.Now())
	if !through.Before(c.Now()) || through.Nanosecond() != 0 {
		t.Fatal("coverage overtook pending durable claim or lost second precision")
	}
	persistedFence := through
	if _, err := calendars.NewInstant(persistedFence); err != nil {
		t.Fatal("persisted fence cannot be queried", err)
	}
	close(j.release)
	select {
	case r := <-result:
		if r.Kind() != ResultFired {
			t.Fatal("claim did not fire")
		}
	case <-time.After(time.Second):
		t.Fatal("claim blocked")
	}
	through, _ = s.CalendarCheckpoint(source, c.Now())
	if !through.Equal(c.Now()) {
		t.Fatal("confirmed claim still fenced")
	}
	first, err := calendars.NewInstant(persistedFence.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	last, err := calendars.NewInstant(c.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	zone, _ := calendars.ParseZone("America/Toronto")
	if _, err := calendars.TimeWindow(first, last, zone); err != nil {
		t.Fatal("healthy refresh after claim rejected checkpoint", err)
	}
}

func TestNHA161PreservedDueDoesNotAdmitNewSameTimeOccurrence(t *testing.T) {
	c := &nha040Clock{wall: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	s := nha161Engine(t, c, &nha161Journal{keys: map[[32]byte]bool{}}, "start")
	start, end := c.Now().Add(time.Minute), c.Now().Add(2*time.Minute)
	nha161Publish(t, s, c, start, end)
	source, _ := ParseCalendarSourceID("fixture-calendar")
	a, _ := NewCalendarOccurrence(source, "uid", "original", start, end, "America/Toronto", false, domain.FreshnessFresh)
	b, _ := NewCalendarOccurrence(source, "new-uid", "new-original", start, end, "America/Toronto", false, domain.FreshnessFresh)
	s.mu.Lock()
	c.Advance(time.Minute, time.Minute)
	window, err := NewProjectedCalendarWindow(source, 2, c.Now(), c.Now().Add(time.Hour), nil, []CalendarOccurrence{a, b})
	if err == nil {
		err = s.replaceCalendarWindowLocked(context.Background(), window)
	}
	bKey, _, _ := calendarAlarm(s.plan.rules[0], b)
	correct := len(s.results) == 1 && s.results[0].kind == ResultMissed && s.results[0].calendarDelivery == calendarDeliveryKey(bKey)
	s.mu.Unlock()
	if err != nil || !correct {
		t.Fatal("new elapsed B inherited A's admission", err)
	}
	if r := nha161Read(t, s); r.Kind() != ResultMissed {
		t.Fatal("B was not missed")
	}
	if r := nha161Read(t, s); r.Kind() != ResultFired || r.calendarDelivery == calendarDeliveryKey(bKey) {
		t.Fatal("only original A may fire")
	}
}
