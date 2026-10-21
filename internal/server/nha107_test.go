package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/derivedpower"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha107OutputRecorder struct{ ids []reactivetemplate.ProgramID }

func (r *nha107OutputRecorder) PublishReactiveTemplate(_ context.Context, _ *integrations.Manager, update reactivetemplate.Update) error {
	r.ids = append(r.ids, update.ID())
	return nil
}

func TestNHA107SharesOneTemplateEngineWithPermanentAndWait(t *testing.T) {
	legacyID, _ := reactivetemplate.ParseProgramID("legacy-permanent")
	legacyProgram, err := template.Compile("{{ 'kept' }}")
	if err != nil {
		t.Fatal(err)
	}
	legacyDefinition, err := reactivetemplate.NewDefinition(legacyID, legacyProgram, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{legacyDefinition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	prior := &nha107OutputRecorder{}
	derived := &nha107OutputRecorder{}
	clock := &reactivetemplate.RealClock{}
	oldComposition, err := NewReactiveTemplateComposition(legacyPlan, prior, clock)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := domain.ParseEntityID("ent_nha107_merged_ups")
	derivedDefinition, err := derivedpower.Definition(derivedpower.KindUPS, output)
	if err != nil {
		t.Fatal(err)
	}
	derivedPlan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{derivedDefinition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := composeDerivedReactiveTemplates(oldComposition, derivedPlan, derived)
	if err != nil || merged.clock != clock || len(merged.plan.ProgramIDs()) != 2 {
		t.Fatalf("merge: %v", err)
	}
	withWaits, err := merged.plan.WithScriptWaits(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	service, err := reactivetemplate.Start(context.Background(), sequence, withWaits, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	for range 2 {
		select {
		case update := <-service.Updates():
			if err := merged.output.PublishReactiveTemplate(context.Background(), nil, update); err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("merged permanent output timeout")
		}
	}
	if len(prior.ids) != 1 || prior.ids[0] != legacyID || len(derived.ids) != 1 || derived.ids[0] != derivedPlan.ProgramIDs()[0] {
		t.Fatalf("permanent routes: prior=%v derived=%v", prior.ids, derived.ids)
	}
	waitProgram, err := template.Compile("{{ true }}")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := service.ObserveTemplate(context.Background(), waitProgram, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Ready():
		if err := handle.Outcome(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("script wait was lost by merged permanent plan")
	}
	if err := handle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNHA107RunComposesThreePowerOutputsAndMissingHVACEnergy(t *testing.T) {
	cluster := testpg.Start(t)
	configuration := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nderived_power: mvp-v1\nenergy:\n  - source: sensor.hvac_power_consumption\n    output: sensor.hvac_energy_consumption\n    precision: 3\n    max_gap: 1h\n    time_basis: receipt\n", shortPrivateDataDir(t), cluster.Port))
	startup := nha029RunStartupFromRaw(t, configuration)
	sequenceReady := make(chan *sequencer.Sequencer, 1)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() {
		done <- runWithDependencies(ctx, startup, listener.Listen, func(net.Addr) { close(ready) }, nil, runDependencies{
			observe: func(_ context.Context, sequence *sequencer.Sequencer) error { sequenceReady <- sequence; return nil },
		})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("Run did not join derived and energy owners")
		}
	}()
	var sequence *sequencer.Sequencer
	select {
	case sequence = <-sequenceReady:
	case err := <-done:
		t.Fatalf("Run failed before projection: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("projection composition timeout")
	}
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run failed before readiness: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run readiness timeout")
	}
	wanted := map[string]bool{
		"sensor.ups_power_consumption":    false,
		"sensor.plancher_cuisine_power_2": false,
		"sensor.hvac_power_consumption":   false,
		"sensor.hvac_energy_consumption":  false,
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		snapshot, err := sequence.Capture()
		if err != nil {
			t.Fatal(err)
		}
		for entity, record := range snapshot.Registry {
			original, _, _, err := record.Aliases()
			if err != nil {
				t.Fatal(err)
			}
			alias, err := original.Value()
			if err != nil {
				t.Fatal(err)
			}
			if alias == "climate.home" {
				t.Fatal("excluded Ecobee source was configured")
			}
			if _, tracked := wanted[alias]; !tracked {
				continue
			}
			state, exists := snapshot.States[entity]
			if !exists {
				continue
			}
			_, quality, _, _, _, err := state.Components()
			if err != nil {
				t.Fatal(err)
			}
			availability, freshness, provenance, err := quality.Components()
			if err != nil || availability != domain.AvailabilityUnavailable || freshness != domain.FreshnessStale || provenance != domain.ProvenanceEstimated {
				t.Fatalf("%s initial missing source quality: %v/%v/%v err=%v", alias, availability, freshness, provenance, err)
			}
			if _, present := state.CurrentValue(); present {
				t.Fatalf("%s fabricated a current value", alias)
			}
			if _, present := state.DisplayValue(); present {
				t.Fatalf("%s fabricated a previous value", alias)
			}
			wanted[alias] = true
		}
		complete := true
		for _, seen := range wanted {
			complete = complete && seen
		}
		if complete {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("Run stopped before all outputs: %v", err)
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("missing composed output reports: %v", wanted)
		}
	}
}
