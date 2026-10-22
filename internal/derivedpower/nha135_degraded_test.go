package derivedpower

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/sequencer"
)

func TestNHA135InitialBudgetDegradationKeepsIndependentProgramAndNoPower(t *testing.T) {
	// Like NHA-044's graph-depth oracle, build 32 internal producer nodes.
	// The real HVAC definition reads the last node at depth 33 and must be
	// degraded before any source read, while the independent program renders.
	sequence, err := sequencer.New(sequencer.RealClock{}, 128)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha135-depth-fixture")
	if err != nil {
		t.Fatal(err)
	}
	records := make([]integrations.RegistryRecord, 0, 32)
	definitions := make([]reactivetemplate.Definition, 0, 34)
	previousAlias := ""
	for index := 0; index < 32; index++ {
		name := fmt.Sprintf("depth_%02d", index)
		device, err := domain.ParseDeviceID("device_" + name)
		if err != nil {
			t.Fatal(err)
		}
		entity, err := domain.ParseEntityID("entity_" + name)
		if err != nil {
			t.Fatal(err)
		}
		key, err := integrations.NewSourceKey("fixture", name)
		if err != nil {
			t.Fatal(err)
		}
		aliasName := "sensor." + name
		if index == 31 {
			aliasName = "climate.home"
		}
		alias, err := domain.ParseHAAlias(aliasName)
		if err != nil {
			t.Fatal(err)
		}
		record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		source := "{{ 'seed' }}"
		if index != 0 {
			source = fmt.Sprintf("{{ states('%s') }}", previousAlias)
		}
		program, err := template.Compile(source)
		if err != nil {
			t.Fatal(err)
		}
		id, err := reactivetemplate.ParseProgramID(name)
		if err != nil {
			t.Fatal(err)
		}
		definition, err := reactivetemplate.NewDefinition(id, program, nil, &entity)
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
		previousAlias = aliasName
	}
	if err := sequence.PublishRegistry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	target, err := domain.ParseEntityID("ent_nha135_degraded_hvac_power")
	if err != nil {
		t.Fatal(err)
	}
	hvac, err := Definition(KindHVAC, target)
	if err != nil {
		t.Fatal(err)
	}
	program, err := template.Compile("{{ 'independent healthy' }}")
	if err != nil {
		t.Fatal(err)
	}
	id, err := reactivetemplate.ParseProgramID("nha135.independent")
	if err != nil {
		t.Fatal(err)
	}
	independent, err := reactivetemplate.NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions = append(definitions, hvac, independent)
	plan, err := reactivetemplate.NewPlan(definitions, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := reactivetemplate.Start(context.Background(), sequence, plan, reactivetemplate.RealClock{})
	if err != nil {
		t.Fatalf("isolated initial degradation refused Start: %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	seenHVAC, seenIndependent := false, false
	deadline := time.After(2 * time.Second)
	for !seenHVAC || !seenIndependent {
		select {
		case update, open := <-service.Updates():
			if !open {
				t.Fatal("isolated initial degradation closed the service")
			}
			switch update.ID() {
			case hvac.ID():
				if update.Diagnostic() != reactivetemplate.DiagnosticWorkBudget || len(update.Reads()) != 0 {
					t.Fatal("fixture did not degrade before its first read")
				}
				if _, current := update.CurrentResult(); current {
					t.Fatal("degraded HVAC has a current result")
				}
				projection, err := Project(KindHVAC, update)
				if err != nil || projection.Status() != StatusMissingSource {
					t.Fatalf("degraded HVAC projection: %v %v", projection.Status(), err)
				}
				if _, present := projection.Watts(); present {
					t.Fatal("degraded HVAC fabricated estimated W")
				}
				seenHVAC = true
			case independent.ID():
				result, current := update.CurrentResult()
				if !current || update.Diagnostic() != reactivetemplate.DiagnosticHealthy || result.Text() != "independent healthy" {
					t.Fatal("independent program lost its healthy result")
				}
				seenIndependent = true
			}
		case <-deadline:
			t.Fatal("initial degraded and independent publications did not arrive")
		}
	}
}
