package configactivation

import (
	"github.com/damione1/Homeward/internal/storage"
	"testing"
)

// Requires the actual delivered SQL26. This fixture must not run against a
// projected parent or be counted as a passing pure/baseline gate.
func TestNHA055RealSQLGroupedBindingsAllowSameEntryAndRejectCrossGroupIDReuse(t *testing.T) {
	store, ctx, _ := nha029Store(t)
	first, err := storage.DashboardSourceDescriptor("dashboard.nha055_first", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.DashboardSourceDescriptor("dashboard.nha055_second", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	firstGroup := nha055SourceGroup(t, first, "source.dashboard.first", "# first\nviews: []\n")
	secondGroup := nha055SourceGroup(t, second, "source.dashboard.second", "# second\nviews: []\n")
	author, err := storage.NewConfigAuthor("nha055:grouped-real-sql")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha055:grouped-real-sql")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	var refs [2]storage.ConfigVersionRef
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		for index, group := range []SourceGroup{firstGroup, secondGroup} {
			if err := tx.ConfirmEditableSourceVersion(group.descriptor, group.bindings[0], storage.ConfigVersionRevision{}, [32]byte{}, true); err != nil {
				return err
			}
			ref, err := tx.SaveEditableSource(group.descriptor, group.bindings[0], group.files[0], metadata)
			if err != nil {
				return err
			}
			refs[index] = ref
		}
		return nil
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("actual SQL26 did not admit separate identities with the same native entry", err)
	}
	for index, group := range []SourceGroup{firstGroup, secondGroup} {
		if err := store.CheckEditableSourceBinding(ctx, group.descriptor, group.bindings[0], refs[index].Revision(), group.files[0].SHA256(), false); err != nil {
			t.Fatal("grouped revision CAS did not retain exact identity", err)
		}
	}
	if err := store.CheckEditableSourceBinding(ctx, second, firstGroup.bindings[0], refs[0].Revision(), firstGroup.files[0].SHA256(), false); err == nil {
		t.Fatal("existing document ID moved into another source identity")
	}
}
