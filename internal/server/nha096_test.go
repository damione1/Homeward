package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha096ServerPilot struct {
	policy  *integrations.CloudPolicy
	url     string
	started chan error
	stopped chan struct{}
	client  *integrations.CloudClient
}

func (p *nha096ServerPilot) Start(ctx context.Context, host integrations.SessionHost, _ integrations.Tasks) error {
	client, err := integrations.NewCloudClient(host, p.policy, nil)
	if err != nil {
		p.started <- err
		return err
	}
	p.client = client
	request, err := integrations.NewCloudRequest(http.MethodGet, p.url, nil, nil)
	if err != nil {
		p.started <- err
		return err
	}
	response, err := client.Observe(ctx, request)
	if err != nil || response.StatusCode() != http.StatusOK {
		p.started <- errors.New("local cloud observation failed at server composition")
		return errors.New("local cloud observation failed at server composition")
	}
	p.started <- nil
	return nil
}

func (p *nha096ServerPilot) Stop(context.Context) error {
	if p.client != nil {
		p.client.Close()
	}
	close(p.stopped)
	return nil
}

func TestNHA096RunWithFactoryComposesScopedCloudPilotAndStops(t *testing.T) {
	cluster := testpg.Start(t)
	c := nha051ServerConfig(t, cluster.Port, filepath.Join(t.TempDir(), "private-data"))
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"synthetic":true}`))
	}))
	defer endpoint.Close()
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha096-server-pilot")
	if err != nil {
		t.Fatal(err)
	}
	pilot := &nha096ServerPilot{policy: policy, url: endpoint.URL, started: make(chan error, 1), stopped: make(chan struct{})}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}}, nil
	}
	var lc net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, c, lc.Listen, func(address net.Addr) { ready <- address }, factory)
	}()
	select {
	case err := <-pilot.started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scoped cloud pilot did not start")
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not reach ready")
	}
	if requests.Load() != 1 {
		t.Fatalf("server-composed pilot requests=%d", requests.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case <-pilot.stopped:
	case <-time.After(time.Second):
		t.Fatal("cloud pilot Stop was not called")
	}
}
