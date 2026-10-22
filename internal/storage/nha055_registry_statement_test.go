package storage

import (
	"errors"
	"testing"
)

func TestNHA055CompilationAliasQueryRejectsNonPrimitiveDestinations(t *testing.T) {
	store, ctx := nha151Store(t)
	probe := &nha029RegistryScanProbe{}
	var nilString *string
	for _, test := range []struct {
		name string
		dest any
	}{
		{"nil", nil}, {"typed-nil", nilString}, {"row-scanner", probe}, {"wrong-primitive", new(bool)},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := store.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
				return config.WithRegistryBorrow(store, func(borrow *RegistryBorrow) error {
					_ = borrow.QueryRow(RegistryEntityAliasCount(), "ent_nha055_nonexistent").Scan(test.dest)
					return nil
				})
			})
			if outcome != TransactionNotCommitted {
				t.Fatalf("rejected scan committed: outcome=%v err=%v", outcome, err)
			}
			nha029ConfigErrorKind(t, err, TransactionFailed)
			var outer *Error
			if !errors.As(err, &outer) {
				t.Fatal("missing transaction failure envelope")
			}
			nha029ConfigErrorKind(t, outer.Unwrap(), ConfigInvalid)
		})
	}
	if probe.called {
		t.Fatal("untrusted RowScanner reached the SQL borrow")
	}
}
