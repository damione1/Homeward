package configsimulation_test

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/configsimulation"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/scope"
)

// This corpus probe records the exact 17 retained sources, including the two
// deliberately unsupported scenarios. It does not activate their definitions.
func TestNHA054RetainedSeventeenCoverageIsExplicit(t *testing.T) {
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	aliases := regexp.MustCompile(`(?:light|switch|fan|climate|input_boolean|binary_sensor)\.[a-z0-9_]+`)
	scenes, automations, unsupported := 0, 0, 0
	for _, selected := range selection.Retained() {
		name, err := selected.ID().Value()
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			raw, err := fs.ReadFile(scope.LogicMVPSourcesV1(), strings.TrimPrefix(selected.Path(), "scope/"))
			if err != nil || sha256.Sum256(raw) != selected.Digest() {
				t.Fatal("pinned source changed", err)
			}
			file, err := parser.NewSourceFile(selected.Path(), raw)
			if err != nil {
				t.Fatal(err)
			}
			family := configplans.Automation
			if strings.HasPrefix(name, "scene.") {
				family = configplans.Scene
			}
			candidate, err := configplans.NewCandidate(family, name, selected.Path(), []parser.SourceFile{file})
			if err != nil {
				t.Fatal(err)
			}
			fixtures := []configsimulation.Fixture{}
			seen := map[string]bool{}
			state := "on"
			// Test fixtures declare semantic capabilities explicitly; they do
			// not attest production routes or capabilities for these aliases.
			for _, alias := range aliases.FindAllString(string(raw), -1) {
				if seen[alias] {
					continue
				}
				seen[alias] = true
				fixture, err := configsimulation.NewFixture(alias, &state, nil, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportClimateSetpoint, ir.SupportSwitchTurnOn, ir.SupportLightTurnOn, ir.SupportLightTurnOff, ir.SupportLightBrightnessAbsolute, ir.SupportLightBrightnessRelative, ir.SupportLightTransition, ir.SupportSceneTurnOn, ir.SupportSceneTransition}, false)
				if err != nil {
					t.Fatal(err)
				}
				fixtures = append(fixtures, fixture)
			}
			snapshot, err := configsimulation.NewSnapshot(fixtures)
			if err != nil {
				t.Fatal(err)
			}
			hour := 9
			if name == "automation.turn_off_basement_climate_the_evening" {
				hour = 20
			}
			now := time.Date(2026, 10, 22, hour, 0, 0, 0, time.UTC).Add(-2 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			report, err := configsimulation.Run(ctx, configsimulation.Input{Candidate: candidate, Snapshot: snapshot, Now: now, Zone: time.UTC, VirtualLimit: time.Minute})
			if err != nil || !report.Simulated() || report.HardwareValidated() || len(report.Limitations()) == 0 {
				t.Fatal("invalid coverage claim", err)
			}
			switch name {
			case "automation.ikea_kitchen_lights_remote", "automation.turn_on_lights_at_sunset":
				if report.Status() != configsimulation.Failed || len(report.Intents()) != 0 || len(report.Diagnostics()) == 0 {
					t.Fatal("unsupported trigger/blueprint claimed covered")
				}
				unsupported++
				t.Logf("explicitly unsupported, zero intentions: %s diagnostics=%v", name, report.Diagnostics())
			default:
				if report.Status() != configsimulation.Completed || len(report.Intents()) == 0 || len(report.Diagnostics()) != 0 {
					t.Fatalf("retained fixture unexpectedly lost coverage: %s status=%v diagnostics=%v", name, report.Status(), report.Diagnostics())
				}
				if family == configplans.Scene {
					scenes++
				} else {
					automations++
				}
				t.Logf("fixture intentions recorded: %s count=%d; no physical validation", name, len(report.Intents()))
			}
		})
	}
	if scenes != 12 || automations != 3 || unsupported != 2 {
		t.Fatalf("coverage changed: scenes=%d timed automations=%d explicitly unsupported=%d", scenes, automations, unsupported)
	}
}
