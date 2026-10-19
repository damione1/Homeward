package ir_test

import (
	"fmt"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
)

func TestNHA041ParallelModeAndExistingMaxProfile(t *testing.T) {
	for _, test := range []struct {
		mode     string
		max      int
		accepted bool
	}{
		{"parallel", 1, true},
		{"parallel", 10000, true},
		{"unknown", 1, false},
	} {
		t.Run(fmt.Sprintf("%s-%d", test.mode, test.max), func(t *testing.T) {
			body := fmt.Sprintf("id: nha041_ir\nmode: %s\nmax: %d\ntriggers:\n- trigger: time_pattern\n  seconds: '*'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.synthetic}\n  data: {}\n", test.mode, test.max)
			file, err := parser.NewSourceFile("nha041-ir.yaml", []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			result, err := ir.CompileSources("automation.nha041_ir", "nha041-ir.yaml", []parser.SourceFile{file}, nha039Catalogue(t))
			if err != nil {
				t.Fatal(err)
			}
			if !test.accepted {
				if len(result.Accepted()) != 0 || len(result.Rejected()) != 1 {
					t.Fatalf("unknown mode admitted: accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
				}
				return
			}
			if len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
				t.Fatalf("valid mode/max rejected: accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
			}
			automation := result.Accepted()[0]
			maximum, present := automation.Max()
			if automation.Mode() != ir.AutomationParallel || !present || maximum != test.max {
				t.Fatalf("compiled mode/max changed: mode=%v max=%d present=%v", automation.Mode(), maximum, present)
			}
		})
	}
}
