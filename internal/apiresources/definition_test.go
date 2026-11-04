package apiresources

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestDefinitionETagIgnoresObservationsButBindsDefinitionAndNativeFences(t *testing.T) {
	config, rights := sha256.Sum256([]byte("synthetic-config")), sha256.Sum256([]byte("synthetic-rights"))
	fence, err := NewDefinitionFence(1, config, strings.Repeat("a", 32), 4, "synthetic-admission-epoch", rights)
	if err != nil {
		t.Fatal(err)
	}
	id := entity(t, "synthetic-definition-lamp")
	project := func(on bool, description *registry.CapabilityDescription) Capability {
		t.Helper()
		value := domain.NewSwitchValue(on)
		observed := observation(t, id, &value, domain.AvailabilityAvailable)
		caps, err := ProjectCapabilities(home(t), id, []*registry.CapabilityDescription{description}, &observed)
		if err != nil || len(caps) != 1 {
			t.Fatal(err)
		}
		return caps[0]
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "switch", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
	initial := project(false, description)
	first, err := initial.BindDefinition(fence)
	if err != nil {
		t.Fatal(err)
	}
	second, err := project(true, description).BindDefinition(fence)
	if err != nil || first.Resource().Etag != second.Resource().Etag {
		t.Fatal("observations changed the definition etag", err)
	}
	if !strings.HasPrefix(first.Resource().Etag, `W/"`) || initial.Resource().Etag != "" {
		t.Fatal("weak etag syntax or immutable input violated")
	}
	description.GetSwitch().SetOn = false
	changed, err := project(false, description).BindDefinition(fence)
	if err != nil || first.Resource().Etag == changed.Resource().Etag {
		t.Fatal("definition change retained the old token", err)
	}
	for _, kind := range []string{"config", "catalogue", "epoch", "authorization"} {
		t.Run(kind, func(t *testing.T) {
			revision, position, epoch, binding := uint64(1), uint64(4), "synthetic-admission-epoch", rights
			switch kind {
			case "config":
				revision++
			case "catalogue":
				position++
			case "epoch":
				epoch = "next-synthetic-epoch"
			case "authorization":
				binding = sha256.Sum256([]byte("next-synthetic-rights"))
			}
			next, err := NewDefinitionFence(revision, config, strings.Repeat("a", 32), position, epoch, binding)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := initial.BindDefinition(next)
			if err != nil || first.Resource().Etag == bound.Resource().Etag {
				t.Fatal("native definition fence ignored", err)
			}
		})
	}
	if _, err := initial.BindDefinition(DefinitionFence{}); err == nil {
		t.Fatal("zero fence admitted")
	}
}
