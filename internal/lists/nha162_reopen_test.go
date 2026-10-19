package lists

import (
	"testing"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA162CommittedListAndItemSurviveStoreReopen(t *testing.T) {
	f := nha103Setup(t)
	ctx := f.as(t, f.admin)
	created, err := f.service.CreateList(ctx, mustList(t, "Synthetic durable list"))
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	item, err := NewItemPayload(ItemInput{Summary: "Synthetic durable item", Completed: completed, Due: NoDue()})
	if err != nil {
		t.Fatal(err)
	}
	added, err := f.service.AddItem(ctx, created.ListID(), listRev(t, created), item)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := added.ItemUID()
	if !ok {
		t.Fatal("committed add omitted item UID")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.store.Close()
	reopened, err := storage.Open(f.ctx, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	authority := authorization.NewAuthority()
	if err := authority.HydrateFrom(func(yield func(authorization.AccountState) error) error {
		return reopened.ForEachAuthorizationPolicy(f.ctx, yield)
	}); err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepository(reopened, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, authority)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	list, err := service.GetList(ctx, created.ListID())
	if err != nil || list.Name() != "Synthetic durable list" || list.Revision() != listRev(t, added) {
		t.Fatalf("reopened list lost content or revision: %v", err)
	}
	got, err := service.GetItem(ctx, created.ListID(), uid)
	if err != nil || got.Summary() != "Synthetic durable item" || got.Revision() != itemRev(t, added) || got.Completed() {
		t.Fatalf("reopened item lost content or revision: %v", err)
	}
}
