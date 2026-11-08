package dashboard

import (
	"crypto/sha256"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/domain"
	"strings"
	"testing"
)

func TestNHA212FamiliesDoNotGrantOrMergeSourceModules(t *testing.T) {
	options := apiFixtureProjection(t)
	original := options.Capabilities
	config, rights := sha256.Sum256([]byte("synthetic")), sha256.Sum256([]byte("rights"))
	fence, err := apiresources.NewDefinitionFence(1, config, strings.Repeat("b", 32), 2, "epoch", rights)
	if err != nil {
		t.Fatal(err)
	}
	options.Capabilities = func(alias domain.HAAlias) []apiresources.Capability {
		values := original(alias)
		text, _ := alias.Value()
		access := api.ActionAccess_ACTION_ACCESS_ALLOWED
		if text == "input_boolean.synthetic" {
			access = api.ActionAccess_ACTION_ACCESS_DENIED
		}
		for i, c := range values {
			bound, e := c.BindDefinition(fence)
			if e != nil {
				t.Fatal(e)
			}
			bound, e = bound.WithActionAccess(access)
			if e != nil {
				t.Fatal(e)
			}
			values[i] = bound
		}
		return values
	}
	principal, _ := domain.ParseHAAlias("light.synthetic")
	secondary, _ := domain.ParseHAAlias("input_boolean.synthetic")
	p := apiProjector{options: options}
	var name string
	for _, b := range p.bindings(principal) {
		if b.native.Trait() == apiresources.Light {
			name = b.resource.Name
		}
	}
	for _, family := range []string{"binary", "continuous_level", "stepped_level", "blind", "garage"} {
		card := Card{entity: principal, visualFamily: family, controlModules: []sourceControlModule{{entity: secondary, role: "power"}}}
		actual, modules, err := p.controlComposition(card, name)
		if err != nil || actual != visualFamily(family) {
			t.Fatal(family, err)
		}
		if len(modules) != 3 {
			t.Fatalf("family changed functions: %s %d", family, len(modules))
		}
		for i, m := range modules {
			if m.Etag == "" {
				t.Fatal("missing source fence")
			}
			if i < 2 && (m.Capability != name || m.ActionAccess != api.ActionAccess_ACTION_ACCESS_ALLOWED) {
				t.Fatal("principal binding changed")
			}
			if i == 2 && (m.Capability == name || m.ActionAccess != api.ActionAccess_ACTION_ACCESS_DENIED) {
				t.Fatal("secondary borrowed principal permission")
			}
			if m.Role == api.ControlModuleRole_CONTROL_MODULE_ROLE_MOTION {
				t.Fatal("garage style manufactured motion command")
			}
		}
	}
	options.Capabilities = func(alias domain.HAAlias) []apiresources.Capability {
		if alias == secondary {
			return nil
		}
		return original(alias)
	}
	p.options = options
	if _, _, err = p.controlComposition(Card{entity: principal, controlModules: []sourceControlModule{{entity: secondary, role: "power"}}}, name); err == nil {
		t.Fatal("unreadable optional source borrowed principal")
	}
}
