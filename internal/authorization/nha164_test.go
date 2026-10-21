package authorization

import (
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"testing"
)

func TestNHA164HistoryIsEntityReadOnly(t *testing.T) {
	for _, operation := range []auth.ScopeOperation{auth.ScopeOperation_SCOPE_OPERATION_READ, auth.ScopeOperation_SCOPE_OPERATION_WRITE, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER} {
		permission, err := NewPermission(auth.ScopeFamily_SCOPE_FAMILY_HISTORY, operation)
		if err != nil {
			t.Fatal(err)
		}
		want := objectInvalid
		if operation == auth.ScopeOperation_SCOPE_OPERATION_READ {
			want = objectEntity
		}
		if got := configurable(permission); got != want {
			t.Fatalf("history operation %v maps to %v", operation, got)
		}
	}
}
