package nha045

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationruns"
)

// The Controller really refuses the invocation. Only the presentation catalogue
// is changed afterwards: a capture must retain the native refusal, not infer it
// again from that mutable presentation. This test opens no network/container.
func TestNHA045NativeRefusalSurvivesDifferentPresentationMode(t *testing.T) {
	all, e := Corpus()
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name      string
		fixture   int
		run, mode string
		decision  automationruns.Decision
		outcome   Outcome
	}{
		{"single_busy_presented_queued", 2, "b", "queued", automationruns.DecisionSingleBusy, SingleBusy},
		{"max_exceeded_presented_single", 1, "c", "single", automationruns.DecisionMaxExceeded, MaxExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, e := buildRig(ctx, all[tc.fixture])
			if e != nil {
				t.Fatal(e)
			}
			defer func() {
				if e := r.dispose(); e != nil {
					t.Error(e)
				}
			}()
			for _, p := range r.f.phases {
				if e := r.step(ctx, p); e != nil {
					t.Fatalf("phase %s: %v", p.id.value, e)
				}
			}
			traces, evicted := r.controller.Traces()
			if evicted != 0 {
				t.Fatal("native refusal evidence evicted")
			}
			observed := false
			for _, trace := range traces {
				if trace.Decision == tc.decision {
					observed = true
				}
			}
			if !observed || r.handles[tc.run] != nil {
				t.Fatal("test did not obtain the real Controller refusal")
			}
			root := r.f.wire.Root
			source, e := parser.NewSourceFile(root+".yaml", []byte("mode: "+tc.mode+"\nsequence:\n- delay: 5\n"))
			if e != nil {
				t.Fatal(e)
			}
			catalogue, e := ir.NewCatalogue(nil, nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			scriptID, e := ir.ParseScriptID(root)
			if e != nil {
				t.Fatal(e)
			}
			catalogue, e = catalogue.WithScripts([]ir.ScriptID{scriptID})
			if e != nil {
				t.Fatal(e)
			}
			result, e := ir.CompileScriptSources(root, source.Path(), []parser.SourceFile{source}, catalogue)
			if e != nil || len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
				t.Fatalf("presentation compile: %v %+v", e, result.Rejected())
			}
			r.scripts[root] = result.Accepted()[0]
			// No runtime/network qualification is claimed by this pure projection test.
			capture, e := r.capture(NetworkEvidence{})
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, fact := range capture.facts {
				if fact.key.value != tc.run+"/admission" {
					continue
				}
				payload, ok := fact.payload.(admissionPayload)
				if !ok {
					t.Fatal("refusal lost its admitted variant")
				}
				found = true
				if payload.outcome != tc.outcome {
					t.Fatalf("native refusal %v was replaced from presentation mode %s: got %s", tc.decision, tc.mode, outcomeName(payload.outcome))
				}
			}
			if !found {
				t.Fatal("native refusal disappeared from capture")
			}
		})
	}
}
