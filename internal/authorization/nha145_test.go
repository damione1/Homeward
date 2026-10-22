package authorization

import (
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"testing"
)

func TestNHA145CredentialScopeIssuanceUsesCurrentAccount(t *testing.T) {
	source := testSource(t, allEquipment)
	user := testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1)
	read := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	accounts := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err := user.AdmitCredentialScopes([]Permission{read}); err != nil {
		t.Fatal(err)
	}
	if err := user.AdmitCredentialScopes([]Permission{accounts}); err == nil {
		t.Fatal("user minted administrative scope")
	}
	admin := testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_ADMIN, 1, 1)
	if err := admin.AdmitCredentialScopes([]Permission{accounts}); err != nil {
		t.Fatal(err)
	}
	if err := admin.AdmitCredentialScopes([]Permission{read, read}); err == nil {
		t.Fatal("duplicate scope admitted")
	}
	if err := (AccountState{}).AdmitCredentialScopes(nil); err == nil {
		t.Fatal("invalid state minted key")
	}
}
