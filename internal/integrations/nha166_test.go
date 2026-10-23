package integrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func nha166Host(t *testing.T) (*integrations.Manager, *fake.Fixture, *integrations.Session, *nha022Generation) {
	t.Helper()
	ctx := context.Background()
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewMQTTReadOnlyObservationHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha166-passive")
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, key, "switch.nha166", "switch.nha166", false)}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	device, err := fake.NewActionDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := device.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if !host.MQTTReadOnlyObservation() || !host.AdmissionsOpen() {
		t.Fatal("observation closed state ingestion or lost guard")
	}
	t.Cleanup(func() { _ = host.CloseAdmissions(); _, _ = host.ClosePreparedRuntime(context.Background()) })
	return host, fixture, session, &nha022Generation{revision: 7}
}
func TestNHA166AllVolatileActionAdmissionsRefuseBeforeLedgerAndDriver(t *testing.T) {
	host, fixture, session, generation := nha166Host(t)
	driver := &nha022Driver{}
	if err := host.AttachCommandDriver(session, driver); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("observation attached an action driver", err)
	}
	service, err := integrations.NewVolatileCommandService(host, generation, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	router, err := integrations.NewCommandRouter(host, generation)
	if err != nil {
		t.Fatal(err)
	}
	start, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(context.Background(), nha016Instance(t, "nha166-passive"), []integrations.SourceKey{mustNHA166Key(t)})
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	for _, actor := range []string{"nha166-admin", "nha166-user"} {
		identity, err := domain.ParseActorID(actor)
		if err != nil {
			t.Fatal(err)
		}
		for retry := 0; retry < 2; retry++ {
			if response, err := router.Dispatch(context.Background(), identity, request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
				t.Fatal("direct/replay action was not denied", err)
			}
			if response, err := service.Dispatch(context.Background(), identity, request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
				t.Fatal("volatile/replay action was not denied", err)
			}
		}
	}
	alias := nha022Alias(t, "switch.nha166")
	// Captures require a command driver, which this immutable Host can never
	// attach. No opaque prepared capability can be minted in this profile.
	if _, err := service.CaptureDirect(context.Background(), alias, integrations.DirectSwitch); !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
		t.Fatal("observation minted prepared direct capture", err)
	}
	status, err := service.Status()
	if err != nil || status.Entries != 0 || status.RetainedBytes != 0 || driver.count() != 0 || host.CommandDriverEntries() != 0 {
		t.Fatal("refused action mutated ledger or entered driver", err, status)
	}
}
func mustNHA166Key(t *testing.T) integrations.SourceKey {
	t.Helper()
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func TestNHA166DurableAndTrustedOccurrenceRefuseBeforeLedgerCallback(t *testing.T) {
	host, fixture, _, generation := nha166Host(t)
	store, _ := nha024Store(t)
	service, err := integrations.NewDurableCommandService(context.Background(), host, generation, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	start, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(context.Background(), nha016Instance(t, "nha166-passive"), []integrations.SourceKey{mustNHA166Key(t)})
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	for i := 0; i < 2; i++ {
		if response, err := service.Dispatch(context.Background(), nha022Actor(t), request); response != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
			t.Fatal("durable/replay action admitted", err)
		}
	}
	port, err := service.InternalAutomationPort()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := port.WithOccurrence(context.Background(), "nha166-double", func(context.Context) error { called = true; return nil }); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) || called {
		t.Fatal("trusted occurrence callback entered", err)
	}
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.CommandIntent(context.Background(), id); err != nil || found {
		t.Fatal("durable refusal created an intent", err)
	}
	if host.CommandDriverEntries() != 0 {
		t.Fatal("durable refusal entered driver")
	}
}
