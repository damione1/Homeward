package registry_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Optional interfaces keep the same rollback oracle when the raw-SQL surface
// is removed. Only this regression test attempts transaction-control SQL.
type nha029RawRegistryExec interface {
	Exec(string, ...any) (pgconn.CommandTag, error)
}
type nha029RawRegistryQuery interface {
	QueryRow(string, ...any) pgx.Row
}

func TestNHA029RegistryBorrowCannotCommitBehindOwner(t *testing.T) {
	for _, method := range []string{"Exec", "QueryRow"} {
		t.Run(method, func(t *testing.T) {
			store, ctx, settings := nha015Store(t)
			repo := nha015Repository(t, store)
			_, sink, err := fake.NewCommandFixture()
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.AttachProjection(ctx, sink); err != nil {
				t.Fatal(err)
			}
			instance := nha015Instance(t, "mqtt-main")
			key := nha015Source(t, "nha029-owner-only-commit", "switch")
			gate, err := repo.BeginActivation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			refused := errors.New("NHA029 refusal after borrowed registry writes")
			rawExecuted := false
			outcome, err := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
				if err := gate.ApplyInConfigTx(config, func(writer *registry.ConfigWriter) error {
					_, err := writer.ConfigureBinding(instance, key, "switch.arc_soundbar_crossfade")
					return err
				}); err != nil {
					return err
				}
				return config.WithRegistryBorrow(store, func(borrow *storage.RegistryBorrow) error {
					switch method {
					case "Exec":
						if raw, exposed := any(borrow).(nha029RawRegistryExec); exposed {
							if _, err := raw.Exec("COMMIT"); err != nil {
								return err
							}
							rawExecuted = true
						}
					case "QueryRow":
						if raw, exposed := any(borrow).(nha029RawRegistryQuery); exposed {
							if err := raw.QueryRow("COMMIT").Scan(); !errors.Is(err, pgx.ErrNoRows) {
								return errors.Join(errors.New("unexpected COMMIT row result"), err)
							}
							rawExecuted = true
						}
					}
					return refused
				})
			})
			t.Logf("raw SQL executed=%t; owner outcome=%v err=%v", rawExecuted, outcome, err)
			if outcome != storage.TransactionNotCommitted || !errors.Is(err, refused) {
				t.Fatalf("owner did not report definite callback refusal: outcome=%v err=%v", outcome, err)
			}
			if err := gate.ResolveNotCommitted(outcome); err != nil {
				t.Fatal(err)
			}
			if err := gate.Release(); err != nil {
				t.Fatal(err)
			}
			reopened, err := storage.Open(ctx, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			independent := nha015Repository(t, reopened)
			if got, err := independent.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrUnconfigured) {
				t.Fatalf("binding durable despite NotCommitted: rawSQL=%t identity=%v err=%v", rawExecuted, got, err)
			}
		})
	}
}
