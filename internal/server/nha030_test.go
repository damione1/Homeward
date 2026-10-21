package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha030ProjectedIdentity struct{}

func (nha030ProjectedIdentity) Resolve(context.Context, domain.IntegrationInstanceID, []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	return nil, ErrIdentityNotConfigured
}

func (nha030ProjectedIdentity) AttachProjection(ctx context.Context, sink integrations.ProjectionSink) error {
	return sink.PublishRegistry(ctx, nil)
}

func nha030ServerConfig(t *testing.T, port int) nha029RunFixture {
	t.Helper()
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", shortPrivateDataDir(t), port))
	return nha029RunFixtureFromRaw(t, data)
}

func nha030ServerObserver(t *testing.T, port int) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	configuration, err := pgx.ParseConfig("host=127.0.0.1 port=" + strconv.Itoa(port) + " dbname=postgres user=homeward_test sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	configuration.Password = testpg.Password()
	conn, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func nha030EpochState(conn *pgx.Conn) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int64
	var clean sql.NullBool
	if err := conn.QueryRow(ctx, `SELECT count(*),bool_and(clean_close) FROM public.recorder_epochs`).Scan(&count, &clean); err != nil {
		return 0, false, err
	}
	return count, clean.Valid && clean.Bool, nil
}

func TestNHA030RunAndFailurePathsOwnRecorderEpoch(t *testing.T) {
	for _, mode := range []string{"default", "factory-error", "configure-error", "configure-authorization-error", "cancel-before-start", "listener-error"} {
		t.Run(mode, func(t *testing.T) {
			cluster := testpg.Start(t)
			cfg := nha030ServerConfig(t, cluster.Port)
			observer := nha030ServerObserver(t, cluster.Port)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("NHA030 deliberate runtime failure")
			calledAuthorization := false
			var listen ListenFunc = (&net.ListenConfig{}).Listen
			if mode == "listener-error" {
				listen = func(context.Context, string, string) (net.Listener, error) { return nil, failure }
			}
			factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
				count, clean, err := nha030EpochState(observer)
				if err != nil {
					return RuntimePlan{}, err
				}
				if count != 1 || clean {
					return RuntimePlan{}, fmt.Errorf("Recorder was not prepared before factory: count=%d clean=%v", count, clean)
				}
				if mode == "factory-error" {
					return RuntimePlan{}, failure
				}
				if mode == "cancel-before-start" {
					cancel()
				}
				plan := RuntimePlan{Identity: unconfiguredIdentity{}}
				if mode == "configure-error" {
					plan.Configure = func(context.Context, *integrations.Manager, *storage.Store) error { return failure }
				}
				if mode == "configure-authorization-error" {
					plan.Identity = nha030ProjectedIdentity{}
					plan.ConfigureAuthorization = func(context.Context, AuthorizationComposition) error {
						calledAuthorization = true
						return failure
					}
				}
				return plan, nil
			})
			if mode == "default" {
				factory = nil
			}
			ready := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				if mode == "default" {
					result <- RunWithStartup(ctx, cfg.startup, listen, func(net.Addr) { close(ready) })
				} else {
					result <- RunWithStartupFactory(ctx, cfg.startup, listen, func(net.Addr) { close(ready) }, factory)
				}
			}()
			finished := false
			t.Cleanup(func() {
				if finished {
					return
				}
				cancel()
				select {
				case <-result:
				case <-time.After(12 * time.Second):
					t.Error("Run did not finish during cleanup")
				}
			})
			if mode == "default" {
				select {
				case <-ready:
					count, clean, err := nha030EpochState(observer)
					if err != nil {
						t.Fatal(err)
					}
					if count != 1 || clean {
						t.Fatalf("Run did not own live Recorder: count=%d clean=%v", count, clean)
					}
					cancel()
				case err := <-result:
					finished = true
					t.Fatalf("Run exited before ready: %v", err)
				case <-time.After(8 * time.Second):
					t.Fatal("Run did not become ready")
				}
			}
			select {
			case err := <-result:
				finished = true
				switch mode {
				case "default":
					if err != nil {
						t.Fatal(err)
					}
				case "cancel-before-start":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
				default:
					if !errors.Is(err, failure) {
						t.Fatalf("runtime failure lost: %v", err)
					}
				}
			case <-time.After(12 * time.Second):
				t.Fatal("Run did not release its Recorder owner")
			}
			count, clean, err := nha030EpochState(observer)
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 || !clean {
				t.Fatalf("Recorder epoch leaked or closed uncleanly after %s: count=%d clean=%v", mode, count, clean)
			}
			if mode == "configure-authorization-error" && !calledAuthorization {
				t.Fatal("Run did not reach ConfigureAuthorization")
			}
		})
	}
}

func TestNHA030RunStopsBoundedlyWhenPostgresDisappears(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	result := make(chan error, 1)
	var listener net.ListenConfig
	go func() { result <- RunWithStartup(ctx, cfg.startup, listener.Listen, func(net.Addr) { close(ready) }) }()
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		cancel()
		select {
		case <-result:
		case <-time.After(12 * time.Second):
			t.Error("Run did not finish during PG outage cleanup")
		}
	})
	select {
	case <-ready:
	case err := <-result:
		finished = true
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not become ready")
	}
	if err := cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		finished = true
		if !errors.Is(err, recorder.ErrDrainIncomplete) {
			t.Fatalf("PG loss falsely ended with a clean Recorder drain: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Run waited indefinitely for unavailable Recorder SQL")
	}
}
