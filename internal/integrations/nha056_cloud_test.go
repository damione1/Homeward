package integrations_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func TestNHA056ActualCloudQueueAndFailurePreserveLocalConfirmedControl(t *testing.T) {
	driver := &nha025Driver{}
	local := nha025Setup(t, driver, nil)
	store, _ := nha024Store(t)
	repo, err := store.BootstrapSecrets(context.Background(), filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.host.AttachSecrets(repo); err != nil {
		t.Fatal(err)
	}
	remote, err := local.host.OpenSession(nha016Instance(t, "nha056-remote-cloud"))
	if err != nil {
		t.Fatal(err)
	}
	in := integrations.DefaultCloudLimits().Input()
	in.MaxInFlight = 1
	in.MaxQueued = 1
	in.ObservationAttempts = 1
	limits, err := integrations.AdmitCloudLimits(in)
	if err != nil {
		t.Fatal(err)
	}
	timer := &nha096Timer{waits: make(chan nha096TimerRequest, 8)}
	policy, err := integrations.NewCloudPolicy(limits, nil, integrations.RealCloudClock{}, timer, nha096NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	cells, err := local.host.ObservationCells()
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.AttachObservation(cells); err != nil {
		t.Fatal(err)
	}
	client, err := integrations.NewCloudClient(remote, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("NHA056_CLOUD_PAYLOAD_TOKEN_CANARY"))
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	request, err := integrations.NewCloudRequest(http.MethodGet, upstream.URL+"/private?token=NHA056_SIGNED_URI_CANARY", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { _, err := client.Observe(context.Background(), request); first <- err }()
	nha096Wait(t, entered)
	waitingCtx, cancelWaiting := context.WithCancel(context.Background())
	defer cancelWaiting()
	waiting := make(chan error, 1)
	go func() { _, err := client.Observe(waitingCtx, request); waiting <- err }()
	// The actual CloudPolicy invokes its timer after incrementing the real
	// waiting population, so this is an owner transition witness, without sleep.
	nha096Wait(t, timer.waits)
	metrics, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS)
	if err != nil || metrics.GetInstalled() == nil {
		t.Fatal("actual cloud queue not installed", err)
	}
	queue := metrics.GetInstalled()
	if queue.ActiveQueues.GetValue() != 1 || queue.QueuedEntries.GetValue() != 1 || queue.CapacityEntries.GetValue() != 1 || queue.InFlightEntries.GetValue() != 1 || queue.PendingEntries.GetNotMeasured() == nil {
		t.Fatal("cloud waiting and in-flight populations were conflated")
	}
	_, err = client.Observe(context.Background(), request)
	nha096Kind(t, err, integrations.CloudQueueFull)
	cancelWaiting()
	if err := nha096Wait(t, waiting); err == nil {
		t.Fatal("cancelled waiting request entered transport")
	}
	close(release)
	failure := nha096Wait(t, first)
	nha096Kind(t, failure, integrations.CloudHTTPFailed)
	if strings.Contains(failure.Error(), "NHA056_") {
		t.Fatal("cloud diagnostic exposed URI or response body")
	}
	observed, err := local.fixture.Sequencer().IntegrationHealth(nha016Instance(t, "nha056-remote-cloud"))
	if err != nil || observed.GetPresent().GetStatus() != wire.HealthStatus_HEALTH_STATUS_DEGRADED {
		t.Fatal("real cloud failure did not degrade its integration", err)
	}
	metrics, err = cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS)
	if err != nil || metrics.GetInstalled().QueuedEntries.GetValue() != 0 || metrics.GetInstalled().InFlightEntries.GetValue() != 0 || metrics.GetInstalled().RejectedTotal.GetExact() != 1 {
		t.Fatal("actual cloud queue did not release or preserve its rejection total", err)
	}
	// Counter saturation is observational and must not change the following
	// real local command, its one physical fake effect, or its confirmation.
	if err := cells.AddQueueRejected(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	saturated, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS)
	if err != nil || saturated.GetInstalled().RejectedTotal.GetSaturated().GetValue() != math.MaxUint64 {
		t.Fatal("counter did not saturate", err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		cause, err := command.Causality()
		if err != nil {
			return err
		}
		return nha025Publish(t, local, true, &cause, quality)
	}
	response, err := local.service.Dispatch(context.Background(), nha022Actor(t), local.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.Count() != 1 {
		t.Fatal("cloud failure blocked or replayed local control", err)
	}
	nha025Status(t, local.host, nha025ID(t, local.request), actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	for range 3 {
		if _, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS); err != nil {
			t.Fatal(err)
		}
	}
	if driver.Count() != 1 {
		t.Fatal("observing cloud state replayed a local effect")
	}
}
