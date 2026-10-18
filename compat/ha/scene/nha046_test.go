package scene_test

import (
	"crypto/sha256"
	"io/fs"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/scope"
)

func TestNHA046PinnedTwelveSceneSources(t *testing.T) {
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	wantMembers := map[string]int{
		"scene.ceilling_fan_day": 1, "scene.ceilling_fan_night": 1,
		"scene.climate_day_temperature": 2, "scene.climate_night_temperature": 2,
		"scene.cosy": 5, "scene.kitchen_cooking_mode": 3,
		"scene.kitchen_cosy_mode": 3, "scene.living_room_daylight": 5,
		"scene.living_room_in_between": 5, "scene.night_mode": 3,
		"scene.turn_off_living_room": 5, "scene.turn_off_main_floor": 10,
	}
	seen := make(map[string]bool, len(wantMembers))
	ctMembers := 0
	for _, selected := range selection.Retained() {
		alias, _ := selected.ID().Value()
		expected, isScene := wantMembers[alias]
		if !isScene {
			continue
		}
		path := selected.Path()
		if path != "scope/logic_mvp_sources/"+alias+".yaml" {
			t.Fatalf("%s source path: %q", alias, path)
		}
		data, err := fs.ReadFile(scope.LogicMVPSourcesV1(), strings.TrimPrefix(path, "scope/"))
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(data) != selected.Digest() {
			t.Fatalf("%s source digest", alias)
		}
		ref, err := scene.NewSourceRef(selected.ID(), path, selected.Digest())
		if err != nil {
			t.Fatal(err)
		}
		result, err := scene.Load(ref, data)
		if err != nil || len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
			t.Fatalf("%s compile: result=%+v err=%v", alias, result, err)
		}
		definition := result.Accepted()[0]
		if definition.ID() != selected.ID() || definition.Source().Digest() != selected.Digest() ||
			len(definition.Members()) != expected {
			t.Fatalf("%s identity or member count", alias)
		}
		for _, member := range definition.Members() {
			if member.Source().File != path || member.Source().Line == 0 || len(member.Diagnostics()) != 0 {
				t.Fatalf("%s member %v lost position or acquired diagnostic: %+v", alias, member.Alias(), member.Diagnostics())
			}
			if light, ok := member.Light(); ok {
				_, _, ct := light.Components()
				if ct != nil {
					ctMembers++
				}
			}
		}
		seen[alias] = true
		if alias == "scene.ceilling_fan_day" || alias == "scene.ceilling_fan_night" {
			fan, ok := definition.Members()[0].Fan()
			if !ok || !fan.On() || fan.Percentage() != 75 {
				t.Fatalf("%s lost 75%% fan", alias)
			}
		}
		if alias == "scene.turn_off_main_floor" {
			member := findMember(t, definition, "light.kitchen_lights")
			light, ok := member.Light()
			on, brightness, ct := light.Components()
			if !ok || !on || brightness == nil || *brightness != 3 || ct != nil {
				t.Fatal("turn_off_main_floor must retain kitchen lights ON brightness 3")
			}
		}
		if alias == "scene.cosy" {
			assertCT(t, definition, "light.library_lamp", 89, 357)
		}
		if alias == "scene.living_room_daylight" {
			assertCT(t, definition, "light.library_lamp", 255, 250)
			assertCT(t, definition, "light.table_lamp", 255, 250)
		}
		if alias == "scene.living_room_in_between" {
			assertCT(t, definition, "light.table_lamp", 76, 383)
			assertCT(t, definition, "light.library_lamp", 153, 357)
		}
		if alias == "scene.climate_night_temperature" {
			home, ok := findMember(t, definition, "climate.home").Climate()
			mode, temperature, humidity, fanMode, preset := home.Components()
			if !ok || mode != domain.ClimateModeOff || temperature != nil || humidity == nil || *humidity != 40 ||
				fanMode == nil || *fanMode != "auto" || preset != nil {
				t.Fatal("climate.home OFF lost operative humidity/fan mode")
			}
			neviweb, ok := findMember(t, definition, "climate.neviweb130_climate_living_room").Climate()
			_, temperature, _, _, preset = neviweb.Components()
			if !ok || temperature == nil || *temperature != 16 || preset == nil || *preset != "none" {
				t.Fatal("Neviweb preset or setpoint lost")
			}
		}
	}
	if len(seen) != 12 || ctMembers != 5 {
		t.Fatalf("source coverage: %d scenes / %d CT members", len(seen), ctMembers)
	}
}

func findMember(t *testing.T, definition scene.Definition, name string) scene.Member {
	t.Helper()
	alias, err := domain.ParseHAAlias(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range definition.Members() {
		if member.Alias() == alias {
			return member
		}
	}
	t.Fatalf("missing %s", name)
	return scene.Member{}
}
func assertCT(t *testing.T, definition scene.Definition, alias string, wantBrightness uint8, wantCT uint32) {
	t.Helper()
	light, ok := findMember(t, definition, alias).Light()
	on, brightness, ct := light.Components()
	if !ok || !on || brightness == nil || *brightness != wantBrightness || ct == nil || *ct != wantCT {
		t.Fatalf("%s: b=%v CT=%v", alias, brightness, ct)
	}
}

func TestNHA046MemberRefusalAndCopies(t *testing.T) {
	source := []byte("id: '1'\nname: Test\nentities:\n  light.a:\n    state: 'on'\n    brightness: 0\n    effect: pulse\n  switch.b:\n    state: 'off'\n    unknown_effect: blink\nmetadata: {}\n")
	alias, _ := domain.ParseHAAlias("scene.test")
	ref, err := scene.NewSourceRef(alias, "scene.test.yaml", sha256.Sum256(source))
	if err != nil {
		t.Fatal(err)
	}
	result, err := scene.Load(ref, source)
	if err != nil || len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("fixture compile: %v %+v", err, result)
	}
	members := result.Accepted()[0].Members()
	light, _ := members[0].Light()
	on, brightness, _ := light.Components()
	if on || brightness != nil || len(members[0].Diagnostics()) != 0 {
		t.Fatal("HA service brightness zero must become OFF without requiring ON attributes")
	}
	if len(members[1].Diagnostics()) != 1 || members[1].Diagnostics()[0].Code() != scene.CodeUnknownField ||
		members[1].Diagnostics()[0].Field() != "unknown_effect" {
		t.Fatal("unknown operative field was silently discarded")
	}
	members[1] = scene.Member{}
	if len(result.Accepted()[0].Members()[1].Diagnostics()) != 1 {
		t.Fatal("member result was mutable")
	}
	broken := append([]byte(nil), source...)
	broken[len(broken)-1] = 'x'
	if _, err := scene.Load(ref, broken); err == nil {
		t.Fatal("source digest mismatch admitted")
	}
}
