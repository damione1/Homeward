package mqtt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
)

// This adapter is test-only. The production transport deliberately has no
// Zigbee2MQTT discovery or entity mappings until NHA-069.
type nha068BindingAdapter struct{ binding integrations.Binding }

func (a nha068BindingAdapter) Observe(ctx context.Context, host integrations.SessionHost, observation Observation) error {
	_, payload, quality, err := observation.Record()
	if err != nil {
		return err
	}
	value := domain.NewSwitchValue(string(payload) == "on")
	state, err := integrations.NewStateObservation("relay", quality, &value, nil, time.Now(), nil)
	if err != nil {
		return err
	}
	if err := host.Publish(ctx, a.binding, state); err != nil {
		return err
	}
	if _, _, err := observation.Event(); errors.Is(err, ErrNotAnEvent) {
		return nil
	} else if err != nil {
		return err
	}
	event, err := integrations.NewDeviceEvent("fixture.relay.pressed", time.Now(), nil)
	if err != nil {
		return err
	}
	return host.Emit(ctx, a.binding, event)
}

type nha068BindingHost struct {
	*integrations.Session
	provider nha068Provider
}

func (h nha068BindingHost) Secrets() (integrations.SecretProvider, error) { return h.provider, nil }

func TestNHA068SyntheticBindingRetirementAndRestoredState(t *testing.T) {
	broker := testbroker.Start(t)
	broker.Publish(t, "nha068/state/relay", []byte("on"), true)
	settings := nha068Settings(t, broker.Address, ModePassive)
	manager, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.OpenSession(settings.instance)
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic relay", Endpoint: "relay", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("synthetic discovery: %v %d", err, len(bindings))
	}
	ids, err := fixture.Resolve(context.Background(), settings.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPilot(settings, nha068BindingAdapter{binding: bindings[0]})
	if err != nil {
		t.Fatal(err)
	}
	tasks := newNHA068Tasks()
	host := nha068BindingHost{Session: session, provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := p.Start(ctx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, p) })
	check := func(freshness domain.Freshness, provenance domain.Provenance) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			state, ok := fixture.State(ids[0].Entity)
			if ok {
				_, q, _, _, _, err := state.Components()
				if err == nil {
					a, f, pr, _ := q.Components()
					if a == domain.AvailabilityAvailable && f == freshness && pr == provenance {
						return
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("binding quality did not become %v/%v", freshness, provenance)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	check(domain.FreshnessStale, domain.ProvenanceRestored)
	if events, err := fixture.Events(); err != nil || len(events) != 0 {
		t.Fatalf("retained generated edge: %d %v", len(events), err)
	}
	broker.Publish(t, "nha068/state/relay", []byte("off"), true)
	check(domain.FreshnessFresh, domain.ProvenanceObserved)
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := fixture.Events()
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live edge missing: %d", len(events))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := manager.Revoke(session); err != nil {
		t.Fatal(err)
	}
	state, ok := fixture.State(ids[0].Entity)
	if !ok {
		t.Fatal("binding state missing after revoke")
	}
	_, q, _, _, _, err := state.Components()
	if err != nil {
		t.Fatal(err)
	}
	a, f, _, err := q.Components()
	if err != nil || a != domain.AvailabilityUnavailable || f != domain.FreshnessStale {
		t.Fatalf("retirement quality %v/%v %v", a, f, err)
	}
}
