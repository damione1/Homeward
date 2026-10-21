package nha047_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
)

func consumedSnapshot(t *testing.T, input string) parser.SourceFile {
	t.Helper()
	file, err := parser.NewSourceFile("consumed.yaml", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// Fixture-only migration: explicitly supply the bytes this synthetic envelope
// declares consumed. Real producers must use their loader's actual snapshot.
func syntheticInputSnapshot(t *testing.T, fixture nha047.Case, side string) parser.SourceFile {
	t.Helper()
	input := fixture.YAML()
	if side == "go" && fixture.Candidate() != "" {
		input = fixture.Candidate()
	}
	return consumedSnapshot(t, input)
}

func TestNHA047CaptureEncoderHashesConsumedSnapshotWithoutStampingExpectedRole(t *testing.T) {
	fixture := caseByID(t, "legacy-service-alias")
	demand, err := nha047.NewDemand(nha047.SwitchOn, "switch.nha047_value", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"ha", "go"} {
		t.Run(side, func(t *testing.T) {
			wrong, counterpart, version, source := fixture.Candidate(), fixture.Candidate(), nha047.HAVersion, "homeassistant.core"
			otherSide, otherVersion, otherSource := "go", nha047.GoVersion, "probe"
			if side == "go" {
				wrong, counterpart, version, source = fixture.YAML(), fixture.YAML(), nha047.GoVersion, "probe"
				otherSide, otherVersion, otherSource = "ha", nha047.HAVersion, "homeassistant.core"
			}
			consumed := consumedSnapshot(t, wrong)
			data, err := nha047.EncodeCapture(fixture, side, version, consumed, syntheticCaptureSources(t, side, source), []nha047.Demand{demand}, true, true)
			if err != nil {
				t.Fatal(err)
			}
			var observed string
			if err := json.Unmarshal(presenceObject(t, data)["input_sha256"], &observed); err != nil || observed != nha047.Digest(consumed.SHA256()).String() {
				t.Fatalf("encoder stamped expected corpus input instead of consumed snapshot: %q %v", observed, err)
			}
			other, err := nha047.EncodeCapture(fixture, otherSide, otherVersion, consumedSnapshot(t, counterpart), syntheticCaptureSources(t, otherSide, otherSource), []nha047.Demand{demand}, true, true)
			if err != nil {
				t.Fatal(err)
			}
			ha, goSide := data, other
			if side == "go" {
				ha, goSide = other, data
			}
			comparison, proof, err := nha047.CompareInput(inputProvenanceBundle(t, fixture, ha, goSide))
			if !errors.Is(err, nha047.ErrInvalidCapture) || comparison.Equivalent || proof.Kind() != nha047.EvidenceInvalid {
				t.Fatalf("wrong consumed snapshot acquired a valid role: %+v %v", comparison, err)
			}
		})
	}
}

func TestNHA047CaptureEncoderRejectsZeroAndEmptyConsumedSnapshots(t *testing.T) {
	fixture := caseByID(t, "legacy-service-alias")
	for _, consumed := range []parser.SourceFile{{}, consumedSnapshot(t, "")} {
		if _, err := nha047.EncodeCapture(fixture, "go", nha047.GoVersion, consumed, []nha047.SourceHash{{Path: "probe", SHA256: strings.Repeat("a", 64)}}, nil, true, true); !errors.Is(err, nha047.ErrInvalidCapture) {
			t.Fatalf("missing consumed snapshot was reconstructed from fixture: %v", err)
		}
	}
}

func TestNHA047RegisteredRolesSeparateCanonicalAndAsymmetricProofs(t *testing.T) {
	if (nha047.Case{}).Role() != nha047.RoleInvalid || (nha047.Proof{}).Role() != nha047.RoleInvalid {
		t.Fatal("zero role admitted")
	}
	for _, test := range []struct {
		id   string
		role nha047.ComparisonRole
	}{
		{"legacy-service-alias", nha047.OriginalToCandidate},
		{"legacy-action-canonical", nha047.SameSource},
		{"sunset-switch-on", nha047.SameSource},
	} {
		fixture := caseByID(t, test.id)
		if fixture.Role() != test.role {
			t.Fatalf("registered case %s lost source role", test.id)
		}
		demand, err := nha047.NewDemand(nha047.SwitchOn, "switch.nha047_value", 0, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		comparison, proof, err := nha047.Compare(fixture, syntheticCapture(t, fixture, "ha", []nha047.Demand{demand}), syntheticCapture(t, fixture, "go", []nha047.Demand{demand}))
		if err != nil || !comparison.Equivalent || proof.Role() != test.role {
			t.Fatalf("synthetic comparator did not retain the registered role %s: %v", test.id, err)
		}
		if test.id == "legacy-action-canonical" {
			for _, feature := range proof.Features() {
				if feature.Construction() == nha047.LegacyServiceAlias {
					t.Fatal("canonical witness acquired asymmetric adaptation coverage")
				}
			}
		}
	}
}
