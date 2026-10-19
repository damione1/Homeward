package view_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

func nha050ScopedGrant(t *testing.T, f *watchFixture, revision uint64, all, readable []authorization.CapabilitySlot, binding string) view.Grant {
	t.Helper()
	access, err := view.NewEntityAccess(f.ids[0], all, readable)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := view.NewScopedGrant(f.actor, revision, sha256.Sum256([]byte(binding)), []view.EntityAccess{access})
	if err != nil {
		t.Fatal(err)
	}
	return grant
}
func TestNHA050PartialCompositePurgesOldViewAndHidesForbiddenSlot(t *testing.T) {
	f := newWatchFixture(t, 2)
	f.publish(t, 1, false)
	ctx := context.Background()
	_, key, err := f.bindings[0].Source()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.seq.Capture()
	if err != nil {
		t.Fatal(err)
	}
	public := proto.Clone(snapshot.Capabilities[f.ids[0]][0]).(*registry.CapabilityDescription)
	secret := proto.Clone(public).(*registry.CapabilityDescription)
	secret.Endpoint = "private"
	secret.DisplayName = "FORBIDDEN-SLOT-CANARY"
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{public, secret})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := f.session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	f.bindings = bindings
	f.publish(t, 0, true)
	allowed, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	forbidden, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "private")
	all := []authorization.CapabilitySlot{allowed, forbidden}
	firstAccess, err := view.NewEntityAccess(f.ids[0], all, all)
	if err != nil {
		t.Fatal(err)
	}
	secondAccess, err := view.NewEntityAccess(f.ids[1], []authorization.CapabilitySlot{allowed}, []authorization.CapabilitySlot{allowed})
	if err != nil {
		t.Fatal(err)
	}
	full, err := view.NewScopedGrant(f.actor, 1, sha256.Sum256([]byte("full")), []view.EntityAccess{firstAccess, secondAccess})
	if err != nil {
		t.Fatal(err)
	}
	f.auth.SetGrant(full)
	filter, err := view.Only(f.ids)
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.service.OpenView(ctx, f.actor, filter, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldWire, err := view.EncodePage(old)
	if err != nil || len(oldWire.States) != 1 || old.NextPageToken == "" {
		t.Fatal("positive full view lost state", err)
	}
	watch := openWatch(t, f, oldWire.Cursor, f.ids...)
	defer watch.Close()
	f.auth.SetGrant(nha050ScopedGrant(t, f, 2, all, []authorization.CapabilitySlot{allowed}, "partial"))
	frame := nextFrame(t, watch)
	if frame.GetResyncRequired().GetReason() != statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED {
		t.Fatal("old full view was not invalidated", frame)
	}
	if _, err := f.service.ReadPage(ctx, f.actor, old.SnapshotToken, old.NextPageToken); !errors.Is(err, view.ErrViewChanged) {
		t.Fatal("old cached composite remained accessible", err)
	}
	partialFilter, _ := view.Only(f.ids[:1])
	partial := f.page(t, partialFilter)
	if len(partial.Entities) != 1 || len(partial.States) != 0 || partial.Entities[0].State != nil || len(partial.Capabilities) != 1 || partial.Capabilities[0].Endpoint != "relay" {
		t.Fatal("partial composite or forbidden capability exposed", partial)
	}
	encoded, err := proto.Marshal(partial)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("FORBIDDEN-SLOT-CANARY")) || bytes.Contains(encoded, []byte("private")) {
		t.Fatal("forbidden metadata sentinel leaked")
	}
}
func TestNHA050ScopedMonocapPositiveAndCredentialCursorBinding(t *testing.T) {
	f := newWatchFixture(t, 1)
	f.publish(t, 0, false)
	slot, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	all := []authorization.CapabilitySlot{slot}
	f.auth.SetGrant(nha050ScopedGrant(t, f, 1, all, all, "credential-one"))
	filter, _ := view.Only(f.ids)
	page := f.page(t, filter)
	if len(page.States) != 1 || page.Entities[0].State == nil || page.States[0].GetSwitchOn() {
		t.Fatal("authorized monocap false state disappeared", page)
	}
	f.auth.SetGrant(nha050ScopedGrant(t, f, 1, all, all, "credential-two"))
	watch := openWatch(t, f, page.Cursor, f.ids...)
	defer watch.Close()
	if reason := nextFrame(t, watch).GetResyncRequired().GetReason(); reason != statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED {
		t.Fatal("same IDs reused a different credential cursor", reason)
	}
}
