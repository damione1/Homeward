package integrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha040Identity struct{}

func (nha040Identity) Resolve(context.Context, domain.IntegrationInstanceID, []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	return nil, integrations.ErrIdentityConflict
}

func TestNHA040HostFencesNativeIngressByInstanceAndSession(t *testing.T) {
	ctx := context.Background()
	sequence, err := sequencer.New(sequencer.RealClock{}, 128)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(nha040Identity{}, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("native-mqtt")
	other, _ := domain.ParseIntegrationInstanceID("external-worker")
	first, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := host.OpenSession(other)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := integrations.NewNativeMQTTEdge("buttons/press", []byte("on"), false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PublishNativeMQTTEdge(ctx, edge); !errors.Is(err, integrations.ErrNativeMQTTEdgeUnavailable) {
		t.Fatalf("native ingress opened before trusted composition: %v", err)
	}
	if err := host.EnableNativeMQTTEdges(instance); err != nil {
		t.Fatal(err)
	}
	if err := host.EnableNativeMQTTEdges(instance); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("native grant was repeated: %v", err)
	}
	if err := otherSession.PublishNativeMQTTEdge(ctx, edge); !errors.Is(err, integrations.ErrNativeMQTTEdgeUnavailable) {
		t.Fatalf("foreign pilot published native edge: %v", err)
	}
	before, err := sequence.CaptureTriggerView(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PublishNativeMQTTEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	second, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PublishNativeMQTTEdge(ctx, edge); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("replaced session still admitted edge: %v", err)
	}
	if err := second.PublishNativeMQTTEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	sub, replay, _, _, err := sequence.SubscribeTriggerViewAfter(before.Cursor, 16, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	var sources []string
	for _, entry := range replay {
		if entry.Kind() != sequencer.KindNativeMQTTEdge {
			continue // Session retirement also emits a health report.
		}
		_, stamp, _, ok := entry.Origin()
		if !ok {
			t.Fatal("native edge lost its session origin")
		}
		session, sequence, err := stamp.Components()
		if err != nil || sequence == nil || *sequence != 1 {
			t.Fatalf("native source sequence: %v %v", sequence, err)
		}
		sources = append(sources, session)
	}
	if len(sources) != 2 || sources[0] == sources[1] {
		t.Fatalf("reconnect did not get a new source session: %+v", sources)
	}
	if err := host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	if err := second.PublishNativeMQTTEdge(ctx, edge); !errors.Is(err, integrations.ErrNativeMQTTEdgeUnavailable) {
		t.Fatalf("native ingress survived admission close: %v", err)
	}
}
