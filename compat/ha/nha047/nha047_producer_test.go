package nha047

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
)

func producerCase(t *testing.T, id string) Case {
	t.Helper()
	all, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.ID() == id {
			return c
		}
	}
	t.Fatal("missing registered producer case")
	return Case{}
}

func TestNHA047NativeProducerUsesRealTimeControllerAndConditionReader(t *testing.T) {
	for _, id := range []string{"legacy-action-canonical", "legacy-service-alias", "time-15-false", "time-18-true"} {
		t.Run(id, func(t *testing.T) {
			c := producerCase(t, id)
			input := c.YAML()
			if c.Candidate() != "" {
				input = c.Candidate()
			}
			consumed, err := parser.NewSourceFile("consumed.yaml", []byte(input))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			facts, err := runConsumedAutomation(ctx, c, consumed)
			if err != nil {
				t.Fatal(err)
			}
			want := c.goExpected
			if len(facts) != len(want) {
				t.Fatalf("native requests: got %d want %d", len(facts), len(want))
			}
			for i := range facts {
				if facts[i] != want[i] {
					t.Fatalf("native request %d differs", i)
				}
			}
		})
	}
}

func TestNHA047NativeCompilerConsumesTheSuppliedSnapshotInsteadOfReselectingCase(t *testing.T) {
	c := producerCase(t, "time-15-true")
	wrong := strings.Replace(c.YAML(), "temperature: 15", "temperature: 19", 1)
	consumed, err := parser.NewSourceFile("actually-loaded.yaml", []byte(wrong))
	if err != nil {
		t.Fatal(err)
	}
	climate, err := ir.NewEntity("climate.nha047_value", ir.ReferencePresent, ir.SupportClimateSetpoint)
	if err != nil {
		t.Fatal(err)
	}
	season, err := ir.NewEntity("switch.nha047_season", ir.ReferencePresent, ir.SupportObserveState)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{climate, season}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	automation, err := compileConsumed(c, consumed, catalogue)
	if err != nil {
		t.Fatal(err)
	}
	actions := automation.Actions()
	if len(actions) != 1 || actions[0].Call().Temperature().Value() != 19 {
		t.Fatal("loader silently restored the case's expected input")
	}
	demand, err := NewDemand(ClimateTemperature, "climate.nha047_value", 19, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeCapture(c, "go", GoVersion, consumed, []SourceHash{{Path: "probe", SHA256: strings.Repeat("a", 64)}}, []Demand{demand}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Input string `json:"input_sha256"`
	}
	if err := json.Unmarshal(encoded, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Input != Digest(consumed.SHA256()).String() || observed.Input == Hash([]byte(c.YAML())).String() {
		t.Fatal("loader consumption was stamped with the expected input")
	}
	capture, err := DecodeCapture(encoded)
	if err != nil {
		t.Fatal(err)
	}
	fixture, _ := c.JSON()
	payload, _ := json.Marshal(struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
	}{fixture, encoded})
	if _, err := ProjectInput(payload); !errors.Is(err, ErrInvalidCapture) {
		t.Fatal("wrong runtime/input was projected as HA")
	}
	if capture.wire.InputSHA256 != observed.Input {
		t.Fatal("capture admission changed observed provenance")
	}
}

func TestNHA047NativeWholeRegisteredCorpusUsesDeliveredEngines(t *testing.T) {
	all, err := Corpus()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 29 {
		t.Fatalf("expected 29 bounded native/HA scenarios, got %d", len(all))
	}
	second := producerCase(t, "time-20-workday-false")
	if !second.Season() || second.Workday() || len(second.goExpected) != 0 || len(second.haExpected) != 0 {
		t.Fatal("second AND condition is not independently falsified")
	}
	positive := producerCase(t, "time-20-true")
	if second.YAML() != positive.YAML() || len(positive.goExpected) != 1 {
		t.Fatal("condition controls must consume the same source with opposite observable outcome")
	}
	detached := second.Features()
	detached[0] = Feature{}
	if !second.Features()[0].Valid() {
		t.Fatal("corpus feature list is mutable")
	}
	for _, c := range all {
		t.Run(c.ID(), func(t *testing.T) {
			input := c.YAML()
			if c.Candidate() != "" {
				input = c.Candidate()
			}
			consumed, e := parser.NewSourceFile("native-corpus.yaml", []byte(input))
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var facts []Demand
			switch c.Scenario() {
			case SceneProjection:
				facts, e = runConsumedScene(ctx, c, consumed)
			case TimeClimate, LegacyAction, SunSwitch:
				facts, e = runConsumedAutomation(ctx, c, consumed)
			default:
				t.Fatal("unimplemented registered scenario")
			}
			if e != nil {
				t.Fatal(e)
			}
			if len(facts) != len(c.goExpected) {
				t.Fatalf("observed %d demands, want %d", len(facts), len(c.goExpected))
			}
			for i := range facts {
				if facts[i] != c.goExpected[i] {
					t.Fatalf("native demand %d: got %+v want %+v", i, facts[i], c.goExpected[i])
				}
			}
		})
	}
}
