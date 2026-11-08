package apiresources

import (
	"crypto/sha256"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"strings"
	"testing"
)

func TestNHA212ModulesKeepIndependentIdentityAccessAndDefinitionFence(t *testing.T) {
	d := light()
	d.GetLight().Rgb = true
	d.GetLight().Presets = &registry.ControlChoices{Choices: []*registry.ControlChoice{{Key: "scene/night:7"}}}
	config, rights := sha256.Sum256([]byte("synthetic")), sha256.Sum256([]byte("rights"))
	fence, err := NewDefinitionFence(1, config, strings.Repeat("a", 32), 1, "epoch", rights)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := ProjectCapabilities(home(t), entity(t, "module-lamp"), []*registry.CapabilityDescription{d}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var source Capability
	for _, c := range caps {
		if c.Trait() == Light {
			source = c
		}
	}
	bound, err := source.BindDefinition(fence)
	if err != nil {
		t.Fatal(err)
	}
	again, err := bound.BindDefinition(fence)
	if err != nil || again.Resource().Etag != bound.Resource().Etag {
		t.Fatal("derived module etag fed back into definition", err)
	}
	for _, access := range []api.ActionAccess{api.ActionAccess_ACTION_ACCESS_ALLOWED, api.ActionAccess_ACTION_ACCESS_DENIED} {
		current, err := bound.WithActionAccess(access)
		if err != nil {
			t.Fatal(err)
		}
		resource := current.Resource()
		if len(resource.Modules) < 3 {
			t.Fatal("missing functional modules")
		}
		for _, m := range resource.Modules {
			if m.Capability != resource.Name || m.Etag != resource.Etag || m.ActionAccess != access {
				t.Fatal("module borrowed identity/access", m)
			}
		}
	}
	preset, _ := apitypes.NewSelectorKey("scene/night:7")
	rgb := domain.RGB{Red: 0, Green: 137, Blue: 255}
	intent, err := apitypes.NewLightCommand(apitypes.LightInput{RGB: &rgb, Preset: &preset})
	if err != nil {
		t.Fatal(err)
	}
	args, err := NativeArguments(bound, intent)
	if err != nil {
		t.Fatal(err)
	}
	if v := args.GetSetLight(); v == nil || v.Rgb.GetGreen() != 137 || v.GetPreset() != "scene/night:7" {
		t.Fatal("atomic light intention split or dropped", args)
	}
	d.GetLight().Presets.Choices[0].Key = "replacement"
	changed, _ := ProjectCapabilities(home(t), entity(t, "module-lamp"), []*registry.CapabilityDescription{d}, nil)
	for _, c := range changed {
		if c.Trait() == Light {
			next, e := c.BindDefinition(fence)
			if e != nil || next.Resource().Etag == bound.Resource().Etag {
				t.Fatal("changed preset retained executable fence", e)
			}
			if _, e = NativeArguments(next, intent); e == nil {
				t.Fatal("old preset admitted under replacement source")
			}
		}
	}
}
