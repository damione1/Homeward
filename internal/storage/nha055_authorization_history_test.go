package storage

import (
	"testing"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
)

// The policy pointer is independent from latest retained document metadata and
// generation membership. SQL24 already owns both authorities used here.
func TestNHA055ActivePolicyReadIgnoresUnactivatedRetainedVersion(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "NHA055 retained policy admin", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	active, err := s.AuthorizationPolicy(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := active.Version()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := AuthorizationPolicyBinding(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	var staged ConfigVersionRef
	outcome, err := s.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var err error
		staged, err = tx.SaveAuthorizationPolicy(nha050Source(t, admin.ID, true, "unactivated retained source"), nha151Metadata(t))
		return err
	})
	if err != nil || outcome != TransactionCommitted {
		t.Fatal("retained fixture write", err)
	}
	metadata, err := s.ReadConfigDocument(ctx, binding.ID())
	if err != nil || metadata.Ref() != staged || metadata.Active() {
		t.Fatal("fixture did not separate latest metadata from active policy", err)
	}
	actual, present, err := s.ReadActiveAuthorizationVersion(ctx, binding.ID())
	if err != nil || !present || actual != expected {
		t.Fatal("retained latest row hid or replaced true active policy", err)
	}
	other, _ := NewConfigDocumentID("operational.main")
	if _, present, err := s.ReadActiveAuthorizationVersion(ctx, other); err != nil || present {
		t.Fatal("non-policy document manufactured policy activation", err)
	}
}
