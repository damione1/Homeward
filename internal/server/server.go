// Package server composes the cancellable bootstrap runtime. Driver activation
// from configuration and the public API belong to later tickets.
package server

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

type ListenFunc func(context.Context, string, string) (net.Listener, error)

var ErrInvalidContext = errors.New("server: missing context")
var ErrIdentityNotConfigured = errors.New("server: integration identity authority is not configured")

type unconfiguredIdentity struct{}

func (unconfiguredIdentity) Resolve(context.Context, domain.IntegrationInstanceID, []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	return nil, ErrIdentityNotConfigured
}

// RuntimePlan declares the runtime before serving. Configure must complete
// synchronously, honor ctx, and register any durable service on host. The
// server owns those services until Supervisor.Start succeeds; then the
// supervisor drains them before PostgreSQL closes.
type RuntimePlan struct {
	Identity  integrations.IdentityAuthority
	Pilots    []integrations.PilotSpec
	Configure func(context.Context, *integrations.Manager, *storage.Store) error
}

// RuntimeFactory is trusted server composition. The default runtime has no
// configured identity source, pilots, or services. Test assemblies use this
// same Run path after the same PostgreSQL migration.
type RuntimeFactory func(context.Context, *storage.Store) (RuntimePlan, error)

func Run(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	return RunWithFactory(ctx, c, listen, ready, nil)
}

func RunWithFactory(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr), factory RuntimeFactory) error {
	if ctx == nil || listen == nil {
		return ErrInvalidContext
	}
	settings, err := c.PostgresSettings()
	if err != nil {
		return err
	}
	if _, err := c.ListenEndpoint(); err != nil {
		return err
	}
	dataDir, err := c.DataDir()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return err
	}
	if _, err := store.Migrate(ctx); err != nil {
		store.Close()
		return err
	}
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		store.Close()
		return err
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		store.Close()
		return err
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		store.Close()
		return err
	}
	plan := RuntimePlan{Identity: unconfiguredIdentity{}}
	if factory != nil {
		plan, err = factory(ctx, store)
		if err != nil {
			store.Close()
			return err
		}
	} else if mqttSettings, enabled := c.MQTTSettings(); enabled {
		instance, err := mqttSettings.Instance()
		if err != nil {
			store.Close()
			return err
		}
		pilotFactory, err := mqtt.NewPilotFactory(mqttSettings, mqtt.UnmappedObservations{})
		if err != nil {
			store.Close()
			return err
		}
		spec, err := integrations.NewPilotSpec(instance, pilotFactory)
		if err != nil {
			store.Close()
			return err
		}
		plan.Pilots = []integrations.PilotSpec{spec}
	}
	if err := ctx.Err(); err != nil {
		store.Close()
		return err
	}
	host, err := integrations.NewHost(plan.Identity, sequence, catalog)
	if err != nil {
		store.Close()
		return err
	}
	if err := host.AttachSecrets(secrets); err != nil {
		store.Close()
		return err
	}
	if plan.Configure != nil {
		if err := plan.Configure(ctx, host, store); err != nil {
			return closePreparedRuntime(host, store, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return closePreparedRuntime(host, store, err)
	}
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		return closePreparedRuntime(host, store, err)
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := supervisor.Start(ctx); err != nil {
		lifecycleCancel()
		return closePreparedRuntime(host, store, err)
	}
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = host.CloseAdmissions()
			lifecycleCancel()
		case <-watchStop:
		}
	}()
	listenerErr := runListener(lifecycleCtx, c, listen, ready)
	_ = host.CloseAdmissions()
	close(watchStop)
	<-watchDone
	lifecycleCancel()
	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	stopErr := supervisor.Stop(stopCtx)
	cancel()
	if stopErr != nil {
		// Retain PostgreSQL until the last cooperative or delayed pilot exits.
		go func() { <-supervisor.Done(); store.Close() }()
		return errors.Join(listenerErr, stopErr)
	}
	store.Close()
	return listenerErr
}

func closePreparedRuntime(host *integrations.Manager, store *storage.Store, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	done, closeErr := host.ClosePreparedRuntime(ctx)
	cancel()
	if closeErr != nil && done != nil {
		go func() { <-done; store.Close() }()
		return errors.Join(cause, closeErr)
	}
	store.Close()
	return errors.Join(cause, closeErr)
}

func runListener(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	endpoint, err := c.ListenEndpoint()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l, err := listen(ctx, "tcp", endpoint.String())
	if err != nil {
		return err
	}
	defer l.Close()
	// Close interrupts Accept; defer joins this goroutine even on unexpected listener failure.
	stop := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-stop:
		}
	}()
	defer func() { close(stop); <-joined }()
	if ready != nil {
		ready(l.Addr())
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		_ = conn.Close()
	}
}
