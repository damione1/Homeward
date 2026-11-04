package lists

import (
	"testing"
	"time"
)

func TestNHA202ItemPatchRetainsUnselectedFieldsAndClearsExplicitValues(t *testing.T) {
	description := "retained"
	due, _ := NewDueDate("2026-11-03")
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	original, _ := NewItemPayload(ItemInput{Summary: "original", Completed: true, Description: &description, Due: due, CompletedAt: &at})
	current := ItemSnapshot{origin: Native, payload: original}
	patch, err := NewItemPatch(ItemInput{Completed: false, Due: NoDue()}, []ItemField{ItemCompleted})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := patch.apply(current)
	if err != nil {
		t.Fatal(err)
	}
	if updated.completed || updated.completedAt != nil || updated.summary != "original" || updated.description == nil || *updated.description != "retained" || updated.due != due {
		t.Fatal("selected false did not clear completion or altered unselected content")
	}
	patch, err = NewItemPatch(ItemInput{Due: NoDue()}, []ItemField{ItemDescription, ItemDue})
	if err != nil {
		t.Fatal(err)
	}
	updated, err = patch.apply(current)
	if err != nil || updated.description != nil || updated.due.Present() || !updated.completed || updated.completedAt == nil {
		t.Fatal("explicit due/description clearing changed other fields")
	}
}
func TestNHA202PatchRejectsZeroAndUnknownFieldsAndOwnsCompletionTime(t *testing.T) {
	for _, fields := range [][]ItemField{nil, {0}, {255}} {
		if _, err := NewItemPatch(ItemInput{Due: NoDue()}, fields); err == nil {
			t.Fatal("unadmitted mask entered domain")
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := NewItemPatch(ItemInput{Summary: "value", Due: NoDue(), CompletedAt: &now}, []ItemField{ItemSummary}); err == nil {
		t.Fatal("caller-assigned readonly completion time")
	}
	payload, _ := NewItemPayload(ItemInput{Summary: "value", Due: NoDue()})
	current := ItemSnapshot{origin: Native, payload: payload}
	patch, _ := NewItemPatch(ItemInput{Completed: true, Due: NoDue()}, []ItemField{ItemCompleted})
	updated, err := patch.apply(current)
	if err != nil || !updated.completed || updated.completedAt == nil || updated.completedAt.Nanosecond()%1000 != 0 {
		t.Fatal("completion timestamp missing or not PostgreSQL microseconds")
	}
	current.origin = ShoppingList
	updated, err = patch.apply(current)
	if err != nil || updated.completedAt != nil {
		t.Fatal("shopping list acquired unsupported timestamp")
	}
}
