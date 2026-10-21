package report_test

import (
	"bytes"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/report"
	"testing"
)

// A substitution of an excluded platform is a business change, not the one
// proven service/action syntax alias. These are refusal oracles, not HA proofs.
func TestNHA047ExcludedPlatformSubstitutionsCannotAcquireAnAdaptedVerdict(t *testing.T) {
	definition, _, canonical := pairSources(t)
	canonicalBefore := canonical.Bytes()
	for _, service := range []string{"homekit_controller.send_command", "cast.show_lovelace_view", "music_assistant.play_media"} {
		t.Run(service, func(t *testing.T) {
			originalBytes := bytes.Replace(canonical.Bytes(), []byte("- action: switch.turn_on"), []byte("- action: "+service), 1)
			original, e := parser.NewSourceFile(canonical.Path(), originalBytes)
			if e != nil {
				t.Fatal(e)
			}
			candidate, e := report.NewCandidatePair(definition.ID(), original, canonical)
			if e != nil {
				t.Fatal(e)
			}
			input := pairInput(t, pairUnitEvidence(t, true))
			input.Candidates = []report.Candidate{candidate}
			result, e := report.Build(input)
			if e != nil {
				t.Fatal(e)
			}
			object := findObject(t, result, pairObjectID)
			if object.Verdict() != report.Refused || object.Refusal() != report.AdaptationUnvalidated || object.Activatable() {
				t.Fatal("excluded platform substitution admitted")
			}
			if !bytes.Equal(original.Bytes(), originalBytes) || !bytes.Equal(canonical.Bytes(), canonicalBefore) {
				t.Fatal("report applied a replacement")
			}
		})
	}
}
