package scope

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNHA137RevisionBoundary(t *testing.T) {
	if _, err := (Revision{}).Value(); !errors.Is(err, ErrInvalidRevision) {
		t.Fatal("zero revision admitted")
	}
	for _, text := range []string{"", "V1", "v2", "ecobee", " ecobee_mvp_v1", "ecobee_mvp_v1 "} {
		if _, err := ParseRevision(text); !errors.Is(err, ErrInvalidRevision) {
			t.Fatalf("revision %q admitted", text)
		}
	}
	for _, want := range []Revision{BaselineV1(), EcobeeMVPV1()} {
		name, err := want.Value()
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseRevision(name)
		if err != nil || got != want {
			t.Fatalf("revision roundtrip %q: %v", name, err)
		}
	}
}

func TestNHA137EcobeeExactProfileAndImmutableCopies(t *testing.T) {
	a, err := LoadEcobeeMVPV1()
	if err != nil {
		t.Fatal(err)
	}
	if !a.Valid() || len(a.AddedIDs()) != 1 || a.AddedIDs()[0] != "climate.home" || len(a.RemovedIDs()) != 0 || len(a.AdditionalAliases()) != 0 {
		t.Fatal("unexpected identity diff")
	}
	if a.Qualification().Commit != "cf65857901c2c761c6d8526fb49f8742b6af76c5" || a.Qualification().Profile != "qualification/ecobee/profile.go" {
		t.Fatal("009 provenance changed")
	}
	want := map[string]bool{"F": true, "33": true, "11": true, "35": true, "36": true, "10": false, "12": false, "D": false, "AF": false, "BF": false}
	fields := a.Fields()
	fanServices := a.LinkedFanServices()
	fanServices[0] = "foreign"
	if len(fields) != len(want) {
		t.Fatal("field count")
	}
	for _, field := range fields {
		required, known := want[field.HAPType]
		if !known || required != field.Required || field.Limitation == "" {
			t.Fatalf("wrong field %+v", field)
		}
		delete(want, field.HAPType)
	}
	fields[0].Name = "foreign"
	ids := a.AddedIDs()
	ids[0] = "climate.foreign"
	consumers := a.Consumers()
	consumers[3].Via[0] = "scene.foreign"
	technical := a.TechnicalConsumers()
	technical[0].Via[0] = "climate.foreign"
	limits := a.Limits()
	limits[0] = "foreign"
	bytes := EcobeeMVPV1Bytes()
	bytes[0] = 'x'
	if a.LinkedFanServices()[0] != "40" || a.Fields()[0].Name == "foreign" || a.AddedIDs()[0] != "climate.home" || a.Consumers()[3].Via[0] == "scene.foreign" || a.TechnicalConsumers()[0].Via[0] != "climate.home" || a.Limits()[0] == "foreign" {
		t.Fatal("mutable amendment escaped")
	}
	if _, err := LoadEcobeeMVPV1(); err != nil {
		t.Fatal("embedded bytes escaped", err)
	}
}

func TestNHA137EcobeeStrictAdmission(t *testing.T) {
	original := string(EcobeeMVPV1Bytes())
	for _, mutant := range []string{
		original + " {}",
		strings.Replace(original, `"revision": "ecobee_mvp_v1"`, `"revision": "ecobee_mvp_v1", "revision": "ecobee_mvp_v1"`, 1),
		strings.Replace(original, `"schema":`, `"Schema":`, 1),
		strings.Replace(original, `"schema":`, `"unknown": true, "schema":`, 1),
		strings.Replace(original, `"required": false`, `"required": null`, 1),
		strings.Replace(original, `"required": false,`, ``, 1),
		strings.Replace(original, `"required": true`, `"required": 1`, 1),
		strings.Replace(original, `"line": 52`, `"line": 52.0`, 1),
		strings.Replace(original, "climate.home", "sensor.home_current_temperature", 1),
		strings.Replace(original, `"hap_type": "BF"`, `"hap_type": "clear_hold"`, 1),
		strings.Replace(original, `"hap_type": "10"`, `"hap_type": "target_humidity"`, 1),
		strings.Replace(original, `"aliases": []`, `"aliases": ["select.home_current_mode"]`, 1),
		strings.Replace(original, `"removals": []`, `"removals": ["light.foreign"]`, 1),
		strings.Replace(original, "cf65857901c2c761c6d8526fb49f8742b6af76c5", strings.Repeat("0", 40), 1),
		strings.Replace(original, BaselineIDsSHA256, strings.Repeat("0", 64), 1),
	} {
		if _, err := ParseEcobeeAmendment([]byte(mutant), ManifestV1()); !errors.Is(err, ErrInvalidEcobeeAmendment) {
			t.Fatal("mutant admitted", err)
		}
	}
	if _, err := ParseEcobeeAmendment(EcobeeMVPV1Bytes(), append(ManifestV1(), '\n')); !errors.Is(err, ErrInvalidEcobeeAmendment) {
		t.Fatal("nonidentical baseline admitted")
	}
	var reordered any
	if err := json.Unmarshal(EcobeeMVPV1Bytes(), &reordered); err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEcobeeAmendment(compact, ManifestV1()); err != nil {
		t.Fatal("equivalent member order rejected", err)
	}
}
