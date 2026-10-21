package report_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/report"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/scope"
)

func TestNHA047ReportRejectsRefabricatedSelectionDefinition(t *testing.T) {
	input := budgetInput(t)
	sources := fstest.MapFS{}
	paths, err := fs.Glob(scope.LogicMVPSourcesV1(), "logic_mvp_sources/*.yaml")
	if err != nil || len(paths) != 17 {
		t.Fatalf("precondition: canonical source lot: %v", err)
	}
	for _, path := range paths {
		data, err := fs.ReadFile(scope.LogicMVPSourcesV1(), path)
		if err != nil {
			t.Fatal(err)
		}
		sources[path] = &fstest.MapFile{Data: data}
	}
	path := "logic_mvp_sources/scene.ceilling_fan_day.yaml"
	// This is deliberate provenance forgery for a rejection oracle. It is
	// never treated as a delivered source158 or a positive migration witness.
	sources[path].Data = append(sources[path].Data, []byte("\n# synthetic noncanonical snapshot\n")...)
	var amendment map[string]json.RawMessage
	if err := json.Unmarshal(scope.LogicMVPV1(), &amendment); err != nil {
		t.Fatal(err)
	}
	var retained []map[string]json.RawMessage
	if err := json.Unmarshal(amendment["retained"], &retained); err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, item := range retained {
		var id string
		if err := json.Unmarshal(item["entity_id"], &id); err != nil {
			t.Fatal(err)
		}
		if id != "scene.ceilling_fan_day" {
			continue
		}
		var definition map[string]json.RawMessage
		if err := json.Unmarshal(item["definition"], &definition); err != nil {
			t.Fatal(err)
		}
		definition["sha256"], err = json.Marshal(nha047.Hash(sources[path].Data).String())
		if err != nil {
			t.Fatal(err)
		}
		item["definition"], err = json.Marshal(definition)
		if err != nil {
			t.Fatal(err)
		}
		changed = true
	}
	if !changed {
		t.Fatal("precondition: selected definition was not changed")
	}
	amendment["retained"], err = json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	forgedJSON, err := json.Marshal(amendment)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := logicselection.Parse(forgedJSON, scope.LogicV1(), input.Manifest, sources)
	if err != nil {
		t.Fatalf("precondition: partition parser no longer admits the revised definition: %v", err)
	}
	for _, entry := range forged.Entries() {
		want, present := input.Selection.DispositionFor(entry.ID())
		if !present || want != entry.Disposition() {
			t.Fatal("precondition: test changed a business disposition")
		}
	}
	input.Selection, input.Sources = forged, sources
	if _, err := report.Build(input); !errors.Is(err, report.ErrInvalidInput) {
		t.Errorf("same partition laundered a noncanonical definition158: %v", err)
	}
}
