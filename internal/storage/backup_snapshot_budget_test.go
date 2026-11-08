package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type backupBudgetRows struct {
	pgx.Rows
	remaining int
	value     string
	closed    bool
}

func (r *backupBudgetRows) Next() bool {
	if r.remaining == 0 {
		return false
	}
	r.remaining--
	return true
}
func (r *backupBudgetRows) Scan(dst ...any) error { *dst[0].(*string) = r.value; return nil }
func (r *backupBudgetRows) Close()                { r.closed = true }
func (r *backupBudgetRows) Err() error            { return nil }

type backupBudgetTx struct {
	pgx.Tx
	rows []*backupBudgetRows
	next int
}

func (tx *backupBudgetTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if tx.next >= len(tx.rows) {
		return nil, errors.New("unexpected query")
	}
	r := tx.rows[tx.next]
	tx.next++
	return r, nil
}
func TestBackupCanonicalStreamingBudget(t *testing.T) {
	// Reuse one row buffer, as pgx streaming does: the test does not allocate a
	// snapshot-sized string. The former 512 MiB cap failed the first scenario.
	row := `{"value":"` + strings.Repeat("x", (1<<20)-12) + `"}`
	if len(row) != 1<<20 {
		t.Fatal("fixture row size")
	}
	for _, test := range []struct {
		name     string
		rows     int
		raw      string
		accepted bool
	}{
		{"above former limit", 513, row, true},
		{"over new limit", 2049, row, false},
		{"row count remains bounded", 2000001, `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			names := &backupBudgetRows{remaining: 1, value: "synthetic_history"}
			data := &backupBudgetRows{remaining: test.rows, value: test.raw}
			tx := &backupBudgetTx{rows: []*backupBudgetRows{names, data}}
			result, err := backupTables(context.Background(), tx)
			if test.accepted {
				if err != nil || len(result) != 1 || result[0].Rows != int64(test.rows) || len(result[0].SHA256) != 64 {
					t.Fatalf("streamed witness: %v %v", result, err)
				}
			} else {
				var failure *Error
				if !errors.As(err, &failure) || failure.kind != ConfigInvalid {
					t.Fatalf("expected budget rejection, got %v", err)
				}
			}
			if !names.closed || !data.closed {
				t.Fatal("rows left open")
			}
		})
	}
}
