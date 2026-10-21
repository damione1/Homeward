package nha047_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/nha047"
)

func caseByID(t *testing.T, id string) nha047.Case {
	t.Helper()
	corpus, err := nha047.Corpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("missing registered case %s", id)
	return nha047.Case{}
}

// These are synthetic boundary observations used to test the comparator. They
// do not claim that HA, Linux isolation, the native probe or any device ran.
func syntheticCaptureSources(t *testing.T, side, path string) []nha047.SourceHash {
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

func syntheticCapture(t *testing.T, c nha047.Case, side string, demands []nha047.Demand) nha047.Capture {
	t.Helper()
	version, path := nha047.GoVersion, "probe"
	if side == "ha" {
		version, path = nha047.HAVersion, "homeassistant.core"
	}
	data, err := nha047.EncodeCapture(c, side, version, syntheticInputSnapshot(t, c, side), syntheticCaptureSources(t, side, path), demands, true, true)
	if err != nil {
		t.Fatal(err)
	}
	result, err := nha047.DecodeCapture(data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHA047RegisteredOraclesAreFixedAndBounded(t *testing.T) {
	c := caseByID(t, "scene-light-89-357")
	found := false
	for _, f := range c.Features() {
		if f.Construction() == nha047.SceneLightCT {
			found = f.Value() == 89 && f.Secondary() == 357
		}
	}
	if !found || !strings.Contains(c.YAML(), "color_temp_kelvin: 2801") {
		t.Fatal("CT oracle lost independent units/values")
	}
	data, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nha047.DecodeCase(data); err != nil {
		t.Fatal(err)
	}
	mutant := strings.Replace(string(data), "2801", "2802", 1)
	if _, err := nha047.DecodeCase([]byte(mutant)); !errors.Is(err, nha047.ErrInvalidInput) {
		t.Fatal("unregistered fixture acquired its own proof")
	}
	features := c.Features()
	features[0] = nha047.Feature{}
	if !c.Features()[0].Valid() {
		t.Fatal("mutable registered feature list")
	}
	if _, err := nha047.NewFeature(nha047.SceneLightBrightness, 256, 0); err == nil {
		t.Fatal("out-of-bound brightness")
	}
	if _, err := nha047.NewFeature(nha047.ClimateSetpoint, 19, 0); err == nil {
		t.Fatal("a proof for 18 must not cover 19")
	}
}

func TestNHA047ComparatorKeepsParametersMultiplicityAndProvenance(t *testing.T) {
	c := caseByID(t, "scene-light-89-357")
	d, err := nha047.NewDemand(nha047.LightOn, "light.nha047_value", 89, 357, "")
	if err != nil {
		t.Fatal(err)
	}
	ha := syntheticCapture(t, c, "ha", []nha047.Demand{d})
	goSide := syntheticCapture(t, c, "go", []nha047.Demand{d})
	comparison, proof, err := nha047.Compare(c, ha, goSide)
	if err != nil || !comparison.Equivalent || proof.Kind() != nha047.DifferentialEquivalent {
		t.Fatalf("exact synthetic comparator input: %+v %v", comparison, err)
	}
	other, _ := nha047.NewDemand(nha047.LightOn, "light.nha047_value", 89, 250, "")
	for _, facts := range [][]nha047.Demand{{}, {d, d}, {other}} {
		result, p, err := nha047.Compare(c, ha, syntheticCapture(t, c, "go", facts))
		if !errors.Is(err, nha047.ErrDivergence) || result.Equivalent || len(result.Differences) == 0 || p.Kind() != nha047.EvidenceInvalid {
			t.Fatal("missing/duplicate/changed request hidden")
		}
	}
	otherCase := caseByID(t, "scene-light-255-250")
	if _, _, err := nha047.Compare(otherCase, ha, goSide); !errors.Is(err, nha047.ErrInvalidCapture) {
		t.Fatal("captures transferred to another fixture")
	}
}

func TestNHA047CaptureAdmissionRejectsAmbiguousBoundaries(t *testing.T) {
	c := caseByID(t, "scene-light-89-357")
	d, _ := nha047.NewDemand(nha047.LightOn, "light.nha047_value", 89, 357, "")
	good, err := nha047.EncodeCapture(c, "go", nha047.GoVersion, syntheticInputSnapshot(t, c, "go"), []nha047.SourceHash{{Path: "probe", SHA256: strings.Repeat("a", 64)}}, []nha047.Demand{d}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"terminal":true`, `"terminal":null`, 1) },
		func(s string) string { return strings.Replace(s, `"evicted":0`, `"evicted":1`, 1) },
		func(s string) string { return strings.Replace(s, `"loopback_only":true`, `"loopback_only":false`, 1) },
		func(s string) string { return strings.Replace(s, `"runtime":"go"`, `"runtime":"go","runtime":"ha"`, 1) },
		func(s string) string { return strings.Replace(s, `"schema":`, `"unknown":true,"schema":`, 1) },
		func(s string) string { return strings.Replace(s, `"version":"go1.27.2"`, `"version":"go1.25.2"`, 1) },
	} {
		if _, err := nha047.DecodeCapture([]byte(edit(string(good)))); err == nil {
			t.Fatal("ambiguous capture admitted")
		}
	}
	if _, err := nha047.DecodeEvidence([]byte(`{"schema":"nha047/v1","comparisons":[],"tests":["TestNHA047"]}`)); err == nil {
		t.Fatal("a test name became evidence")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(good, &envelope); err != nil {
		t.Fatal(err)
	}
	delete(envelope, "evicted")
	missing, _ := json.Marshal(envelope)
	if _, err := nha047.DecodeCapture(missing); err == nil {
		t.Fatal("absent required scalar admitted")
	}
}

func TestNHA047EvidenceRejectsZerosAndOwnsCopies(t *testing.T) {
	if _, err := nha047.NewEvidence([]nha047.Proof{{}}); err == nil {
		t.Fatal("zero proof")
	}
	if (nha047.Evidence{}).Valid() {
		t.Fatal("zero evidence")
	}
	empty, err := nha047.NewEvidence(nil)
	if err != nil || !empty.Valid() {
		t.Fatal("explicit empty evidence must be usable for an honest refusal report")
	}
	c := caseByID(t, "scene-fan-on")
	d, _ := nha047.NewDemand(nha047.FanOn, "fan.nha047_value", 75, 0, "")
	_, p, err := nha047.Compare(c, syntheticCapture(t, c, "ha", []nha047.Demand{d}), syntheticCapture(t, c, "go", []nha047.Demand{d}))
	if err != nil {
		t.Fatal(err)
	}
	e, err := nha047.NewEvidence([]nha047.Proof{p})
	if err != nil {
		t.Fatal(err)
	}
	copy := e.Proofs()
	copy[0] = nha047.Proof{}
	if e.Proofs()[0].Kind() != nha047.DifferentialEquivalent {
		t.Fatal("mutable evidence")
	}
	if _, err := nha047.NewEvidence([]nha047.Proof{p, p}); err == nil {
		t.Fatal("duplicate proof")
	}
}
