package lists

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
	"strings"
	"time"
)

// ItemField is the closed set of editable task fields. Ordering, identities,
// completion timestamps and revisions are never writable payload fields.
type ItemField uint8

const (
	ItemSummary ItemField = iota + 1
	ItemCompleted
	ItemDescription
	ItemDue
	ItemDueDate
	ItemDueInstant
)

// ItemPatch is admitted before SQL. Its private mask prevents mass assignment;
// omitted fields retain the value read under the mutation's existing SQL lock.
type ItemPatch struct {
	fields uint8
	input  ItemInput
}

func NewItemPatch(input ItemInput, fields []ItemField) (ItemPatch, error) {
	var mask uint8
	for _, field := range fields {
		if field < ItemSummary || field > ItemDueInstant {
			return ItemPatch{}, failure(Invalid)
		}
		mask |= 1 << (field - 1)
	}
	if mask == 0 || input.CompletedAt != nil {
		return ItemPatch{}, failure(Invalid)
	}
	if mask&1 != 0 && !validText(input.Summary, MaxNameBytes, true) {
		return ItemPatch{}, failure(Invalid)
	}
	if mask&4 != 0 && input.Description != nil && !validDescription(*input.Description) {
		return ItemPatch{}, failure(Invalid)
	}
	if mask&(8|16|32) != 0 {
		if _, _, _, err := input.Due.Values(); err != nil {
			return ItemPatch{}, failure(Invalid)
		}
	}
	input.Summary = strings.Clone(input.Summary)
	if input.Description != nil {
		text := strings.Clone(*input.Description)
		input.Description = &text
	}
	return ItemPatch{mask, input}, nil
}
func (p ItemPatch) apply(current ItemSnapshot) (ItemPayload, error) {
	if p.fields == 0 {
		return ItemPayload{}, failure(Invalid)
	}
	input := ItemInput{Summary: current.payload.summary, Completed: current.payload.completed, Description: current.payload.description, Due: current.payload.due, CompletedAt: current.payload.completedAt}
	if p.fields&1 != 0 {
		input.Summary = p.input.Summary
	}
	if p.fields&2 != 0 {
		input.Completed = p.input.Completed
		if !input.Completed {
			input.CompletedAt = nil
		} else if input.CompletedAt == nil && current.origin != ShoppingList {
			now := time.Now().UTC().Truncate(time.Microsecond)
			input.CompletedAt = &now
		}
	}
	if p.fields&4 != 0 {
		input.Description = p.input.Description
	}
	date := p.fields&(8|16) != 0
	instant := p.fields&(8|32) != 0
	switch {
	case date && instant:
		input.Due = p.input.Due
	case date:
		if p.input.Due.kind == dueDate {
			input.Due = p.input.Due
		} else if input.Due.kind == dueDate {
			input.Due = NoDue()
		}
	case instant:
		if p.input.Due.kind == dueInstant {
			input.Due = p.input.Due
		} else if input.Due.kind == dueInstant {
			input.Due = NoDue()
		}
	}
	return NewItemPayload(input)
}

// PatchItem performs merge and CAS within the existing account/policy fence
// and list/item locks; no read-before-write race or mutation replay is added.
func (s *Service) PatchItem(ctx context.Context, id domain.ListID, uid ItemUID, list ListPrecondition, item ItemPrecondition, patch ItemPatch) (MutationReceipt, error) {
	return s.editItem(ctx, id, uid, list, item, patch.apply)
}
