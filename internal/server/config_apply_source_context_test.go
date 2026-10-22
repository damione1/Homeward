package server

import (
	"context"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"testing"
)

// The producer is the actual SQL24 registry -> Host admission -> Sequencer
// projection. This qualifies the source of historical input, not SQL26 replay.
func TestNHA055SourceContextCapturesOneRealAdmittedProjectionWithoutLiveAuthority(t *testing.T) {
	catalog, manifest := nha055SemanticCatalog(t)
	store := nha034ServerStore(t)
	ctx := context.Background()
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AttachProjection(ctx, sequence); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha055-context-real-host")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("opaque-context-device", "relay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ConfigureBinding(ctx, instance, key, "switch.chauffe_eau"); err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(repository, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Revoke(session) })
	discover := func(on bool) {
		t.Helper()
		candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{nha055SemanticSwitch(on, "relay")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
			t.Fatal(err)
		}
	}
	discover(true)
	snapshot, err := sequence.CaptureBounded(sequencer.MaxJournalBytes)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := configactivation.SourceContextFromSnapshot(snapshot, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.Valid() || len(admitted.Identities()) != 1 || len(admitted.Capabilities()) != 1 {
		t.Fatal("same atomic projection lost admitted compiler inputs")
	}
	restored, err := configcompilation.ParseContext(admitted.Bytes())
	if err != nil || restored.SHA256() != admitted.SHA256() {
		t.Fatal("historical compiler inputs did not round-trip exactly", err)
	}
	// Input maps/protobufs are detached. Mutating a captured snapshot cannot
	// grant or rewrite the stored compilation value or the live Host catalogue.
	for _, descriptions := range snapshot.Capabilities {
		descriptions[0].DisplayName = "NHA055_MUTATED_CAPTURE"
	}
	if admitted.SHA256() != restored.SHA256() {
		t.Fatal("snapshot mutation changed historical context")
	}
	discover(false)
	next, err := sequence.CaptureBounded(sequencer.MaxJournalBytes)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := configactivation.SourceContextFromSnapshot(next, manifest)
	if err != nil || changed.SHA256() == admitted.SHA256() {
		t.Fatal("real admitted capability change did not change compiler context", err)
	}
	if _, err := configactivation.SourceContextFromSnapshot(sequencer.Snapshot{}, manifest); err == nil {
		t.Fatal("detached registry was presented as an admitted projection")
	}
}
