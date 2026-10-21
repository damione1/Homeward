package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
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
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func fanInput(t *testing.T, evidence nha047.Evidence) report.Input {
	t.Helper()
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	entity, err := ir.NewEntity("fan.living_room_ceiling_fan", ir.ReferencePresent, ir.SupportObserveState)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	description := &registryv1.CapabilityDescription{SchemaId: contracts.FanSchema, Major: 1, Endpoint: "speed", DisplayName: "Synthetic Fan", Constraints: &registryv1.CapabilityDescription_Fan{Fan: &registryv1.FanConstraints{SpeedOptions: []string{"Off", "Low", "Medium", "MediumHigh", "High"}}}}
	declaration, err := report.NewDeclaration(entity.Alias(), description, report.CasetaHAFan4)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := report.NewConfigurationRef("synthetic-report", 1, nha047.Hash([]byte("synthetic configuration")))
	if err != nil {
		t.Fatal(err)
	}
	return report.Input{Selection: selection, Sources: scope.LogicMVPSourcesV1(), Manifest: manifest, Catalogue: catalogue, Configuration: configuration, Declarations: []report.Declaration{declaration}, Evidence: evidence}
}

// Synthetic HA-shaped data exercises the report/comparator boundary only.
// This helper is never published as an executed differential qualification.
func reportFixtureSnapshot(t *testing.T, fixture nha047.Case, side string) parser.SourceFile {
	t.Helper()
	input := fixture.YAML()
	if side == "go" && fixture.Candidate() != "" {
		input = fixture.Candidate()
	}
	file, err := parser.NewSourceFile("synthetic-report.yaml", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func reportCaptureSources(t *testing.T, side, path string) []nha047.SourceHash {
	t.Helper()
	if side != "ha" {
		return []nha047.SourceHash{{Path: path, SHA256: strings.Repeat("a", 64)}}
	}
	data, err := nha047.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Sources []nha047.SourceHash `json:"sources"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Sources
}

func syntheticFanEvidence(t *testing.T) nha047.Evidence {
	t.Helper()
	corpus, err := nha047.Corpus()
	if err != nil {
		t.Fatal(err)
	}
	var fixture nha047.Case
	for _, c := range corpus {
		if c.ID() == "scene-fan-on" {
			fixture = c
		}
	}
	demand, err := nha047.NewDemand(nha047.FanOn, "fan.nha047_value", 75, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	decode := func(side, version, path string) nha047.Capture {
		data, err := nha047.EncodeCapture(fixture, side, version, reportFixtureSnapshot(t, fixture, side), reportCaptureSources(t, side, path), []nha047.Demand{demand}, true, true)
		if err != nil {
			t.Fatal(err)
		}
		capture, err := nha047.DecodeCapture(data)
		if err != nil {
			t.Fatal(err)
		}
		return capture
	}
	comparison, proof, err := nha047.Compare(fixture, decode("ha", nha047.HAVersion, "homeassistant.core"), decode("go", nha047.GoVersion, "probe"))
	if err != nil || !comparison.Equivalent {
		t.Fatalf("synthetic comparison: %v", err)
	}
	evidence, err := nha047.NewEvidence([]nha047.Proof{proof})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func findObject(t *testing.T, r report.Report, id string) report.Object {
	t.Helper()
	for _, object := range r.Objects() {
		name, _ := object.ID().Value()
		if name == id {
			return object
		}
	}
	t.Fatalf("missing object %s", id)
	return report.Object{}
}

func TestNHA047ReportKeepsSelectionAndBusinessSeparate(t *testing.T) {
	input := fanInput(t, syntheticFanEvidence(t))
	r, err := report.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Objects()) != 17 || len(r.Deferred()) != 72 {
		t.Fatal("selection changed")
	}
	for _, id := range []string{"scene.ceilling_fan_day", "scene.ceilling_fan_night"} {
		o := findObject(t, r, id)
		if o.Verdict() != report.AcceptedUnchanged || o.Activation() != report.BusinessPending || o.Activatable() {
			t.Fatalf("technical acceptance changed business admission: %s %+v", id, o)
		}
		for _, node := range o.Constructions() {
			if node.Coverage() != report.Covered || len(node.Proofs()) == 0 {
				t.Fatal("acceptance without an applicable proof")
			}
		}
	}
	if findObject(t, r, "automation.ikea_kitchen_lights_remote").Verdict() != report.Refused {
		t.Fatal("missing private blueprint accepted")
	}
	objects := r.Objects()
	objects[0] = report.Object{}
	if _, err := r.Objects()[0].ID().Value(); err != nil {
		t.Fatal("mutable report")
	}
	var text bytes.Buffer
	if err := report.RenderText(&text, r); err != nil || !strings.Contains(text.String(), "accepté inchangé") {
		t.Fatalf("text report: %v", err)
	}
	data, err := report.JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"activatable": false`)) || bytes.Contains(data, []byte(`"activatable": true`)) {
		t.Fatal("report acquired activation")
	}
	if bytes.Contains(data, []byte("entities:")) {
		t.Fatal("raw YAML leaked")
	}
}

func TestNHA047CoverageAbsentIsAnHonestRefusal(t *testing.T) {
	empty, err := nha047.NewEvidence(nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := report.Build(fanInput(t, empty))
	if err != nil {
		t.Fatal(err)
	}
	o := findObject(t, r, "scene.ceilling_fan_day")
	if o.Verdict() != report.Refused || o.Refusal() != report.CoverageMissing {
		t.Fatalf("absent coverage accepted: %+v", o)
	}
	for _, node := range o.Constructions() {
		if node.Coverage() == report.Covered || len(node.Proofs()) != 0 {
			t.Fatal("a test name substituted for evidence")
		}
	}
}

type nilFS map[string][]byte

func (nilFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func TestNHA047ReportAdmissionRejectsZeroAndTypedNil(t *testing.T) {
	input := fanInput(t, syntheticFanEvidence(t))
	input.Sources = nilFS(nil)
	if _, err := report.Build(input); !errors.Is(err, report.ErrInvalidInput) {
		t.Fatal("typed nil source")
	}
	input = fanInput(t, syntheticFanEvidence(t))
	input.Evidence = nha047.Evidence{}
	if _, err := report.Build(input); !errors.Is(err, report.ErrInvalidInput) {
		t.Fatal("unadmitted evidence")
	}
	if _, err := report.JSON(report.Report{}); !errors.Is(err, report.ErrInvalidReport) {
		t.Fatal("zero report")
	}
	if _, err := report.NewConfigurationRef("untrusted secret", 1, nha047.Hash([]byte("x"))); err == nil {
		t.Fatal("free metadata string")
	}
	if _, err := report.NewDeclaration(domain.HAAlias{}, nil, report.NativeOnly); err == nil {
		t.Fatal("zero declaration")
	}
}

func TestNHA047RendererDoesNotExportSourceValues(t *testing.T) {
	input := fanInput(t, syntheticFanEvidence(t))
	definition, original, canonical := pairSources(t)
	// An explicit, refused input carries the synthetic value without changing
	// source158, its manifest, or any business disposition.
	source, err := parser.NewSourceFile(definition.Path(), append(original.Bytes(), []byte("\n# SENSITIVE_SOURCE_VALUE\n")...))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := report.NewCandidatePair(definition.ID(), source, canonical)
	if err != nil {
		t.Fatal(err)
	}
	input.Candidates = []report.Candidate{candidate}
	r, err := report.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := report.JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SENSITIVE_SOURCE_VALUE")) {
		t.Fatal("source metadata leaked")
	}
	var text bytes.Buffer
	if err := report.RenderText(&text, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), "SENSITIVE_SOURCE_VALUE") {
		t.Fatal("source value leaked in text")
	}
}
