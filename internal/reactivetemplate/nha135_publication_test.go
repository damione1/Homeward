package reactivetemplate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

// Block exactly one render before checkControl observes its source queue.
// No wall-clock sleep or polling decides where the retirement is admitted.
type nha135PublicationClock struct {
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (c *nha135PublicationClock) Now() time.Time {
	c.mu.Lock()
	entered, release := c.entered, c.release
	c.entered, c.release = nil, nil
	c.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	return time.Now()
}

func (*nha135PublicationClock) NewTimer(d time.Duration) Timer { return RealClock{}.NewTimer(d) }

func TestNHA135PlanPublicationJoinsSourceRetirementDuringRender(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, _, record := nha044Fixture(t, "nha135_retired", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	observed := nha044Definition(t, "nha135.observed", "{{ states('switch.nha135_retired') }}", nil)
	initial, err := NewPlan([]Definition{observed}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha135PublicationClock{}
	service, err := Start(ctx, fixture.Sequencer(), initial, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := service.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	initialUpdate := nha044Update(t, service.Updates())
	if _, current := initialUpdate.CurrentResult(); !current {
		t.Fatal("source fixture had no initial current result")
	}
	before := service.Publication()
	independent := nha044Definition(t, "nha135.independent", "{{ 'kept healthy' }}", nil)
	next, err := NewPlan([]Definition{observed, independent}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := service.ReservePlan(next)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	clock.mu.Lock()
	clock.entered, clock.release = entered, release
	clock.mu.Unlock()
	publication := make(chan error, 1)
	go func() { publication <- reservation.Publish(ctx) }()
	select {
	case <-entered:
	case err := <-publication:
		t.Fatalf("publication skipped the render barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("publication did not enter the render barrier", ctx.Err())
	}
	// A real session retirement publishes source absence and
	// health through the same Host/sequencer while the render is paused.
	if err := host.Revoke(session); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-publication:
		if err != nil {
			t.Fatalf("source scheduling escaped the single publication: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("publication did not join source retirement", ctx.Err())
	}
	if service.Publication() != before+1 {
		t.Fatal("retirement caused more than one plan publication")
	}
	seenSource, seenIndependent := false, false
	for !seenSource || !seenIndependent {
		select {
		case update, open := <-service.Updates():
			if !open {
				t.Fatal("retirement closed the publication engine")
			}
			if update.Publication() != before+1 {
				t.Fatal("old plan output survived the publication barrier")
			}
			switch update.ID() {
			case observed.ID():
				if _, current := update.CurrentResult(); current {
					t.Fatal("retired source remained current in the published plan")
				}
				seenSource = true
			case independent.ID():
				result, current := update.CurrentResult()
				if !current || result.Text() != "kept healthy" {
					t.Fatal("retirement degraded the independent program")
				}
				seenIndependent = true
			}
		case <-ctx.Done():
			t.Fatal("published outputs did not arrive", ctx.Err())
		}
	}
	if err := service.Error(); err != nil {
		t.Fatalf("retirement became an engine failure: %v", err)
	}
}

func TestNHA135PlanPublicationRefusesLossDuringSubscriptionHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "nha135_loss", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	observed := nha044Definition(t, "nha135.loss-observed", "{{ states('switch.nha135_loss') }}", nil)
	initial, err := NewPlan([]Definition{observed}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha135PublicationClock{}
	service, err := Start(ctx, fixture.Sequencer(), initial, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := service.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	if initial := nha044Update(t, service.Updates()); initial.Diagnostic() != DiagnosticHealthy {
		t.Fatal("initial source was not healthy")
	}
	before := service.Publication()
	independent := nha044Definition(t, "nha135.loss-independent", "{{ 'candidate healthy' }}", nil)
	next, err := NewPlan([]Definition{observed, independent}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := service.ReservePlan(next)
	if err != nil {
		t.Fatal(err)
	}
	arm := func() (<-chan struct{}, func()) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		clock.mu.Lock()
		clock.entered, clock.release = entered, release
		clock.mu.Unlock()
		return entered, unblock
	}
	publication := make(chan error, 1)
	await := func(entered <-chan struct{}, phase string) {
		t.Helper()
		select {
		case <-entered:
		case err := <-publication:
			t.Fatalf("publication ended before %s: %v", phase, err)
		case <-ctx.Done():
			t.Fatal("render barrier "+phase, ctx.Err())
		}
	}
	first, releaseFirst := arm()
	go func() { publication <- reservation.Publish(ctx) }()
	await(first, "initial render")
	// One harmless queued entry selects the pending-source rebase, with no
	// loss yet. The next barrier is after its snapshot capture.
	health, err := integrations.NewHealthReport(integrations.HealthReady, "synthetic handoff pressure")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ReportHealth(ctx, health); err != nil {
		t.Fatal(err)
	}
	second, releaseSecond := arm()
	releaseFirst()
	await(second, "captured replacement render")
	// This state lies after the capture, so replay will render it only after
	// the replacement subscription is attached. Block that replay render.
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	value := domain.NewSwitchValue(true)
	at := time.Now().UTC()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	third, releaseThird := arm()
	releaseSecond()
	await(third, "replacement subscription replay")
	// A real view witness has the same capacity and receives the same burst.
	// Overflow occurs after attachment, covering the race a Status check
	// before recapture would miss. Health reports have no physical effect.
	snapshot, err := fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	witness, _, _, _, err := fixture.Sequencer().SubscribeTriggerViewAfter(snapshot.Cursor, 256, sequencer.MaxViewQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = witness.Close() })
	status, err := witness.Status()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= status.Capacity; index++ {
		if err := session.ReportHealth(ctx, health); err != nil {
			t.Fatal(err)
		}
	}
	status, err = witness.Status()
	if err != nil || status.PendingLoss == nil || status.ReplayValid {
		t.Fatalf("real view pressure did not establish loss: %+v %v", status, err)
	}
	releaseThird()
	select {
	case err := <-publication:
		if !errors.Is(err, sequencer.ErrCursorInvalidated) {
			t.Fatalf("handoff hid continuity loss: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("loss refusal did not finish", ctx.Err())
	}
	if service.Publication() != before {
		t.Fatal("lost stream advanced the plan publication")
	}
	if err := reservation.Release(); err != nil {
		t.Fatal("refused plan reservation was not retained", err)
	}
	// The old mailbox remains owned, allowing the normal loss path to emit
	// its required degradation before any recapture can become healthy.
	lost := nha044Update(t, service.Updates())
	if lost.ID() != observed.ID() || lost.Publication() != before || lost.Diagnostic() != DiagnosticLoss {
		t.Fatalf("loss was not preserved for the normal path: %v", lost.Diagnostic())
	}
	if _, current := lost.CurrentResult(); current {
		t.Fatal("loss retained a healthy current value")
	}
}
