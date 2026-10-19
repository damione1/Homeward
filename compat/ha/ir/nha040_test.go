package ir_test

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
)

func nha040Catalogue(t *testing.T) ir.Catalogue {
	t.Helper()
	entities := []ir.Entity{
		nha039Entity(t, "switch.synthetic", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn),
		nha039Entity(t, "sensor.temperature", ir.ReferencePresent, ir.SupportObserveState),
		nha039Entity(t, "light.fixture", ir.ReferencePresent, ir.SupportObserveState),
		nha039Entity(t, "calendar.fixture", ir.ReferencePresent),
	}
	catalogue, err := ir.NewCatalogue(entities, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func nha040Compile(t *testing.T, triggerYAML string) ir.Result {
	t.Helper()
	body := "id: fixture\ntriggers:\n" + triggerYAML + "\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n"
	file, err := parser.NewSourceFile("fixture.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileSources("automation.fixture", "fixture.yaml", []parser.SourceFile{file}, nha040Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHA040IRAdmitsAndRejectsTriggerVariants(t *testing.T) {
	result := nha040Compile(t, "- trigger: state\n  entity_id: switch.synthetic\n  from: 'off'\n  to: 'on'\n  for: '00:00:30'\n- trigger: numeric_state\n  entity_id: sensor.temperature\n  above: 0\n  below: 10\n- trigger: time_pattern\n  seconds: '/1'\n- trigger: calendar\n  entity_id: calendar.fixture\n  event: start\n  offset: '-01:00:00'\n- trigger: time\n  at: '09:00:00'\n- trigger: sun\n  event: sunset\n")
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("admission: accepted=%d rejected=%v", len(result.Accepted()), result.Rejected())
	}
	triggers := result.Accepted()[0].Triggers()
	want := []ir.TriggerKind{ir.TriggerState, ir.TriggerNumericState, ir.TriggerTimePattern, ir.TriggerCalendar, ir.TriggerTime, ir.TriggerSunset}
	for i, trigger := range triggers {
		if trigger.Kind() != want[i] || trigger.Key() != "position:"+string(rune('0'+i)) {
			t.Fatalf("trigger %d: kind=%v key=%q", i, trigger.Kind(), trigger.Key())
		}
	}
	if duration, ok := triggers[0].For(); !ok || duration != 30*time.Second {
		t.Fatalf("for=%v present=%t", duration, ok)
	}
	if above, ok := triggers[1].Above(); !ok || above != 0 {
		t.Fatalf("zero threshold lost: %v %t", above, ok)
	}
	h, m, s := triggers[2].Pattern()
	if h.Kind() != ir.PatternPartAny || m.Kind() != ir.PatternPartAny || s.Kind() != ir.PatternPartEvery || s.Value() != 1 {
		t.Fatalf("pattern=%v/%v/%v", h, m, s)
	}
	if triggers[3].CalendarEdge() != ir.CalendarEdgeStart || triggers[3].OffsetSeconds() != -3600 {
		t.Fatal("calendar edge or offset lost")
	}

	for _, test := range []struct{ name, yaml, field string }{
		{"attribute", "- trigger: state\n  entity_id: switch.synthetic\n  attribute: arbitrary", "attribute"},
		{"numeric-order", "- trigger: numeric_state\n  entity_id: sensor.temperature\n  above: 10\n  below: 0", "below"},
		{"pattern-zero", "- trigger: time_pattern\n  seconds: '/0'", "seconds"},
		{"calendar-edge", "- trigger: calendar\n  entity_id: calendar.fixture\n  event: middle", "event"},
		{"unknown-field", "- trigger: state\n  entity_id: switch.synthetic\n  value_template: '{{states()}}'", "value_template"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := nha040Compile(t, test.yaml)
			if len(result.Accepted()) != 0 || len(result.Rejected()) != 1 {
				t.Fatalf("unexpected admission: %+v", result)
			}
			d := result.Rejected()[0]
			if d.Field() != test.field || d.Source().At().File != "fixture.yaml" || d.Source().At().Line == 0 {
				t.Fatalf("diagnostic field/position: %q %+v", d.Field(), d.Source().At())
			}
		})
	}
}
