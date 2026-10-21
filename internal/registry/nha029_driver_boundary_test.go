package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type nha029DriverRewriteProbe struct{ called bool }

func (p *nha029DriverRewriteProbe) RewriteQuery(_ context.Context, _ *pgx.Conn, _ string, _ []any) (string, []any, error) {
	p.called = true
	return "SELECT 'nha029_readonly_probe'", nil, nil
}

type nha029DriverScanProbe struct{ called bool }

func (p *nha029DriverScanProbe) ScanRow(_ pgx.Rows) error { p.called = true; return nil }

type nha029UntypedStatementExec interface {
	Exec(storage.RegistryStatement, ...any) (pgconn.CommandTag, error)
}
type nha029UntypedStatementQuery interface {
	QueryRow(storage.RegistryStatement, ...any) pgx.Row
}

func nha029RequireInvalidDriverValue(t *testing.T, err error) {
	t.Helper()
	var typed *storage.Error
	if !errors.As(err, &typed) || typed.Kind() != storage.ConfigInvalid {
		t.Errorf("driver value was not rejected as ConfigInvalid: %v", err)
	}
}

// These probes only record invocation and return harmless fixture data. No
// transaction-control SQL or connection operation is performed by a probe.
func TestNHA029RegistryBorrowRejectsDriverCallbacks(t *testing.T) {
	for _, mode := range []string{"exec_parameter", "query_parameter", "scan_destination"} {
		t.Run(mode, func(t *testing.T) {
			store, ctx, _ := nha015Store(t)
			repo := nha015Repository(t, store)
			_, sink, err := fake.NewCommandFixture()
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.AttachProjection(ctx, sink); err != nil {
				t.Fatal(err)
			}
			gate, err := repo.BeginActivation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			instance := nha015Instance(t, "mqtt-main")
			key := nha015Source(t, "nha029-driver-value-boundary", "switch")
			const alias = "switch.arc_soundbar_crossfade"
			refused := errors.New("NHA029 fixture rollback after boundary probe")
			outcome, err := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
				if err := gate.ApplyInConfigTx(config, func(writer *registry.ConfigWriter) error {
					_, err := writer.ConfigureBinding(instance, key, alias)
					return err
				}); err != nil {
					return err
				}
				return config.WithRegistryBorrow(store, func(borrow *storage.RegistryBorrow) error {
					switch mode {
					case "exec_parameter":
						if port, exposed := any(borrow).(nha029UntypedStatementExec); exposed {
							probe := &nha029DriverRewriteProbe{}
							_, err := port.Exec(storage.RegistryNewDevice(), probe)
							nha029RequireInvalidDriverValue(t, err)
							if probe.called {
								t.Error("unadmitted query callback reached driver")
							}
						}
					case "query_parameter":
						if port, exposed := any(borrow).(nha029UntypedStatementQuery); exposed {
							probe := &nha029DriverRewriteProbe{}
							var value string
							err := port.QueryRow(storage.RegistryManifestEntity(), probe).Scan(&value)
							nha029RequireInvalidDriverValue(t, err)
							if probe.called {
								t.Error("unadmitted query callback reached driver")
							}
						}
					case "scan_destination":
						probe := &nha029DriverScanProbe{}
						err := borrow.QueryRow(storage.RegistryManifestEntity(), alias).Scan(probe)
						nha029RequireInvalidDriverValue(t, err)
						if probe.called {
							t.Error("unadmitted row callback reached driver")
						}
					}
					return refused
				})
			})
			if outcome != storage.TransactionNotCommitted || !errors.Is(err, refused) {
				t.Fatalf("fixture rollback outcome=%v err=%v", outcome, err)
			}
			if err := gate.ResolveNotCommitted(outcome); err != nil {
				t.Fatal(err)
			}
			if err := gate.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
