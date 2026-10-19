package lists

import (
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA103StrictIdentityAndFieldBounds(t *testing.T) {
	if _, err := (domain.ListID{}).Value(); err == nil {
		t.Fatal("zero list ID admitted")
	}
	if _, err := (ItemUID{}).Value(); err == nil {
		t.Fatal("zero item UID admitted")
	}
	if _, _, err := (ListRevision{}).Values(); err == nil {
		t.Fatal("zero list revision admitted")
	}
	if _, _, _, err := (ItemRevision{}).Values(); err == nil {
		t.Fatal("zero item revision admitted")
	}
	if _, err := ParseItemUID(strings.Repeat("u", MaxUIDBytes+1)); !IsCode(err, Invalid) {
		t.Fatal("oversize UID admitted")
	}
	if _, err := NewListPayload(strings.Repeat("n", MaxNameBytes+1)); !IsCode(err, Invalid) {
		t.Fatal("oversize name admitted")
	}
	longDescription := strings.Repeat("d", MaxDescriptionBytes+1)
	if _, err := NewItemPayload(ItemInput{Summary: "x", Description: &longDescription, Due: NoDue()}); !IsCode(err, Invalid) {
		t.Fatal("oversize description admitted")
	}
	if _, err := NewItemPayload(ItemInput{Summary: "x"}); !IsCode(err, Invalid) {
		t.Fatal("implicit zero due admitted")
	}
	if _, err := NewDueDate("2026-02-30"); !IsCode(err, Invalid) {
		t.Fatal("invalid civil date admitted")
	}
	if _, err := originFromSQL("future"); !IsCode(err, Unavailable) {
		t.Fatal("future SQL origin admitted")
	}
	if _, err := (OriginInvalid).sql(); !IsCode(err, Invalid) {
		t.Fatal("invalid Go origin converted to SQL")
	}
	if _, err := lifecycleFromSQL(3); !IsCode(err, Unavailable) {
		t.Fatal("unknown SQL lifecycle admitted")
	}
	uid, _ := ParseItemUID("ics:opaque@local")
	if _, err := NewLegacyItemPayload(uid, -1, mustItem(t, "valid")); !IsCode(err, Invalid) {
		t.Fatal("negative position admitted")
	}
}

func TestNHA103TemporalRangePrecisionAndPresenceFingerprint(t *testing.T) {
	if _, err := NewDueDate("0000-01-01"); !IsCode(err, Invalid) {
		t.Fatal("year zero civil date admitted")
	}
	if _, err := NewDueInstant(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "Etc/UTC"); !IsCode(err, Invalid) {
		t.Fatal("year 10000 due admitted")
	}
	if _, err := NewDueInstant(time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC), "Etc/UTC"); !IsCode(err, Invalid) {
		t.Fatal("submicrosecond due admitted")
	}
	instant := time.Date(2026, 10, 19, 15, 30, 0, 123000, time.UTC)
	for _, zone := range []string{"UTC", "Etc/UTC", "America/Toronto"} {
		due, err := NewDueInstant(instant, zone)
		if err != nil {
			t.Fatalf("stable timezone %q: %v", zone, err)
		}
		date, at, gotZone, err := due.Values()
		if err != nil || date != "" || !at.Equal(instant) || gotZone != zone {
			t.Fatalf("timezone round-trip %q: %v", zone, err)
		}
	}
	for _, zone := range []string{"Local", "", "No/Such_Zone"} {
		if _, err := NewDueInstant(instant, zone); !IsCode(err, Invalid) {
			t.Fatalf("unstable or invalid timezone %q admitted", zone)
		}
	}
	badCompletion := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := NewItemPayload(ItemInput{Summary: "x", Completed: true, Due: NoDue(), CompletedAt: &badCompletion}); !IsCode(err, Invalid) {
		t.Fatal("year 10000 completion admitted")
	}
	badCompletion = time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	if _, err := NewItemPayload(ItemInput{Summary: "x", Completed: true, Due: NoDue(), CompletedAt: &badCompletion}); !IsCode(err, Invalid) {
		t.Fatal("submicrosecond completion admitted")
	}
	desc := ""
	withEmpty, err := NewItemPayload(ItemInput{Summary: "x", Description: &desc, Due: NoDue()})
	if err != nil {
		t.Fatal(err)
	}
	without, err := NewItemPayload(ItemInput{Summary: "x", Due: NoDue()})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("todo.brico")
	uid, _ := ParseItemUID("ics:presence@local")
	first, _ := NewLegacyItemPayload(uid, 0, withEmpty)
	second, _ := NewLegacyItemPayload(uid, 0, without)
	a, err := itemSourceHash(alias, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := itemSourceHash(alias, second)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("present empty description collapsed into absence")
	}
	forged := first
	forged.item.completed = true
	outside := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	forged.item.completedAt = &outside
	if _, err := itemSourceHash(alias, forged); !IsCode(err, Invalid) {
		t.Fatal("JSON serialization failure hashed empty fallback")
	}
	forged = first
	forged.item.due = Due{kind: dueInstant, instant: outside, zone: "Etc/UTC"}
	if _, err := itemSourceHash(alias, forged); !IsCode(err, Invalid) {
		t.Fatal("invalid due conversion hashed fallback")
	}
}
