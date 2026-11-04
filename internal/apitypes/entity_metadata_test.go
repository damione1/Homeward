package apitypes

import (
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func TestEntityMetadataIdentityRevisionAndAbsence(t *testing.T) {
	home, _ := ParseHomeName("homes/synthetic")
	id, _ := domain.ParseEntityID("ent_synthetic_native")
	other, _ := domain.ParseEntityID("ent_other_native")
	label, _ := NewDisplayName("Pièce témoin")
	first, err := NewEntityMetadata(id, label, 1, nil, nil)
	if err != nil || !first.Valid() {
		t.Fatal(err)
	}
	if _, present := first.CreateTime(); present {
		t.Fatal("historical creation time invented")
	}
	if _, present := first.UpdateTime(); present {
		t.Fatal("historical update time invented")
	}
	second, _ := NewEntityMetadata(id, label, 2, nil, nil)
	different, _ := NewEntityMetadata(other, label, 1, nil, nil)
	if first.ETag() == second.ETag() || first.ETag() == different.ETag() {
		t.Fatal("etag does not bind identity and revision")
	}
	name, err := EntityNameForIdentity(home, id)
	if err != nil || name.Home() != home {
		t.Fatal("native identity mapping", err)
	}
	cleared, _ := NewEntityMetadata(id, DisplayName{}, 1, nil, nil)
	if first.ETag() != cleared.ETag() {
		t.Fatal("etag depends on mutable label outside revision")
	}
	if (EntityMetadata{}).ETag().Valid() {
		t.Fatal("zero metadata produced an etag")
	}
	zone := time.FixedZone("synthetic", 3600)
	created := time.Date(2026, 1, 2, 12, 0, 0, 0, zone)
	updated := created.Add(time.Second)
	known, err := NewEntityMetadata(id, label, 1, &created, &updated)
	if err != nil {
		t.Fatal(err)
	}
	created = created.Add(24 * time.Hour)
	got, present := known.CreateTime()
	if !present || got.Location() != time.UTC || !got.Before(created) {
		t.Fatal("metadata retained mutable timestamp reference")
	}
	if _, err := NewEntityMetadata(id, label, 1, &created, &updated); err == nil {
		t.Fatal("accepted update before creation")
	}
}

func TestEntityMetadataAdmissionBoundsAndNoOp(t *testing.T) {
	for _, label := range []string{" leading", "trailing ", "line\nfeed", string([]byte{0xff}), strings.Repeat("é", 64)} {
		if _, err := NewDisplayName(label); err == nil {
			t.Fatalf("accepted invalid label %q", label)
		}
	}
	if _, err := NewDisplayName(strings.Repeat("é", 63)); err != nil {
		t.Fatal("Unicode code-point boundary", err)
	}
	for _, etag := range []string{"", "bad\x00token", strings.Repeat("x", 257), string([]byte{0xff})} {
		if _, err := ParseEntityETag(etag); err == nil {
			t.Fatal("accepted malformed etag")
		}
	}
	if _, err := ParseEntityETag("opaque-revision-from-another-capture"); err != nil {
		t.Fatal("opaque token must reach CAS", err)
	}
	if (MetadataPatch{}).Valid() {
		t.Fatal("zero patch is executable")
	}
	if _, selected := KeepEntityMetadata().DisplayName(); selected {
		t.Fatal("no-op selected an update")
	}
	if value, selected := SetEntityDisplayName(DisplayName{}).DisplayName(); !selected || value.String() != "" {
		t.Fatal("explicit clear lost presence")
	}
}
