package mqtt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha068Timer struct{ requested chan chan time.Time }

func (timer nha068Timer) After(time.Duration) <-chan time.Time {
	next := make(chan time.Time, 1)
	timer.requested <- next
	return next
}

type nha068Jitter struct{}

func (nha068Jitter) Offset(time.Duration) time.Duration { return 0 }

type nha068SupervisedAdapter struct {
	binding      integrations.Binding
	discovered   bool
	observations chan ObservationClass
}

func (a *nha068SupervisedAdapter) Observe(ctx context.Context, host integrations.SessionHost, observed Observation) error {
	if !a.discovered {
		key, err := integrations.NewSourceKey("synthetic-controller", "relay")
		if err != nil {
			return err
		}
		candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic relay", Endpoint: "relay", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}}}})
		if err != nil {
			return err
		}
		bindings, err := host.Discover(ctx, []integrations.Candidate{candidate})
		if err != nil {
			return err
		}
		if len(bindings) != 1 {
			return integrations.ErrIdentityConflict
		}
		a.binding = bindings[0]
		a.discovered = true
	}
	_, payload, quality, err := observed.Record()
	if err != nil {
		return err
	}
	if string(payload) != "on" && string(payload) != "off" {
		return ErrInvalidSettings
	}
	value := domain.NewSwitchValue(string(payload) == "on")
	state, err := integrations.NewStateObservation("relay", quality, &value, nil, time.Now(), nil)
	if err != nil {
		return err
	}
	if err := host.Publish(ctx, a.binding, state); err != nil {
		return err
	}
	if _, _, err := observed.Event(); err == nil {
		event, err := integrations.NewDeviceEvent("fixture.relay.pressed", time.Now(), nil)
		if err != nil {
			return err
		}
		if err := host.Emit(ctx, a.binding, event); err != nil {
			return err
		}
	} else if !errors.Is(err, ErrNotAnEvent) {
		return err
	}
	select {
	case a.observations <- observed.class:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNHA068SupervisorNetworkLossRetiresBindingBeforeRetryAndResubscribes(t *testing.T) {
	broker := testbroker.Start(t)
	commandObserver := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha068-supervisor-command-observer", true, 0, nil)
	commands := commandObserver.SubscribeRAP(t, true, "nha068/command/#")
	broker.Publish(t, "nha068/state/relay", []byte("on"), true)
	settings := nha068Settings(t, broker.Address, ModePassive)
	cluster := testpg.Start(t)
	pg, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo, err := store.BootstrapSecrets(context.Background(), filepath.Join(t.TempDir(), "app-data"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		secretName, err := storage.ParseSecretName(name)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := storage.NewSecretHandle(settings.instance, secretName)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.RotateCredential(context.Background(), handle, secret); err != nil {
			t.Fatal(err)
		}
	}
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(repo); err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(context.Background(), settings.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	observations := make(chan ObservationClass, 8)
	spec, err := integrations.NewPilotSpec(settings.instance, func() integrations.Pilot {
		p, _ := NewPilot(settings, &nha068SupervisedAdapter{observations: observations})
		return p
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewRetryPolicy(1, 20*time.Millisecond, time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	timer := nha068Timer{requested: make(chan chan time.Time, 1)}
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, policy, timer, nha068Jitter{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer stopCancel()
		if err := supervisor.Stop(stopCtx); err != nil {
			t.Error(err)
		}
		if count := broker.PublishCount(settings.clientID); count != 0 {
			t.Errorf("passive pilot emitted %d PUBLISH packets by joined Stop", count)
		}
	})
	select {
	case class := <-observations:
		if class != ObservationRestored {
			t.Fatalf("initial snapshot class %v", class)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("initial retained state missing")
	}
	state, ok := fixture.State(ids[0].Entity)
	if !ok {
		t.Fatal("binding not published")
	}
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		t.Fatal(err)
	}
	a, f, provenance, _ := quality.Components()
	if a != domain.AvailabilityAvailable || f != domain.FreshnessStale || provenance != domain.ProvenanceRestored {
		t.Fatalf("restored quality %v/%v/%v", a, f, provenance)
	}
	client, ok := broker.Server.Clients.Get(settings.clientID)
	if !ok {
		t.Fatal("pilot client absent")
	}
	client.Stop(errors.New("synthetic network loss"))
	var release chan time.Time
	select {
	case release = <-timer.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervisor did not enter retry")
	}
	state, ok = fixture.State(ids[0].Entity)
	if !ok {
		t.Fatal("binding missing after network loss")
	}
	_, quality, _, _, _, err = state.Components()
	if err != nil {
		t.Fatal(err)
	}
	a, f, _, _ = quality.Components()
	if a != domain.AvailabilityUnavailable || f != domain.FreshnessStale {
		t.Fatalf("binding did not retire before retry: %v/%v", a, f)
	}
	if events, err := fixture.Events(); err != nil || len(events) != 0 {
		t.Fatalf("restored/loss emitted edge: %d %v", len(events), err)
	}
	if _, retained := broker.Server.Topics.Retained.Get("nha068/command/relay"); retained {
		t.Fatal("network loss stored a physical command")
	}
	release <- time.Now()
	select {
	case class := <-observations:
		if class != ObservationRestored {
			t.Fatalf("retry snapshot class %v", class)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resubscription snapshot missing")
	}
	broker.Publish(t, "nha068/state/relay", []byte("off"), true)
	select {
	case class := <-observations:
		if class != ObservationLive {
			t.Fatalf("live retained publication class %v", class)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("live publication missing after retry")
	}
	state, ok = fixture.State(ids[0].Entity)
	if !ok {
		t.Fatal("reconnected binding missing")
	}
	_, quality, _, _, _, err = state.Components()
	if err != nil {
		t.Fatal(err)
	}
	a, f, provenance, _ = quality.Components()
	if a != domain.AvailabilityAvailable || f != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
		t.Fatalf("reconnected quality %v/%v/%v", a, f, provenance)
	}
	if events, err := fixture.Events(); err != nil || len(events) != 1 {
		t.Fatalf("live event count %d %v", len(events), err)
	}
	select {
	case command := <-commands:
		t.Fatalf("lifecycle emitted physical command: %#v", command)
	case <-time.After(100 * time.Millisecond):
	}
	if count := broker.PublishCount(settings.clientID); count != 0 {
		t.Fatalf("passive pilot sent %d application PUBLISH packets", count)
	}
}
