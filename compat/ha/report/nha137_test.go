package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/report"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func TestNHA137ReportUsesLoadedManifestAndKeepsClimateRefused(t *testing.T) {
	evidence, err := nha047.NewEvidence(nil)
	if err != nil {
		t.Fatal(err)
	}
	input := fanInput(t, evidence)
	climate, err := ir.NewEntity("climate.home", ir.ReferencePresent, ir.SupportObserveState, ir.SupportClimateSetpoint)
	if err != nil {
		t.Fatal(err)
	}
	input.Catalogue, err = ir.NewCatalogue(append(input.Catalogue.Entities(), climate), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	configuration := input.Configuration
	var previous string
	for _, revision := range []scope.Revision{scope.BaselineV1(), scope.EcobeeMVPV1(), scope.BaselineV1(), scope.EcobeeMVPV1()} {
		manifest, err := registry.LoadManifest(revision)
		if err != nil {
			t.Fatal(err)
		}
		r, err := report.BuildMVPWithManifest(manifest, input.Catalogue, configuration, input.Declarations, evidence, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Configuration() != configuration {
			t.Fatal("scope selection changed config identity")
		}
		gotRevision, ok := r.ScopeRevision()
		if !ok || gotRevision != revision || len(r.Objects()) != 17 || len(r.Deferred()) != 72 {
			t.Fatal("scope/logic selection mismatch")
		}
		for _, identity := range []string{"scene.climate_day_temperature", "scene.climate_night_temperature"} {
			o := findObject(t, r, identity)
			if o.Verdict() != report.Refused || o.Activatable() {
				t.Fatal("scope admission accepted or activated climate scene")
			}
			unsupported := false
			for _, node := range o.Constructions() {
				if node.Feature().Construction() == nha047.SceneClimateUnsupported && node.Refusal() == report.UnsupportedConstruction {
					unsupported = true
				}
			}
			if !unsupported {
				t.Fatal("whole climate member refusal lost")
			}
		}
		data, err := report.JSON(r)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Manifest       string `json:"manifest_ids_sha256"`
			ConfigRevision uint64 `json:"configuration_revision"`
			Scope          struct {
				Revision    string   `json:"revision"`
				Digest      string   `json:"effective_ids_sha256"`
				Entities    int      `json:"entity_count"`
				Sources     int      `json:"source_count"`
				Diagnostics int      `json:"diagnostic_count"`
				Added       []string `json:"added_ids"`
				Consumers   []struct {
					ID          string `json:"entity_id"`
					Disposition string `json:"disposition"`
					Active      bool   `json:"active"`
					Status      string `json:"status"`
				} `json:"logic_consumers"`
				Technical []struct {
					ID  string   `json:"entity_id"`
					Via []string `json:"via"`
				} `json:"technical_consumers"`
			} `json:"scope"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		wantName, _ := revision.Value()
		if wire.Scope.Revision != wantName || wire.ConfigRevision != configuration.Revision() || wire.Manifest != wire.Scope.Digest || wire.Scope.Diagnostics != 19 {
			t.Fatal("rendered provenance did not come from loaded Manifest")
		}
		counts, _ := manifest.Counts()
		if wire.Scope.Entities != counts.Entities || wire.Scope.Sources != counts.Sources {
			t.Fatal("rendered counts do not describe active set")
		}
		if revision == scope.EcobeeMVPV1() {
			if len(wire.Scope.Added) != 1 || wire.Scope.Added[0] != "climate.home" {
				t.Fatal("wrong active diff")
			}
		} else if len(wire.Scope.Added) != 0 || wire.Manifest != scope.BaselineIDsSHA256 {
			t.Fatal("baseline was widened")
		}
		if previous == wire.Manifest {
			t.Fatal("effective digest did not change on selected scope change")
		}
		previous = wire.Manifest
		wantConsumers := map[string]string{
			"scene.climate_day_temperature": "retained", "scene.climate_night_temperature": "retained",
			"automation.turn_on_hvac": "deferred", "automation.turn_off_hvac": "deferred", "automation.good_morning_work_day": "deferred", "automation.morning_routine": "deferred", "script.morning_routine_weekend": "deferred", "script.morning_routine_work_day": "deferred", "automation.automatic_morning_routine": "deferred",
		}
		if len(wire.Scope.Consumers) != len(wantConsumers) {
			t.Fatal("consumer count")
		}
		for _, consumer := range wire.Scope.Consumers {
			want, known := wantConsumers[consumer.ID]
			if !known || want != consumer.Disposition || consumer.Active || consumer.Status != "requires_remapping_and_qualification" {
				t.Fatal("consumer partition/status")
			}
			delete(wantConsumers, consumer.ID)
		}
		if len(wantConsumers) != 0 || len(wire.Scope.Technical) != 2 || wire.Scope.Technical[0].ID != "sensor.hvac_power_consumption" || wire.Scope.Technical[1].ID != "sensor.hvac_energy_consumption" || wire.Scope.Technical[1].Via[0] != "sensor.hvac_power_consumption" {
			t.Fatal("HVAC dependency chain missing")
		}
		var text bytes.Buffer
		if err := report.RenderText(&text, r); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{wantName, wire.Manifest, "39", "requires_remapping_and_qualification", "automation.automatic_morning_routine", "sensor.hvac_energy_consumption"} {
			if want == "39" && revision == scope.BaselineV1() {
				continue
			}
			if !strings.Contains(text.String(), want) {
				t.Fatalf("text report missing %q", want)
			}
		}
	}
}

func TestNHA137OfflineScopeSelectorIsExplicitAndStrict(t *testing.T) {
	for _, fixture := range []struct {
		member string
		want   scope.Revision
	}{
		{"", scope.BaselineV1()},
		{`"scope_revision":"v1",`, scope.BaselineV1()},
		{`"scope_revision":"ecobee_mvp_v1",`, scope.EcobeeMVPV1()},
	} {
		data := strings.Replace(nha047OfflineEmpty, `"configuration":`, fixture.member+`"configuration":`, 1)
		r, err := report.OfflineReport([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := r.ScopeRevision()
		if !ok || got != fixture.want || r.Configuration().Revision() != 1 {
			t.Fatal("offline selected another scope/config revision")
		}
	}
	for _, member := range []string{
		`"scope_revision":null,`, `"scope_revision":"",`, `"scope_revision":"v2",`, `"scope_revision":1,`,
		`"scope_revision":"ecobee_mvp_v1","scope_revision":"ecobee_mvp_v1",`,
		`"SCOPE_REVISION":"ecobee_mvp_v1",`, `"Scope_Revision":null,`,
	} {
		data := strings.Replace(nha047OfflineEmpty, `"configuration":`, member+`"configuration":`, 1)
		if _, err := report.OfflineReport([]byte(data)); err == nil {
			t.Fatal("unadmitted offline scope", member)
		}
	}
}
