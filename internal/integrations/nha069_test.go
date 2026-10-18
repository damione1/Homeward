package integrations_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func TestNHA069CorrelatedLightProofRequiresEveryRequestedField(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	_, key, err := rig.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic color lamp", Constraints: &wireregistry.CapabilityDescription_Light{Light: &wireregistry.LightConstraints{SetOn: true, BrightnessPercent: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100}, ColorTempMireds: &wireregistry.NumberBounds{Minimum: 250, Maximum: 454}}}}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("light discovery: %v", err)
	}
	rig.binding = bindings[0]
	on := true
	brightness := 50.0
	colorTemp := uint32(300)
	rig.request.CapabilitySchemaId = contracts.LightSchema
	rig.request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, BrightnessPercent: &brightness, ColorTempMireds: &colorTemp}}}
	id := nha025ID(t, rig.request)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	publishLight := func(b *float64, ct *uint32, cause *domain.Causality) error {
		light, err := domain.NewLightValue(true, b, ct)
		if err != nil {
			return err
		}
		value, err := domain.NewLightStateValue(light)
		if err != nil {
			return err
		}
		at := time.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		if cause != nil {
			observation, err = observation.WithCausality(*cause)
			if err != nil {
				return err
			}
		}
		return rig.session.Publish(context.Background(), rig.binding, observation)
	}
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		cause, err := command.Causality()
		if err != nil {
			return err
		}
		if err := nha025Publish(t, rig, true, &cause, quality); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		wrongCT := uint32(299)
		if err := publishLight(&brightness, &wrongCT, &cause); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		if err := publishLight(nil, &colorTemp, &cause); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		if err := publishLight(&brightness, &colorTemp, nil); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		return publishLight(&brightness, &colorTemp, &cause)
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("light dispatch: %v %v", response, err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
}

type nha069DelayedSecondResolve struct {
	base    *fake.Fixture
	count   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (a *nha069DelayedSecondResolve) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	resolved, err := a.base.Resolve(ctx, instance, keys)
	if err != nil {
		return nil, err
	}
	if a.count.Add(1) == 2 {
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return resolved, nil
}

func TestNHA069ConcurrentDiscoverCannotReopenFencedBinding(t *testing.T) {
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	authority := &nha069DelayedSecondResolve{base: fixture, entered: make(chan struct{}), release: make(chan struct{})}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(authority, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha069-race")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate := nha016Candidate(t, "fenced-relay", "Synthetic relay")
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
		done <- err
	}()
	select {
	case <-authority.entered:
	case <-time.After(time.Second):
		t.Fatal("concurrent Resolve did not reach barrier")
	}
	key, err := integrations.NewSourceKey("controller", "fenced-relay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.FenceSource(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	close(authority.release)
	select {
	case err := <-done:
		if !errors.Is(err, integrations.ErrInactiveSession) {
			t.Fatalf("stale Resolve readmitted retired binding: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Discover did not finish")
	}
	if err := session.Publish(context.Background(), bindings[0], nha016Observation(t, time.Now())); err == nil {
		t.Fatal("old binding still publishes after concurrent Discover")
	}
	newSession, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSession.Discover(context.Background(), []integrations.Candidate{candidate}); err == nil {
		t.Fatal("replacement session reopened retired binding")
	}
}

type nha069PanickingCommandProvider struct{ started atomic.Bool }

func (p *nha069PanickingCommandProvider) CommandDriver() integrations.CommandDriver {
	panic("synthetic provider panic")
}
func (p *nha069PanickingCommandProvider) Start(context.Context, integrations.SessionHost, integrations.Tasks) error {
	p.started.Store(true)
	return nil
}
func (p *nha069PanickingCommandProvider) Stop(context.Context) error { return nil }

func TestNHA069SupervisorContainsCommandProviderPanic(t *testing.T) {
	rig := nha026SetupBare(t)
	pilot := &nha069PanickingCommandProvider{}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilot })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, supervisor.Done())
	if pilot.started.Load() {
		t.Fatal("pilot started after provider panic")
	}
	if status, ok := supervisor.Status(rig.instances[0]); !ok || status.Phase != integrations.PilotDegraded {
		t.Fatalf("panic status: %+v %v", status, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(ctx); !errors.Is(err, integrations.ErrPilotFactory) {
		t.Fatal(err)
	}
}
