package sequencer

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

func TestNHA218DiscoverySetsRetainAdmissionAndSessionFences(t *testing.T) {
	ctx := context.Background()
	s, err := New(RealClock{}, 32)
	if err != nil {
		t.Fatal(err)
	}
	device, _ := domain.ParseDeviceID("synthetic-device")
	entity, _ := domain.ParseEntityID("Host:synthetic-light")
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(nha184Identity{device, entity}, s, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("synthetic-instance")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("synthetic-device", "light")
	light := &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, DisplayName: "Synthetic light", Endpoint: "light", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true}}}
	relay := &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic relay", Endpoint: "relay", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
	candidate := func(descriptions ...*registry.CapabilityDescription) integrations.Candidate {
		t.Helper()
		c, e := integrations.NewCandidate(key, descriptions)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	first := candidate(light, relay)
	bindings, err := session.Discover(ctx, []integrations.Candidate{first})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Capture()
	position, _ := s.CataloguePosition()
	reversed := candidate(proto.Clone(relay).(*registry.CapabilityDescription), proto.Clone(light).(*registry.CapabilityDescription))
	if _, err := session.Discover(ctx, []integrations.Candidate{reversed}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Capture()
	next, _ := s.CataloguePosition()
	if before.Cursor != after.Cursor || position != next {
		t.Fatal("descriptor ordering invalidated identical set")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := session.Discover(cancelled, []integrations.Candidate{first}); !errors.Is(err, context.Canceled) {
		t.Fatal("identical discovery bypassed cancellation", err)
	}
	changed := proto.Clone(light).(*registry.CapabilityDescription)
	changed.GetLight().Rgb = true
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate(changed, relay)}); err != nil {
		t.Fatal(err)
	}
	next, _ = s.CataloguePosition()
	if next == position {
		t.Fatal("RGB constraint change did not invalidate catalogue")
	}
	// Projection withdrawal must not leave an equality cache behind. Host
	// retirement fencing is tested separately and is intentionally permanent.
	if err := s.WithdrawDiscovery(ctx, entity); err != nil {
		t.Fatal(err)
	}
	withdrawn, _ := s.CataloguePosition()
	bindings, err = session.Discover(ctx, []integrations.Candidate{candidate(changed, relay)})
	if err != nil {
		t.Fatal(err)
	}
	rediscovered, _ := s.CataloguePosition()
	if rediscovered == withdrawn {
		t.Fatal("withdrawn declaration was mistaken for an unchanged set")
	}
	replacement, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	before, _ = s.Capture()
	if _, err := session.Discover(ctx, []integrations.Candidate{first}); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatal("old session rediscovered after replacement", err)
	}
	renewed, err := replacement.Discover(ctx, []integrations.Candidate{candidate(changed, relay)})
	if err != nil {
		t.Fatal(err)
	}
	after, _ = s.Capture()
	if before.Cursor != after.Cursor {
		t.Fatal("identical reconnect declaration emitted catalogue event")
	}
	if renewed[0] == bindings[0] {
		t.Fatal("new session borrowed old binding")
	}
	if err := replacement.Withdraw(ctx, bindings[0]); !errors.Is(err, integrations.ErrForeignBinding) {
		t.Fatal("old binding crossed replacement fence", err)
	}
	s.InvalidateRegistry()
	if _, err := replacement.Discover(ctx, []integrations.Candidate{candidate(changed, relay)}); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatal("identical discovery bypassed unavailable registry", err)
	}
}
