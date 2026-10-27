package authorization

import (
	"errors"
	"testing"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
)

func TestNHF014MediaReadNeedsItsOwnExactObjectGrantEvenForAdmin(t *testing.T) {
	permission := testPermission(t, auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ)
	principal := testPrincipal(t, testAccount(t, "account"))
	object := testEntity(t, "reader", "home.media.v1.MediaAction", "media")
	for _, role := range []auth.AccountRole{auth.AccountRole_ACCOUNT_ROLE_USER, auth.AccountRole_ACCOUNT_ROLE_ADMIN} {
		state := testState(t, testSource(t, allEquipment), role, 1, 1)
		if err := Evaluate(principal, state, permission, object); !errors.Is(err, ErrDenied) {
			t.Fatal("equipment grant exposed media without its own scope", role, err)
		}
		policy := testSource(t, `  - scope: {family: media, operation: read}
    objects:
      entities:
        explicit:
          - entity_id: reader
            capabilities:
              explicit: [{schema_id: home.media.v1.MediaAction, endpoint: media}]
`)
		state = testState(t, policy, role, 2, 2)
		if err := Evaluate(principal, state, permission, object); err != nil {
			t.Fatal(role, err)
		}
		for _, other := range []Object{testEntity(t, "another-reader", "home.media.v1.MediaAction", "media"), testEntity(t, "reader", "home.media.v1.MediaAction", "other"), testEntity(t, "reader", "core.binary.v1", "media")} {
			if err := Evaluate(principal, state, permission, other); !errors.Is(err, ErrDenied) {
				t.Fatal("media scope escaped exact entity/capability slot", err)
			}
		}
	}
}

func TestNHF014MediaWriteOrAdministerAreNotConfigurableGrants(t *testing.T) {
	for _, operation := range []string{"write", "administer"} {
		raw := []byte("schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: media, operation: " + operation + "}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
		if _, err := AdmitPolicySource(testAccount(t, "account"), raw); err == nil {
			t.Fatal("unsupported media mutation grant admitted", operation)
		}
	}
}
