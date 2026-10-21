package scenes

import (
	"github.com/damione1/Homeward/compat/ha/parser"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

func nha047LoadedDefinition(t *testing.T) hascene.Definition {
	t.Helper()
	alias, e := domain.ParseHAAlias("scene.nha047_catalogue")
	if e != nil {
		t.Fatal(e)
	}
	file, e := parser.NewSourceFile("nha047.yaml", []byte("id: '1'\nname: Synthetic\nentities:\n  switch.nha047_value: 'on'\n"))
	if e != nil {
		t.Fatal(e)
	}
	ref, e := hascene.NewSourceRef(alias, file.Path(), file.SHA256())
	if e != nil {
		t.Fatal(e)
	}
	result, e := hascene.Load(ref, file.Bytes())
	if e != nil || len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
		t.Fatal("real loader rejected fixture", e)
	}
	return result.Accepted()[0]
}

func TestNHA047CatalogueRequiresAdmittedNonzeroUniqueDefinitions(t *testing.T) {
	definition := nha047LoadedDefinition(t)
	for _, definitions := range [][]hascene.Definition{nil, {}, {{}}, {definition, definition}, make([]hascene.Definition, 13)} {
		if _, e := NewCatalogue(definitions); e == nil {
			t.Fatal("invalid definition set admitted")
		}
	}
	if (Catalogue{}).valid() {
		t.Fatal("zero catalogue admitted")
	}
	catalogue, e := NewCatalogue([]hascene.Definition{definition})
	if e != nil || !catalogue.valid() {
		t.Fatal(e)
	}
	input := []hascene.Definition{definition}
	detached, e := NewCatalogue(input)
	if e != nil {
		t.Fatal(e)
	}
	input[0] = hascene.Definition{}
	ids := detached.IDs()
	ids[0] = domain.HAAlias{}
	got, ok := detached.Definition(definition.ID())
	if !ok || got.Source().Digest() != definition.Source().Digest() {
		t.Fatal("caller mutated admitted catalogue")
	}
	malformed := Catalogue{definitions: catalogue.definitions, order: []domain.HAAlias{{}}}
	if malformed.valid() {
		t.Fatal("map/order mismatch admitted")
	}
}
