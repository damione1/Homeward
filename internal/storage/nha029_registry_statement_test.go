package storage

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type nha029RegistryScanProbe struct{ called bool }

func (p *nha029RegistryScanProbe) ScanRow(pgx.Rows) error {
	p.called = true
	return nil
}

func nha029ConfigErrorKind(t *testing.T, err error, want Kind) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind() != want {
		t.Fatalf("config error kind=%v, want %v", err, want)
	}
}

func TestNHA029RegistryStatementsRejectZeroUnknownAndWrongOperation(t *testing.T) {
	queries := []RegistryStatement{
		RegistrySourceBinding(), RegistryManifestEntity(), RegistryDeviceSource(),
		RegistryEntityState(), RegistryAliasOwner(), RegistryEntityAliasCount(),
	}
	commands := []RegistryStatement{
		RegistryNewDevice(), RegistryNewDeviceSource(), RegistryNewEntity(),
		RegistryNewSourceBinding(), RegistryNewAlias(), RegistryClearAliases(),
		RegistryActivateAlias(), RegistryRetireEntity(),
	}
	for _, query := range queries {
		if sql, err := query.QuerySQL(); err != nil || sql == "" {
			t.Fatalf("admitted registry query missing: %v", err)
		}
		_, err := query.CommandSQL()
		nha029ConfigErrorKind(t, err, ConfigInvalid)
	}
	for _, command := range commands {
		if sql, err := command.CommandSQL(); err != nil || sql == "" {
			t.Fatalf("admitted registry command missing: %v", err)
		}
		_, err := command.QuerySQL()
		nha029ConfigErrorKind(t, err, ConfigInvalid)
	}
	for _, invalid := range []RegistryStatement{{}, {kind: registryStatementKind(255)}} {
		_, err := invalid.QuerySQL()
		nha029ConfigErrorKind(t, err, ConfigInvalid)
		_, err = invalid.CommandSQL()
		nha029ConfigErrorKind(t, err, ConfigInvalid)
	}
}

func TestNHA029RegistryBorrowPoisonsSwallowedInvalidStatement(t *testing.T) {
	store, ctx := nha151Store(t)
	cases := []struct {
		name      string
		statement RegistryStatement
		query     bool
	}{
		{"zero command", RegistryStatement{}, false},
		{"zero query", RegistryStatement{}, true},
		{"unknown command", RegistryStatement{kind: registryStatementKind(255)}, false},
		{"unknown query", RegistryStatement{kind: registryStatementKind(255)}, true},
		{"query used as command", RegistryManifestEntity(), false},
		{"command used as query", RegistryNewDevice(), true},
	}
	for _, item := range cases {
		outcome, err := store.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
			return config.WithRegistryBorrow(store, func(borrow *RegistryBorrow) error {
				if item.query {
					_ = borrow.QueryRow(item.statement) // an unscanned invalid row still poisons the owner
				} else {
					_, _ = borrow.Exec(item.statement)
				}
				return nil // deliberately swallow the statement error
			})
		})
		if outcome != TransactionNotCommitted {
			t.Fatalf("invalid registry statement committed: case=%s outcome=%v", item.name, outcome)
		}
		nha029ConfigErrorKind(t, err, TransactionFailed)
		var outer *Error
		if !errors.As(err, &outer) {
			t.Fatalf("missing storage transaction envelope: case=%s err=%v", item.name, err)
		}
		nha029ConfigErrorKind(t, outer.Unwrap(), ConfigInvalid)
	}
}

func TestNHA029RegistryBorrowAdmitsOnlyExactStringArgumentsAndPrimitiveResults(t *testing.T) {
	store, ctx := nha151Store(t)
	const device = "dev_nha029_registry_admission"
	const entity = "ent_nha029_registry_admission"
	const alias = "switch.arc_soundbar_crossfade"
	outcome, err := store.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
		return config.WithRegistryBorrow(store, func(borrow *RegistryBorrow) error {
			if _, err := borrow.Exec(RegistryNewDevice(), device); err != nil {
				return err
			}
			_, err := borrow.Exec(RegistryNewEntity(), entity, device, alias)
			return err
		})
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("fixture registry row was not committed: outcome=%v err=%v", outcome, err)
	}

	probe := &nha029RegistryScanProbe{}
	var nilString *string
	cases := []struct {
		name  string
		query bool
		args  []string
		dest  []any
	}{
		{"command without required parameter", false, nil, nil},
		{"query without required parameter", true, nil, []any{new(string)}},
		{"query with excess parameters", true, []string{alias, "2", "3", "4", "5"}, []any{new(string)}},
		{"nil destination", true, []string{alias}, []any{nil}},
		{"typed-nil destination", true, []string{alias}, []any{nilString}},
		{"unknown destination", true, []string{alias}, []any{new(int)}},
		{"wrong primitive shape", true, []string{alias}, []any{new(bool)}},
		{"row callback destination", true, []string{alias}, []any{probe}},
		{"too many destinations", true, []string{alias}, []any{new(string), new(string), new(string), new(string), new(string)}},
	}
	for _, item := range cases {
		outcome, err := store.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
			return config.WithRegistryBorrow(store, func(borrow *RegistryBorrow) error {
				if item.query {
					_ = borrow.QueryRow(RegistryManifestEntity(), item.args...).Scan(item.dest...)
				} else {
					_, _ = borrow.Exec(RegistryNewDevice(), item.args...)
				}
				return nil // deliberately swallow the invalid-input error
			})
		})
		if outcome != TransactionNotCommitted {
			t.Fatalf("invalid registry value committed: case=%s outcome=%v", item.name, outcome)
		}
		nha029ConfigErrorKind(t, err, TransactionFailed)
		var outer *Error
		if !errors.As(err, &outer) {
			t.Fatalf("missing transaction error: case=%s err=%v", item.name, err)
		}
		nha029ConfigErrorKind(t, outer.Unwrap(), ConfigInvalid)
	}
	if probe.called {
		t.Fatal("unadmitted RowScanner callback reached pgx")
	}

	// The refused live rows were consumed internally. A fresh ConfigTx on the
	// same Store still reads both primitive shapes successfully.
	outcome, err = store.InConfigTransactionOutcome(ctx, func(config *ConfigTx) error {
		return config.WithRegistryBorrow(store, func(borrow *RegistryBorrow) error {
			var gotEntity string
			if err := borrow.QueryRow(RegistryManifestEntity(), alias).Scan(&gotEntity); err != nil || gotEntity != entity {
				return errors.New("valid string result unavailable after rejected scans")
			}
			var retired bool
			var gotAlias string
			if err := borrow.QueryRow(RegistryEntityState(), entity).Scan(&retired, &gotAlias); err != nil || retired || gotAlias != alias {
				return errors.New("valid bool/string result unavailable after rejected scans")
			}
			return nil
		})
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("Store unusable after rejected Scan: outcome=%v err=%v", outcome, err)
	}
}
