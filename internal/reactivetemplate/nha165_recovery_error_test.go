package reactivetemplate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

// The first barrier overflows the existing live mailbox; the second forces
// >256 real admissions between recapture and SubscribeAfter. Retry is released
// through the same clock/timer interface the service normally owns.
type nha165RecoveryClock struct {
	at      time.Time
	calls   atomic.Int32
	entered chan int32
	release [2]chan struct{}
	once    [2]sync.Once
	retry   chan *nha044Timer
}

func (c *nha165RecoveryClock) Now() time.Time {
	n := c.calls.Add(1)
	if n == 2 || n == 3 {
		c.entered <- n
		<-c.release[n-2]
	}
	return c.at
}
func (c *nha165RecoveryClock) NewTimer(d time.Duration) Timer {
	timer := &nha044Timer{channel: make(chan time.Time, 1), due: c.at.Add(d)}
	c.retry <- timer
	return timer
}
func (c *nha165RecoveryClock) unblock(index int) {
	c.once[index].Do(func() { close(c.release[index]) })
}
func (c *nha165RecoveryClock) wait(t *testing.T, want int32) {
	t.Helper()
	select {
	case got := <-c.entered:
		if got != want {
			t.Fatalf("render barrier%d want%d", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("render barrier%d not reached", want)
	}
}

type nha165RecoveryFixture struct {
	service  *Service
	clock    *nha165RecoveryClock
	retry    *nha044Timer
	publish  func(bool)
	sequence *sequencer.Sequencer
	lost     uint64
}

func nha165ExpiredLiveRecapture(t *testing.T) nha165RecoveryFixture {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "nha165_recovery", host)
	if err = fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	definition := nha044Definition(t, "nha165_recovery", "{{ states('switch.nha165_recovery') }}", nil)
	plan, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha165RecoveryClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), entered: make(chan int32, 2), release: [2]chan struct{}{make(chan struct{}), make(chan struct{})}, retry: make(chan *nha044Timer, 4)}
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clock.unblock(0)
		clock.unblock(1)
		bounded, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := service.Close(bounded); err != nil {
			t.Error(err)
		}
	})
	initial := nha044Update(t, service.Updates())
	if initial.Diagnostic() != DiagnosticHealthy {
		t.Fatal("initial value not current")
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	publish := func(on bool) {
		t.Helper()
		value, at := domain.NewSwitchValue(on), clock.at
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = session.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	publish(true)
	clock.wait(t, 2)
	for i := 0; i < 257; i++ {
		publish(i%2 == 0)
	}
	// Read the cumulative counter while the overflowed subscription remains
	// installed; the intentional failed recapture has no installed view.
	count := nha165ViewLoss(t, fixture.Sequencer())
	if count == 0 {
		t.Fatal("overflow loss counter not recorded")
	}
	clock.unblock(0)
	clock.wait(t, 3)
	lost := nha044Update(t, service.Updates())
	if lost.Diagnostic() != DiagnosticLoss {
		t.Fatalf("gap published a current value: %v", lost.Diagnostic())
	}
	if _, ok := lost.CurrentResult(); ok {
		t.Fatal("gap value retained current status")
	}
	for i := 0; i < 257; i++ {
		publish(i%2 != 0)
	} // Last actual source state is off.
	clock.unblock(1)
	var retry *nha044Timer
	select {
	case retry = <-clock.retry:
	case <-time.After(2 * time.Second):
		t.Fatal("failed recapture did not arm owned retry")
	}
	if !errors.Is(service.Error(), sequencer.ErrCursorExpired) || !errors.Is(service.UnrecoveredError(), sequencer.ErrCursorExpired) {
		t.Fatalf("partial rebase erased active/history: history=%v active=%v", service.Error(), service.UnrecoveredError())
	}
	return nha165RecoveryFixture{service, clock, retry, publish, fixture.Sequencer(), count}
}
func nha165ViewLoss(t *testing.T, seq *sequencer.Sequencer) uint64 {
	t.Helper()
	queues, _, err := seq.ObservationMetrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range queues {
		if q.GetQueueClass() == 4 {
			return q.GetInstalled().GetLostDeliveriesTotal().GetExact()
		}
	}
	t.Fatal("view queue loss measurement absent")
	return 0
}
func TestNHA165ReactiveRecoveredReplayKeepsHistoryAndLoss(t *testing.T) {
	f := nha165ExpiredLiveRecapture(t)
	f.retry.channel <- f.retry.due
	recovered := nha044Update(t, f.service.Updates())
	current, ok := recovered.CurrentResult()
	if recovered.Diagnostic() != DiagnosticHealthy || !ok || current.Text() != "off" {
		t.Fatalf("retry did not publish latest state: diagnostic=%v value=%q current=%t", recovered.Diagnostic(), current.Text(), ok)
	}
	// EvaluateCondition is an engine-owned control barrier after publication;
	// observing its reply proves publishRecovered completed before inspection.
	predicate, err := nha165RecoveryPredicate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.EvaluateCondition(context.Background(), predicate, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.service.UnrecoveredError(); err != nil {
		t.Fatalf("successful full recovery stayed active: %v", err)
	}
	if !errors.Is(f.service.Error(), sequencer.ErrCursorExpired) {
		t.Fatalf("historical gap was erased: %v", f.service.Error())
	}
	if got := nha165ViewLoss(t, f.sequence); got < f.lost {
		t.Fatalf("loss counter reset: before=%d after=%d", f.lost, got)
	}
	f.publish(true)
	next := nha044Update(t, f.service.Updates())
	value, ok := next.CurrentResult()
	if !ok || value.Text() != "on" || next.Diagnostic() != DiagnosticHealthy {
		t.Fatal("new delta not current after recovery")
	}
	if err = f.service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.service.UnrecoveredError(); err != nil {
		t.Fatalf("recovered service failed terminal close: %v", err)
	}
}
func TestNHA165ReactiveUnrecoveredReplayStillFailsAtClose(t *testing.T) {
	f := nha165ExpiredLiveRecapture(t)
	if err := f.service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(f.service.UnrecoveredError(), sequencer.ErrCursorExpired) {
		t.Fatalf("unrepaired gap vanished at close: %v", f.service.UnrecoveredError())
	}
}
func TestNHA165ReactiveRecoveryDoesNotHealTerminalFailures(t *testing.T) {
	for _, failure := range []error{ErrWorkBudget, ErrOutputFull, ErrInvalidProjection, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := nha165ExpiredLiveRecapture(t)
			f.service.setError(failure)
			f.retry.channel <- f.retry.due
			if update := nha044Update(t, f.service.Updates()); update.Diagnostic() != DiagnosticHealthy {
				t.Fatal("control recovery did not complete")
			}
			predicate, err := nha165RecoveryPredicate()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.EvaluateCondition(context.Background(), predicate, nil); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(f.service.UnrecoveredError(), failure) {
				t.Fatalf("rebase healed unrelated failure %v: %v", failure, f.service.UnrecoveredError())
			}
		})
	}
}

func nha165RecoveryPredicate() (template.Program, error) { return template.Compile("{{ true }}") }
