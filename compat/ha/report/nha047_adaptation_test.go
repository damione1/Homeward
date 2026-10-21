package report_test

// Public behavior oracle copied from the fixed preparation source. The model
// API is real; the pair's builder/render integration remains to be qualified.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/report"
	"github.com/damione1/Homeward/contracts"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/scope"
)

const pairObjectID = "automation.turn_on_lights_at_sunset"

// These portable synthetic045 archives are unchanged from the executed,
// approved root qualification. Their hashes remain fixed; this control covers
// only AutomationSingle, never the047 constructions or their execution.
func pairControl045(t *testing.T) nha047.Proof {
	t.Helper()
	const archive = "testdata/nha045-single-busy/03-single-busy."
	var parts [3]json.RawMessage
	for i, item := range []struct{ suffix, hash string }{
		{"fixture.json", "e63ebf343dbe30612d79cdd4efe7a1e5b9f25f03333c6226b574319170191f2c"},
		{"ha.json", "2bc8b4e427481454bb8f128b00979209666a3fc03e51104e4c53b338b1b587cd"},
		{"go.json", "8d9f5ff54173dcdcac8183b8ad726467e01133316d133ca3de4c85795a8690da"},
	} {
		data, err := os.ReadFile(archive + item.suffix)
		if err != nil {
			t.Fatal(err)
		}
		if nha047.Hash(data).String() != item.hash {
			t.Fatalf("archived045 %s drifted", item.suffix)
		}
		parts[i] = data
	}
	bundle, err := json.Marshal(struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
		Go      json.RawMessage `json:"go"`
	}{parts[0], parts[1], parts[2]})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := nha047.ControlProof045(bundle)
	if err != nil {
		t.Fatal(err)
	}
	features := proof.Features()
	if len(features) != 1 || features[0].Construction() != nha047.AutomationSingle {
		t.Fatal("archived045 control expanded beyond single busy")
	}
	return proof
}

// Only045 above is archived executed evidence. The047 envelopes here are
// synthetic comparator inputs used to test report behavior, never published
// as actual HA/native captures or as a live047 qualification.
func pairUnitEvidence(t *testing.T, includeDelta bool) nha047.Evidence {
	t.Helper()
	proofs := syntheticFanEvidence(t).Proofs()
	proofs = append(proofs, pairControl045(t))
	ids := []string{"sunset-switch-on", "legacy-action-canonical"}
	if includeDelta {
		ids = append(ids, "legacy-service-alias")
	}
	corpus, err := nha047.Corpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		var fixture nha047.Case
		for _, candidate := range corpus {
			if candidate.ID() == id {
				fixture = candidate
			}
		}
		if !fixture.Valid() {
			t.Fatalf("required registered047 fixture absent: %s", id)
		}
		demand, err := nha047.NewDemand(nha047.SwitchOn, "switch.nha047_value", 0, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		capture := func(side, version, path string) nha047.Capture {
			data, err := nha047.EncodeCapture(fixture, side, version, reportFixtureSnapshot(t, fixture, side), reportCaptureSources(t, side, path), []nha047.Demand{demand}, true, true)
			if err != nil {
				t.Fatal(err)
			}
			value, err := nha047.DecodeCapture(data)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		comparison, proof, err := nha047.Compare(fixture, capture("ha", nha047.HAVersion, "homeassistant.core"), capture("go", nha047.GoVersion, "probe"))
		if err != nil || !comparison.Equivalent {
			t.Fatalf("synthetic047 unit input %s: %v", id, err)
		}
		proofs = append(proofs, proof)
	}
	evidence, err := nha047.NewEvidence(proofs)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func pairSources(t *testing.T) (logicselection.Definition, parser.SourceFile, parser.SourceFile) {
	t.Helper()
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range selection.Retained() {
		id, _ := definition.ID().Value()
		if id != pairObjectID {
			continue
		}
		data, err := fs.ReadFile(scope.LogicMVPSourcesV1(), strings.TrimPrefix(definition.Path(), "scope/"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), "- action: switch.turn_on") != 1 {
			t.Fatal("canonical source no longer has its unique switch call")
		}
		canonical, err := parser.NewSourceFile(definition.Path(), data)
		if err != nil {
			t.Fatal(err)
		}
		// A synthetic alternative input for this test only, not a historical
		// household export or a change to the source158 document.
		legacy, err := parser.NewSourceFile(definition.Path(), bytes.Replace(data, []byte("- action: switch.turn_on"), []byte("- service: switch.turn_on"), 1))
		if err != nil {
			t.Fatal(err)
		}
		return definition, legacy, canonical
	}
	t.Fatal("selected sunset definition absent")
	return logicselection.Definition{}, parser.SourceFile{}, parser.SourceFile{}
}

func pairInput(t *testing.T, evidence nha047.Evidence) report.Input {
	t.Helper()
	input := fanInput(t, evidence)
	entities := input.Catalogue.Entities()
	relay, err := ir.NewEntity("switch.basement_relay", ir.ReferencePresent, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	input.Catalogue, err = ir.NewCatalogue(append(entities, relay), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	description := &registryv1.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic Report Relay", Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}}
	declaration, err := report.NewDeclaration(relay.Alias(), description, report.NativeOnly)
	if err != nil {
		t.Fatal(err)
	}
	input.Declarations = append(input.Declarations, declaration)
	return input
}

func TestNHA047CandidatePairAcceptsOnlyExplicitProvenSourceAndKeepsBusinessPending(t *testing.T) {
	definition, original, canonical := pairSources(t)
	input := pairInput(t, pairUnitEvidence(t, true))
	candidate, err := report.NewCandidatePair(definition.ID(), original, canonical)
	if err != nil {
		t.Fatal(err)
	}
	input.Candidates = []report.Candidate{candidate}
	originalBefore, canonicalBefore := original.Bytes(), canonical.Bytes()
	r, err := report.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	object := findObject(t, r, pairObjectID)
	if object.Verdict() != report.AcceptedAdapted || object.Refusal() != report.RefusalNone || object.Activation() != report.BusinessPending || object.Activatable() {
		t.Fatalf("public Build did not admit the explicit covered pair: verdict=%s refusal=%s", object.Verdict(), object.Refusal())
	}
	if len(r.Objects()) != 17 || len(r.Deferred()) != 72 || object.Definition().Digest() != definition.Digest() {
		t.Fatal("source158 or partition changed")
	}
	if findObject(t, r, "scene.ceilling_fan_day").Verdict() != report.AcceptedUnchanged || findObject(t, r, "automation.ikea_kitchen_lights_remote").Verdict() != report.Refused {
		t.Fatal("three verdicts are not observable through the same public report")
	}
	if digest, present := object.CandidateDigest(); !present || digest != nha047.Digest(canonical.SHA256()) {
		t.Fatal("candidate provenance missing")
	}
	if !bytes.Equal(originalBefore, original.Bytes()) || !bytes.Equal(canonicalBefore, canonical.Bytes()) {
		t.Fatal("report rewrote an input snapshot")
	}
	data, err := report.JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Objects []struct {
			ID        string `json:"id"`
			SourceSHA string `json:"source_sha256"`
			Original  struct {
				Origin string `json:"origin"`
				Path   string `json:"path"`
				SHA    string `json:"sha256"`
			} `json:"original_source"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range wire.Objects {
		if row.ID != pairObjectID {
			continue
		}
		found = row.SourceSHA == nha047.Digest(definition.Digest()).String() && row.Original.Origin == "explicit_import_pair" && row.Original.Path == original.Path() && row.Original.SHA == nha047.Digest(original.SHA256()).String()
	}
	if !found {
		t.Fatal("supplied original was laundered into documentary provenance158")
	}
}

func TestNHA047CandidatePairCanonicalProofCannotReplaceDeltaProof(t *testing.T) {
	definition, original, canonical := pairSources(t)
	input := pairInput(t, pairUnitEvidence(t, false))
	candidate, err := report.NewCandidatePair(definition.ID(), original, canonical)
	if err != nil {
		t.Fatal(err)
	}
	input.Candidates = []report.Candidate{candidate}
	r, err := report.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	object := findObject(t, r, pairObjectID)
	if object.Verdict() != report.Refused || object.Refusal() != report.CoverageMissing || object.Activatable() {
		t.Fatal("canonical-only proof validated the asymmetric delta")
	}
	found := false
	for _, node := range object.Constructions() {
		if node.Feature().Construction() == nha047.LegacyServiceAlias {
			found = node.Coverage() == report.Absent && len(node.Proofs()) == 0
		}
	}
	if !found {
		t.Fatal("missing adaptation coverage was hidden")
	}
}

func TestNHA047CandidatePairRejectsChangedOrUnanchoredDeltas(t *testing.T) {
	definition, original, canonical := pairSources(t)
	for _, test := range []struct {
		name                string
		original, candidate []byte
	}{
		{"no_delta", canonical.Bytes(), canonical.Bytes()},
		{"original_extra_comment", append(original.Bytes(), []byte("\n# extra\n")...), canonical.Bytes()},
		{"changed_target", bytes.Replace(original.Bytes(), []byte("switch.basement_relay"), []byte("switch.nha047_other"), 1), canonical.Bytes()},
		{"changed_condition", bytes.Replace(original.Bytes(), []byte("conditions: []"), []byte("conditions: [{condition: state, entity_id: switch.nha047_other, state: 'on'}]"), 1), canonical.Bytes()},
		{"candidate_extra_comment", original.Bytes(), append(canonical.Bytes(), []byte("\n# extra\n")...)},
		{"candidate_changed_mode", original.Bytes(), bytes.Replace(canonical.Bytes(), []byte("mode: single"), []byte("mode: restart"), 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			left, err := parser.NewSourceFile(definition.Path(), test.original)
			if err != nil {
				t.Fatal(err)
			}
			right, err := parser.NewSourceFile(definition.Path(), test.candidate)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := report.NewCandidatePair(definition.ID(), left, right)
			if err != nil {
				t.Fatal(err)
			}
			input := pairInput(t, pairUnitEvidence(t, true))
			input.Candidates = []report.Candidate{candidate}
			r, err := report.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			object := findObject(t, r, pairObjectID)
			if object.Verdict() != report.Refused || object.Refusal() != report.AdaptationUnvalidated || object.Activatable() {
				t.Fatalf("unvalidated delta admitted: %s / %s", object.Verdict(), object.Refusal())
			}
		})
	}
}

func TestNHA047CandidatePairRequiresTwoNonzeroSnapshotsAndAutomationIdentity(t *testing.T) {
	definition, original, canonical := pairSources(t)
	sceneID, err := domain.ParseHAAlias("scene.ceilling_fan_day")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id                  domain.HAAlias
		original, candidate parser.SourceFile
	}{
		{domain.HAAlias{}, original, canonical},
		{sceneID, original, canonical},
		{definition.ID(), parser.SourceFile{}, canonical},
		{definition.ID(), original, parser.SourceFile{}},
	} {
		if _, err := report.NewCandidatePair(test.id, test.original, test.candidate); !errors.Is(err, report.ErrInvalidInput) {
			t.Errorf("uninitialized pair admitted: %v", err)
		}
	}
}
