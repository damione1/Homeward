package derivedpower

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/sequencer"
)

func TestNHA135SourceReportModeRefusesTargetsAndMultipleReads(t *testing.T) {
	id, err := reactivetemplate.ParseProgramID("nha135.report-admission")
	if err != nil {
		t.Fatal(err)
	}
	target, err := domain.ParseEntityID("ent_nha135_internal_target")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name   string
		source string
		target *domain.EntityID
	}{
		{"internal target", "{{ state_attr('climate.home', 'hvac_action') }}", &target},
		{"multiple attributes", "{{ [state_attr('climate.home', 'hvac_action'), state_attr('climate.home', 'temperature')] }}", nil},
		{"state rather than activity", "{{ states('climate.home') }}", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			program, err := template.Compile(row.source)
			if err != nil {
				t.Fatal(err)
			}
			definition, err := reactivetemplate.NewDefinition(id, program, nil, row.target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := definition.WithSourceReports(); !errors.Is(err, reactivetemplate.ErrInvalidPlan) {
				t.Fatal("source report mode admitted an unsupported shape", err)
			}
		})
	}
	// Static dependencies deduplicate repeated access to the same attribute.
	// The executed trace must still refuse multiple reads before publication.
	program, err := template.Compile("{{ [state_attr('climate.home', 'hvac_action'), state_attr('climate.home', 'hvac_action')] }}")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := reactivetemplate.NewDefinition(id, program, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definition, err = definition.WithSourceReports()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
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
	service, err := reactivetemplate.Start(context.Background(), sequence, plan, reactivetemplate.RealClock{})
	if service != nil {
		_ = service.Close(context.Background())
		t.Fatal("multiple executed reads started a source-report publisher")
	}
	if !errors.Is(err, reactivetemplate.ErrInvalidPlan) {
		t.Fatal("multiple-read refusal", err)
	}
	if _, err := NewHVACSource(domain.EntityID{}); !errors.Is(err, ErrInvalidPower) {
		t.Fatal("zero source identity admitted", err)
	}
}
