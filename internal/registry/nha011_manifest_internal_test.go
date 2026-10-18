package registry

import (
	"testing"

	"github.com/damione1/Homeward/scope"
)

func TestNHA011ManifestSourceRequiresParsedProvenance(t *testing.T) {
	alias, err := ParseHAAlias("climate.chambre_eugenie")
	if err != nil {
		t.Fatal(err)
	}
	aliasOnly := Manifest{aliases: map[HAAlias]struct{}{alias: {}}}
	if _, present := aliasOnly.SourceFor(alias); present {
		t.Fatal("alias-only manifest invented provider provenance")
	}
	if _, present := aliasOnly.Digest(); present {
		t.Fatal("alias-only manifest invented parsed digest")
	}
	parsed, err := ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	source, present := parsed.SourceFor(alias)
	if !present {
		t.Fatal("parsed manifest lost source provenance")
	}
	if value, valid := source.Value(); !valid || value != "hilo" {
		t.Fatalf("source = %q, valid %v", value, valid)
	}
	if _, present := parsed.SourceFor(HAAlias{}); present {
		t.Fatal("zero alias gained source")
	}
}
