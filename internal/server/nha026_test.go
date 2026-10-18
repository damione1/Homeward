package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha026ServerPilot struct {
	started chan struct{}
	starts  atomic.Int32
	stopped atomic.Int32
	joined  atomic.Int32
}

func (p *nha026ServerPilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	p.starts.Add(1)
	health, err := integrations.NewHealthReport(integrations.HealthReady, "")
	if err != nil {
		return err
	}
	if err := host.ReportHealth(ctx, health); err != nil {
		return err
	}
	if err := tasks.Go(func(ctx context.Context) error { <-ctx.Done(); p.joined.Add(1); return nil }); err != nil {
		return err
	}
	close(p.started)
	return nil
}
func (p *nha026ServerPilot) Stop(context.Context) error { p.stopped.Add(1); return nil }

func TestNHA026RunWithFactoryUsesProductionDatabaseListenerAndSupervisorPath(t *testing.T) {
	cluster := testpg.Start(t)
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", cluster.Port))
	configValue, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	pilot := &nha026ServerPilot{started: make(chan struct{})}
	instance, err := domain.ParseIntegrationInstanceID("nha026-server-fixture")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	factoryCalled := make(chan struct{})
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		if store == nil {
			t.Error("factory did not receive migrated Store")
		}
		close(factoryCalled)
		return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, configValue, lc.Listen, func(addr net.Addr) { ready <- addr }, factory)
	}()
	for _, ch := range []<-chan struct{}{factoryCalled, pilot.started} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("runtime did not assemble/start")
		}
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener never became ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if pilot.starts.Load() != 1 || pilot.stopped.Load() != 1 || pilot.joined.Load() != 1 {
		t.Fatalf("pilot Start/Stop/join = %d/%d/%d", pilot.starts.Load(), pilot.stopped.Load(), pilot.joined.Load())
	}
}

type nha026ServerGeneration struct{}

func (nha026ServerGeneration) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

func TestNHA026RunOwnsPreparedDurableServiceThroughFailureAndShutdown(t *testing.T) {
	for _, mode := range []string{"configure-error", "cancel-before-start", "serving-stop"} {
		t.Run(mode, func(t *testing.T) {
			cluster := testpg.Start(t)
			data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", cluster.Port))
			configValue, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
			if err != nil {
				t.Fatal(err)
			}
			pilot := &nha026ServerPilot{started: make(chan struct{})}
			instance, err := domain.ParseIntegrationInstanceID("nha026-server-durable")
			if err != nil {
				t.Fatal(err)
			}
			spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var service *integrations.DurableCommandService
			configureErr := errors.New("deliberate configure failure")
			factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
				return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}, Configure: func(configureCtx context.Context, host *integrations.Manager, store *storage.Store) error {
					var err error
					service, err = integrations.NewDurableCommandService(configureCtx, host, nha026ServerGeneration{}, store)
					if err != nil {
						return err
					}
					switch mode {
					case "configure-error":
						return configureErr
					case "cancel-before-start":
						cancel()
					}
					return nil
				}}, nil
			}
			ready := make(chan net.Addr, 1)
			done := make(chan error, 1)
			var lc net.ListenConfig
			go func() {
				done <- RunWithFactory(ctx, configValue, lc.Listen, func(addr net.Addr) { ready <- addr }, factory)
			}()
			if mode == "serving-stop" {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("durable runtime did not serve")
				}
				select {
				case <-pilot.started:
				case <-time.After(5 * time.Second):
					t.Fatal("durable runtime pilot did not start")
				}
				cancel()
			}
			select {
			case err := <-done:
				switch mode {
				case "configure-error":
					if !errors.Is(err, configureErr) {
						t.Fatalf("configure error lost: %v", err)
					}
				case "cancel-before-start":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("startup cancellation lost: %v", err)
					}
				case "serving-stop":
					if err != nil {
						t.Fatal(err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run retained prepared durable service")
			}
			if service == nil {
				t.Fatal("Configure did not construct durable service")
			}
			select {
			case <-service.Done():
			default:
				t.Fatal("Run returned before durable guard closed")
			}
			if mode == "serving-stop" {
				if pilot.starts.Load() != 1 || pilot.stopped.Load() != 1 || pilot.joined.Load() != 1 {
					t.Fatalf("serving pilot lifecycle = %d/%d/%d", pilot.starts.Load(), pilot.stopped.Load(), pilot.joined.Load())
				}
			} else if pilot.starts.Load() != 0 || pilot.stopped.Load() != 0 {
				t.Fatalf("failed startup activated pilot: %d/%d", pilot.starts.Load(), pilot.stopped.Load())
			}
			settings, err := configValue.PostgresSettings()
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := storage.Open(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			runtime, err := reopened.AcquireCommandRuntime(context.Background())
			if err != nil {
				t.Fatalf("command runtime guard leaked: %v", err)
			}
			runtime.Close()
		})
	}
}
