package authorization

import (
	"encoding/hex"
	"errors"
	"testing"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

// These fixtures predate LISTS. Parent ba7f digests are pinned below so the
// compatibility check does not use the new semantic encoder as its oracle.
const nha103OldAllPolicy = `schema: homeward.authorization/1
account_id: account
grants:
  - scope: {family: dashboards, operation: read}
    objects: {dashboards: {all: {}}}
  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`

const nha103OldMixedPolicy = `schema: homeward.authorization/1
account_id: account
grants:
  - scope: {family: actions, operation: write}
    objects:
      entities:
        explicit:
          - entity_id: entity
            capabilities: {explicit: [{schema_id: core.light.v1, endpoint: main}]}
  - scope: {family: dashboards, operation: read}
    objects: {dashboards: {own: {}}}
  - scope: {family: dashboards, operation: write}
    objects: {dashboards: {explicit: [dashboard]}}
`

func TestNHA103OldPolicyHashesStayByteIdentical(t *testing.T) {
	account := testAccount(t, "account")
	for _, fixture := range []struct {
		name, raw, sourceSHA, semanticSHA string
	}{
		{"all", nha103OldAllPolicy, "577f97dbbf417e9d30f4761ccafd013f4da969acb4aef4605915f0ba07ec5c99", "a2cd23d0ceae38d65d24ff38db6be678541771feb5c56ed534b61cf7d457eef4"},
		{"mixed", nha103OldMixedPolicy, "562b2bbc85691bd77fd5406121533c925f16296f321a3ee55d19480449437e5d", "c6dc8f1186c4a3a7d90a0ed08ef28a6abb4bb50efc1068d5765fb48a3955cf0b"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source, err := AdmitPolicySource(account, []byte(fixture.raw))
			if err != nil {
				t.Fatal(err)
			}
			gotSource, _ := source.SHA256()
			if hex.EncodeToString(gotSource[:]) != fixture.sourceSHA {
				t.Fatalf("old /1 source hash changed: %x", gotSource)
			}
			gotSemantic, _ := source.SemanticSHA256()
			if hex.EncodeToString(gotSemantic[:]) != fixture.semanticSHA {
				t.Fatalf("old /1 semantic hash changed: %x", gotSemantic)
			}
		})
	}
}

// TestNHA103ListsPolicyAdmission characterizes the missing LISTS policy seam
// before the NHA-103 authorization and domain implementation.
func TestNHA103ListsPolicyAdmission(t *testing.T) {
	account := testAccount(t, "account")
	source := []byte(`schema: homeward.authorization/1
account_id: account
grants:
  - scope: {family: lists, operation: read}
    objects: {lists: {explicit: [list-one]}}
  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`)
	if _, err := AdmitPolicySource(account, source); err != nil {
		t.Fatalf("LISTS read/explicit and write/all policy should be admitted: %v", err)
	}
}

func TestNHA103ListsSelectionAndProof(t *testing.T) {
	account := testAccount(t, "account")
	source := testSource(t, `  - scope: {family: lists, operation: read}
    objects: {lists: {explicit: [list-one]}}
  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`)
	authority := NewAuthority()
	if err := authority.Hydrate([]AccountState{testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1)}); err != nil {
		t.Fatal(err)
	}
	principal := testPrincipal(t, account)
	read := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_LISTS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	write := testPermission(t, authv1.ScopeFamily_SCOPE_FAMILY_LISTS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	idA, _ := domain.ParseListID("list-one")
	idB, _ := domain.ParseListID("list-two")
	objectA, _ := ListObject(idA)
	objectB, _ := ListObject(idB)
	if err := Evaluate(principal, testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1), read, objectA); err != nil {
		t.Fatal(err)
	}
	if err := Evaluate(principal, testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1), read, objectB); !errors.Is(err, ErrDenied) {
		t.Fatalf("unlisted read: %v", err)
	}
	if err := Evaluate(principal, testState(t, source, authv1.AccountRole_ACCOUNT_ROLE_USER, 1, 1), write, objectB); err != nil {
		t.Fatal(err)
	}
	permit, err := authority.Permit(t.Context(), principal, read, ListCollectionObject())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	evidence, selection, err := permit.ListEvidence()
	if err != nil || selection.All() {
		t.Fatalf("selection: %v", err)
	}
	ids, err := selection.IDs()
	if err != nil || len(ids) != 1 || ids[0] != idA {
		t.Fatalf("explicit ids: %v", err)
	}
	ids[0] = idB
	again, _ := selection.IDs()
	if again[0] != idA {
		t.Fatal("selection exposed mutable slice")
	}
	semantic, _ := source.SemanticSHA256()
	if !evidence.MatchesSQL(2, 1, 1, 1, semantic[:]) || evidence.MatchesSQL(2, 1, 2, 1, semantic[:]) {
		t.Fatal("SQL proof mismatch")
	}
	if _, err := AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: lists, operation: read}\n    objects: {lists: {own: {}}}\n")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("own list admitted: %v", err)
	}
	if _, err := AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: account\ngrants:\n  - scope: {family: lists, operation: administer}\n    objects: {lists: {all: {}}}\n")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("administer admitted: %v", err)
	}
}
