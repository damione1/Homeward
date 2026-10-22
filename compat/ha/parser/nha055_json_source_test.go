package parser

import "testing"

func TestNHA055JSONSnapshotOwnsExactBytesWithoutOpeningYAMLLoader(t *testing.T) {
	raw := []byte(`{"private":"exact"}`)
	file, err := NewJSONSourceFile("ownership.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest := file.SHA256()
	raw[0] = '['
	exported := file.Bytes()
	exported[0] = '['
	clone, err := CloneSourceFile(file)
	if err != nil || clone.SHA256() != digest || string(clone.Bytes()) != `{"private":"exact"}` {
		t.Fatal("JSON snapshot lost exact detached bytes", err)
	}
	if _, err := NewSourceFile("ownership.json", file.Bytes()); err == nil {
		t.Fatal("YAML source factory admitted JSON path")
	}
	if _, err := LoadSources("ownership.json", []SourceFile{file}, SyntaxProfileCore); err == nil {
		t.Fatal("YAML loader admitted JSON source")
	}
	for _, path := range []string{"../ownership.json", "/ownership.json", "a/../ownership.json", "ownership.yaml"} {
		if _, err := NewJSONSourceFile(path, nil); err == nil {
			t.Fatalf("invalid JSON path admitted: %s", path)
		}
	}
}
