package sequencer

import (
	"context"
	"errors"
	"strings"
	"testing"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func nha164CatalogueRecord(t *testing.T, name string) (domain.EntityID, integrations.RegistryRecord) {
	t.Helper()
	entity, _ := domain.ParseEntityID(name)
	device, _ := domain.ParseDeviceID("device-" + name)
	instance, _ := domain.ParseIntegrationInstanceID("nha164-fixture")
	key, _ := integrations.NewSourceKey("device-"+name, "relay")
	alias, _ := domain.ParseHAAlias("switch." + name)
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	return entity, record
}
func TestNHA164EntityCatalogueIsNarrowBoundedAndImmutable(t *testing.T) {
	s, err := New(RealClock{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	entity, record := nha164CatalogueRecord(t, "selected")
	if _, err = s.CaptureEntityCatalogue(entity, 4096); err == nil {
		t.Fatal("detached registry admitted")
	}
	if err = s.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	// The owning package fixture populates admitted-shaped declarations directly;
	// the production capture and budgeting are the subject of this test.
	s.mu.Lock()
	s.capabilities[entity] = []*registry.CapabilityDescription{{SchemaId: "core.switch.v1", Endpoint: "relay"}}
	other, _ := domain.ParseEntityID("other")
	s.capabilities[other] = []*registry.CapabilityDescription{{SchemaId: "core.switch.v1", Endpoint: "other", DisplayName: strings.Repeat("large", 1<<20)}}
	s.mu.Unlock()
	capture, err := s.CaptureEntityCatalogue(entity, 4096)
	if err != nil || capture.EstimatedBytes() > 4096 {
		t.Fatalf("unrelated large entity affected narrow capture: %v", err)
	}
	descriptions := capture.Descriptions()
	descriptions[0].Endpoint = "mutated"
	repeated, err := s.CaptureEntityCatalogue(entity, 4096)
	if err != nil || repeated.Descriptions()[0].Endpoint != "relay" || capture.Descriptions()[0].Endpoint != "relay" {
		t.Fatalf("declarations were mutable: %v", err)
	}
	s.mu.Lock()
	s.capabilities[entity][0].DisplayName = strings.Repeat("x", 4096)
	s.mu.Unlock()
	if _, err = s.CaptureEntityCatalogue(entity, 4096); !errors.Is(err, ErrSnapshotBudget) {
		t.Fatalf("oversized target admitted: %v", err)
	}
	if err = s.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CaptureEntityCatalogue(entity, 4096); err == nil {
		t.Fatal("removed registry entity admitted")
	}
	s.InvalidateRegistry()
	if _, err = s.CaptureEntityCatalogue(entity, 4096); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("unavailable registry not explicit: %v", err)
	}
}
