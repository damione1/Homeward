package apitypes

import (
	"github.com/damione1/Homeward/internal/domain"
	"strings"
	"testing"
)

func TestNHA202MediaSourceIdentityBindsExactHomeEntityAndSlot(t *testing.T) {
	firstHome, _ := ParseHomeName("homes/first")
	secondHome, _ := ParseHomeName("homes/second")
	entity, _ := domain.ParseEntityID("native-imported-entity")
	first, err := MediaSourceNameForIdentity(firstHome, entity, "content.media.v1", "artwork-one")
	if err != nil {
		t.Fatal(err)
	}
	alternate, _ := MediaSourceNameForIdentity(firstHome, entity, "content.media.v1", "artwork-two")
	otherHome, _ := MediaSourceNameForIdentity(secondHome, entity, "content.media.v1", "artwork-one")
	if first == alternate || first == otherHome {
		t.Fatal("source identity merged distinct slot or home")
	}
	parsed, err := ParseMediaSourceName(first.String())
	if err != nil || parsed != first {
		t.Fatal("canonical source name did not round trip", err)
	}
	for _, raw := range []string{strings.Repeat("/", 1<<20), strings.ToUpper(first.String()), first.String() + "/", strings.Replace(first.String(), "mediaSources", "mediasources", 1)} {
		if _, err := ParseMediaSourceName(raw); err == nil {
			t.Fatal("admitted oversized or noncanonical source name")
		}
	}
	maximumHome, _ := ParseHomeName("homes/" + strings.Repeat("a", 63))
	maximum, _ := MediaSourceNameForIdentity(maximumHome, entity, "content.media.v1", "artwork-one")
	if _, err := ParseMediaSourceName(maximum.String()); err != nil {
		t.Fatal("rejected maximum canonical parent", err)
	}
}
