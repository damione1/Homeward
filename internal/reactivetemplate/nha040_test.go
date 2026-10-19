package reactivetemplate

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA040NativeEdgeDoesNotRecomputeReactiveTemplates(t *testing.T) {
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "native", host)
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan([]Definition{
		nha044Definition(t, "native-state", "{{ states('switch.native') }}", nil),
		nha044Definition(t, "native-clock", "{{ now() }}", nil),
	}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha044Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := Start(ctx, fixture.Sequencer(), plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	for range 2 {
		_ = nha044Update(t, service.Updates())
	}
	nha044WaitForTimer(t, clock, 1)
	instance, _ := domain.ParseIntegrationInstanceID("native")
	sequence := uint64(1)
	stamp, err := domain.NewSourceStamp("nha040-reactive", &sequence)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := integrations.NewNativeMQTTEdge("buttons/press", []byte("pressed"), false, sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Sequencer().AcceptNativeMQTTEdge(ctx, instance, stamp, edge); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-service.Updates():
		t.Fatalf("native edge recomputed a reactive result: %+v", update)
	case <-time.After(40 * time.Millisecond):
	}
	if got := clock.count.Load(); got != 1 {
		t.Fatalf("native edge armed another now() timer: %d", got)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	on := domain.NewSwitchValue(true)
	at := clock.Now().Add(time.Second)
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	changed := nha044Update(t, service.Updates())
	current, ok := changed.CurrentResult()
	if !ok || current.Text() != "on" {
		t.Fatalf("related state stopped recomputing after native edge: %q %v", current.Text(), ok)
	}
}
