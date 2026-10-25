package view_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHF006ActionPermissionsAreSnapshotFactsAndWriteWithdrawalInvalidatesReadView(t *testing.T) {
	f := newWatchFixture(t, 2)
	slot, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	slots := []authorization.CapabilitySlot{slot}
	grant := func(allow bool) view.Grant {
		t.Helper()
		var writable []authorization.CapabilitySlot
		if allow {
			writable = slots
		}
		entries := make([]view.EntityAccess, 0, len(f.ids))
		for _, id := range f.ids {
			entry, err := view.NewEntityAccessWithActions(id, slots, slots, writable)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry)
		}
		// Even a bad authorizer reusing its revision/binding cannot silently
		// change write metadata under a captured cursor: the digest binds it.
		result, err := view.NewScopedGrant(f.actor, 1, sha256.Sum256([]byte("same policy binding")), entries)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	f.auth.SetGrant(grant(true))
	filter, _ := view.Only(f.ids)
	page, err := f.service.OpenView(context.Background(), f.actor, filter, 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := view.EncodePage(page)
	if err != nil || len(encoded.Entities) != 1 || len(encoded.Entities[0].ActionAccess) != 1 || encoded.Entities[0].ActionAccess[0].Access != statewire.ActionAccess_ACTION_ACCESS_ALLOWED {
		t.Fatal("missing explicit write permission", err)
	}
	cloned := page.Items[0].ActionAccess()
	cloned[0].Access = statewire.ActionAccess_ACTION_ACCESS_DENIED
	again, err := view.EncodePage(page)
	if err != nil || again.Entities[0].ActionAccess[0].Access != statewire.ActionAccess_ACTION_ACCESS_ALLOWED {
		t.Fatal("caller mutated snapshot permission")
	}
	watch := openWatch(t, f, encoded.Cursor, f.ids...)
	defer watch.Close()
	f.auth.SetGrant(grant(false))
	if nextFrame(t, watch).GetResyncRequired().GetReason() != statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED {
		t.Fatal("write withdrawal did not invalidate stream")
	}
	if _, err := f.service.ReadPage(context.Background(), f.actor, page.SnapshotToken, page.NextPageToken); !errors.Is(err, view.ErrViewChanged) {
		t.Fatal("old write metadata survived", err)
	}
	current := f.page(t, filter)
	if len(current.Entities) != 1 || len(current.Entities[0].ActionAccess) != 1 || current.Entities[0].ActionAccess[0].Access != statewire.ActionAccess_ACTION_ACCESS_DENIED {
		t.Fatal("read permission incorrectly implied write permission")
	}
	tail, err := f.service.ReadPage(context.Background(), f.actor, current.SnapshotToken, current.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	tailWire, err := view.EncodePage(tail)
	if err != nil || len(tailWire.Entities) != 1 || len(tailWire.Entities[0].ActionAccess) != 1 || tailWire.Entities[0].ActionAccess[0].Access != statewire.ActionAccess_ACTION_ACCESS_DENIED {
		t.Fatal("remaining readable entity lost explicit denial", err)
	}
}

func TestNHF006LegacyPermissionIsUnknownAndForbiddenSlotsCannotBeAdvertised(t *testing.T) {
	f := newWatchFixture(t, 1)
	slot, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	private, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "private")
	slots := []authorization.CapabilitySlot{slot}
	if _, err := view.NewEntityAccessWithActions(f.ids[0], append(slots, private), slots, []authorization.CapabilitySlot{private}); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal("advertised unreadable slot")
	}
	if _, err := view.NewEntityAccessWithActions(f.ids[0], slots, slots, []authorization.CapabilitySlot{slot, slot}); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal("accepted duplicate slot")
	}
	entry, err := view.NewEntityAccess(f.ids[0], slots, slots)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := view.NewScopedGrant(f.actor, 1, sha256.Sum256([]byte("legacy")), []view.EntityAccess{entry})
	if err != nil {
		t.Fatal(err)
	}
	f.auth.SetGrant(grant)
	filter, _ := view.Only(f.ids)
	page := f.page(t, filter)
	if len(page.Entities) != 1 || len(page.Entities[0].ActionAccess) != 0 {
		t.Fatal("invented permission for old authority")
	}
}
