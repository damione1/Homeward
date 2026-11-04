package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sonos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Capture the real native pilot's declarations without executing its held task.
// This fixture never polls, sends a command or reproduces a descriptor by hand.
func nha197SonosDescriptions(t *testing.T) []*wireregistry.CapabilityDescription {
	t.Helper()
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newNativeSonosRuntime(catalog)
	if err != nil {
		t.Fatal(err)
	}
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := sonos.ParseSettings(sonos.Input{Instance: "nha197-native-fixture", Players: []sonos.PlayerInput{{Address: "192.168.50.10", UID: "RINCON_00112233445501400", Alias: "media_player.demo", Label: "Demo"}}})
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := settings.Instance()
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := runtime.Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	pilot, tasks := factory(), &nha195HeldTasks{}
	if err := pilot.Start(context.Background(), session, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pilot.Stop(context.Background()); _ = host.Revoke(session) })
	resolved, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{settings.Players()[0].SourceKey()})
	if err != nil || len(resolved) != 1 {
		t.Fatalf("native discovery: %v", err)
	}
	descriptions, err := host.Capabilities(resolved[0].Entity)
	if err != nil || len(descriptions) != 2 || len(tasks.tasks) != 1 {
		t.Fatalf("native declarations missing: %v", err)
	}
	return descriptions
}

func TestNHA197HistoricalNativeCodecPreservesClosedAdmission(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	identity, _ := nha055HistoricalIdentity(t, "media_player.garage", false)
	var media *wireregistry.CapabilityDescription
	for _, description := range nha197SonosDescriptions(t) {
		if description.SchemaId == sonos.Schema {
			media = description
		}
	}
	if media == nil || media.GetExtension().GetActionMessage() != "home.media.v1.MediaAction" {
		t.Fatal("real native action extension absent")
	}
	input := nha055HistoricalContext(t, manifest, identity, []*wireregistry.CapabilityDescription{media})
	catalogue, records, err := historicalSourceCompilationInputs(context.Background(), input, manifest)
	if err != nil || !catalogue.Valid() || len(records) != 1 {
		t.Fatalf("known native codec was lost during detached compilation: %v", err)
	}
	for _, entity := range catalogue.Entities() {
		if len(entity.Supports()) != 0 {
			t.Fatal("native extension manufactured HA IR observation or command support")
		}
	}
	unknown := proto.Clone(media).(*wireregistry.CapabilityDescription)
	unknown.SchemaId = "fixture.media.v1"
	changed := proto.Clone(media).(*wireregistry.CapabilityDescription)
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(changed.DescriptorSet, set); err != nil || len(set.File) == 0 {
		t.Fatalf("native descriptor fixture: %v", err)
	}
	if set.File[0].Options == nil {
		set.File[0].Options = &descriptorpb.FileOptions{}
	}
	set.File[0].Options.GoPackage = proto.String("example.invalid/nha197/unknown")
	changed.DescriptorSet, err = proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(changed.DescriptorSet)
	changed.DescriptorSha256 = digest[:]
	for _, test := range []struct {
		name        string
		description *wireregistry.CapabilityDescription
	}{{"unknown schema", unknown}, {"different descriptor", changed}} {
		t.Run(test.name, func(t *testing.T) {
			input := nha055HistoricalContext(t, manifest, identity, []*wireregistry.CapabilityDescription{test.description})
			if _, records, err := historicalSourceCompilationInputs(context.Background(), input, manifest); !errors.Is(err, contracts.ErrUnknownExtensionCodec) || !errors.Is(err, ir.ErrInvalidCatalogue) || records != nil {
				t.Fatalf("unregistered codec admitted or failure cause hidden: %v", err)
			}
		})
	}
}

func TestNHA197ActualRunTLSAppliesDashboardWithNativeSonosCodecAndRestarts(t *testing.T) {
	f := nha055PublicationStart(t)
	repository, ok := f.actual.identity.(*registry.Repository)
	if !ok {
		t.Fatal("actual Run registry absent")
	}
	instance, err := domain.ParseIntegrationInstanceID("nha197-live-discovery")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("nha197-synthetic-player", "player")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := repository.ConfigureBinding(f.ctx, instance, key, "media_player.garage")
	if err != nil {
		t.Fatal(err)
	}
	host := f.actual.host
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Revoke(session) })
	candidate, err := integrations.NewCandidate(key, nha197SonosDescriptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(f.ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatalf("default Run did not admit its native codec: %v", err)
	}
	before := f.read(t)
	raw := []byte("views: []\n")
	dashboard := &planwire.ConfigurationPlanCandidate{Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.nha197_native", Entry: "dashboard.yaml", Files: []*planwire.ConfigurationPlanSourceFile{{LogicalPath: "dashboard.yaml", ExactSource: raw}}}
	plan := f.build(t, dashboard)
	if plan.State != planwire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED {
		t.Fatal("dashboard preview rejected")
	}
	request := nha055Request(before, dashboard, plan)
	request.Sources = []*wire.SourcePrecondition{nha055PublicationExpected("nha197.dashboard", "dashboard.yaml", nil, nil)}
	applied, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !applied.Msg.Done {
		t.Fatalf("dashboard Apply lost the already-admitted native codec: %v", err)
	}
	appliedMetadata := nha203PublishedMetadata(t, applied.Msg)
	if appliedMetadata.GenerationRevision != before.ActiveGeneration.Revision+1 || host.CommandDriverEntries() != 0 {
		t.Fatal("dashboard did not publish exactly one generation without commands")
	}
	read, err := f.api.GetConfigurationDocument(f.ctx, nha056SessionRequest(&configuration.GetConfigurationDocumentRequest{Name: nha203DocumentName(t, f.ctx, f.run, "nha197.dashboard")}, f.token))
	if err != nil || read.Msg.Version.Revision != 1 {
		t.Fatalf("published source missing: %v", err)
	}
	version := proto.Clone(read.Msg.Version).(*configuration.DocumentVersion)
	if err := f.run.join(t); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.startRun(t)
	f.login(t)
	read, err = f.api.GetConfigurationDocument(f.ctx, nha056SessionRequest(&configuration.GetConfigurationDocumentRequest{Name: nha203DocumentName(t, f.ctx, f.run, "nha197.dashboard")}, f.token))
	if err != nil || !proto.Equal(version, read.Msg.Version) || read.Msg.ActiveGeneration.Revision != appliedMetadata.GenerationRevision {
		t.Fatalf("cold replay lost the native codec or rewrote source history: %v", err)
	}
	if declarations, err := f.actual.host.Capabilities(identity.Entity); err != nil || len(declarations) != 0 || f.actual.host.CommandDriverEntries() != 0 {
		t.Fatalf("historical native declarations became live authority or commands: %v", err)
	}
	// Keep the fixture's operational document unchanged across both runs.
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(f.raw, exact) {
		t.Fatal("dashboard Apply changed operational source bytes")
	}
}
