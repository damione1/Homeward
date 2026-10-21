package reactivetemplate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA029PermanentPlanSwapRetainsRealTemplateWait(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "first", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	initial, err := NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	initial, err = initial.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := Start(ctx, fixture.Sequencer(), initial, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	program, err := template.Compile("{{ is_state('switch.first', 'on') }}")
	if err != nil {
		t.Fatal(err)
	}
	wait, err := service.ObserveTemplate(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wait.Close(ctx)
	id, err := ParseProgramID("sensor.nha029_candidate")
	if err != nil {
		t.Fatal(err)
	}
	output, err := template.Compile("{{ true }}")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := NewDefinition(id, output, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	full := make([]Definition, 0, MaxPrograms)
	for index := 0; index < MaxPrograms; index++ {
		fullID, err := ParseProgramID(fmt.Sprintf("sensor.nha029_full_%d", index))
		if err != nil {
			t.Fatal(err)
		}
		entry, err := NewDefinition(fullID, output, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		full = append(full, entry)
	}
	fullPlan, err := NewPlan(full, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := service.ReservePlan(fullPlan); reserved != nil || !errors.Is(err, ErrWorkBudget) {
		t.Fatalf("64 permanent programs plus a real wait admitted: %v", err)
	}
	candidate, err := NewPlan([]Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = candidate.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := service.ReservePlan(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("published reservation was reusable: %v", err)
	}
	empty, err := NewPlan(nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	remove, err := service.ReservePlan(empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := remove.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wait.Ready():
		t.Fatal("old wait was terminated by permanent replacement")
	default:
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	at := time.Now().UTC()
	on := domain.NewSwitchValue(true)
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wait.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("retained wait did not observe same service after replacement")
	}
	if err := wait.Outcome(); err != nil {
		t.Fatal(err)
	}
}

func TestNHA029ConfigurationCutRebasesRealTemplateWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, binding, record := nha044Fixture(t, "first", host)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	plan, err := (Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := Start(ctx, fixture.Sequencer(), plan, RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := service.Close(cleanup); err != nil {
			t.Error(err)
		}
	}()
	predicate, err := template.Compile("{{ is_state('switch.first', 'on') }}")
	if err != nil {
		t.Fatal(err)
	}
	wait, err := service.ObserveTemplate(ctx, predicate, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := wait.Close(cleanup); err != nil {
			t.Error(err)
		}
	}()
	cut, err := fixture.Sequencer().BeginConfigurationPublication()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = cut.Release()
		}
	}()
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	// A real engine control joins the registry entry before it can evaluate.
	// It cannot replace that entry's required rebase with a timer or a test flag.
	if _, err := service.EvaluateCondition(ctx, predicate, nil); err != nil {
		t.Fatalf("live internal template could not rebase within the configuration cut: %v", err)
	}
	if err := service.Error(); err != nil {
		t.Fatalf("configuration cut manufactured a template loss: %v", err)
	}
	select {
	case <-wait.Ready():
		t.Fatal("configuration cut terminated an unchanged false wait")
	default:
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	on := domain.NewSwitchValue(true)
	observation, err := integrations.NewStateObservation("relay", quality, &on, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, observation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wait.Ready():
	case <-ctx.Done():
		t.Fatal("same template wait did not finish under the real cut")
	}
	if err := wait.Outcome(); err != nil {
		t.Fatal(err)
	}
	if err := service.Error(); err != nil {
		t.Fatalf("live template rebase retained a false loss: %v", err)
	}
	if err := cut.Release(); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
