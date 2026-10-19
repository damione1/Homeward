package reactivetemplate

import (
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
	"time"
)

func TestNHA105SolarTemplatePreservesTypedAbsence(t *testing.T) {
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i] = missing
	}
	input.Next[3], _ = domain.SolarOccurrenceAt(time.Unix(100, 0))
	input.Next[2], _ = domain.SolarOccurrenceAt(time.Unix(200, 0))
	solar, _ := domain.NewSolarValue(input)
	value, _ := domain.NewSolarStateValue(solar)
	text, attrs, err := formatCurrent(value)
	if err != nil || text != "above_horizon" {
		t.Fatal("solar horizon projection changed")
	}
	fields := map[string]template.Value{}
	for _, attr := range attrs {
		fields[attr.Name] = attr.Value
	}
	if kind, _ := fields["next_dawn"].Kind(); kind != template.KindNone {
		t.Fatal("absent event invented a template timestamp")
	}
	if rising, ok := fields["rising"].Bool(); !ok || !rising {
		t.Fatal("present rising boolean lost")
	}
	if _, exists := fields["latitude"]; exists {
		t.Fatal("observer leaked into template")
	}
	event, _ := domain.NewSolarEventValue(domain.SolarDawn, missing)
	native, _ := domain.NewSolarEventStateValue(event)
	text, _, err = formatCurrent(native)
	if err != nil || text != "unknown" {
		t.Fatal("absent event lost projection semantics")
	}
}
