package integrations_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

type nha096Rig struct {
	host     *integrations.Manager
	fixture  *fake.Fixture
	session  *integrations.Session
	client   *integrations.CloudClient
	policy   *integrations.CloudPolicy
	repo     *storage.SecretRepository
	store    *storage.Store
	instance domain.IntegrationInstanceID
}

func nha096CloudRig(t *testing.T, policy *integrations.CloudPolicy) nha096Rig {
	t.Helper()
	store, _ := nha024Store(t)
	repo, err := store.BootstrapSecrets(context.Background(), filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(repo); err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha096-cloud")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		policy, err = integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
		if err != nil {
			t.Fatal(err)
		}
	}
	client, err := integrations.NewCloudClient(session, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return nha096Rig{host: host, fixture: fixture, session: session, client: client, policy: policy, repo: repo, store: store, instance: instance}
}

func nha096Kind(t *testing.T, err error, kind integrations.CloudFailureKind) *integrations.CloudError {
	t.Helper()
	var cloudErr *integrations.CloudError
	if !errors.As(err, &cloudErr) || cloudErr.Kind() != kind {
		t.Fatalf("cloud error kind: got %v, want %v", err, kind)
	}
	return cloudErr
}

func nha096Wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("NHA-096 barrier timed out")
		var zero T
		return zero
	}
}

type nha096Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nha096Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *nha096Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type nha096TimerRequest struct {
	delay time.Duration
	fire  chan time.Time
}
type nha096Timer struct{ waits chan nha096TimerRequest }
type nha096Waiter struct{ fire chan time.Time }

func (t *nha096Timer) NewTimer(delay time.Duration) integrations.CloudWait {
	request := nha096TimerRequest{delay: delay, fire: make(chan time.Time, 1)}
	t.waits <- request
	return nha096Waiter{fire: request.fire}
}
func (w nha096Waiter) C() <-chan time.Time { return w.fire }
func (w nha096Waiter) Stop() bool          { return true }

type nha096NoJitter struct{}

func (nha096NoJitter) Offset(time.Duration) time.Duration { return 0 }

func TestNHA096RetryAfterNoEarlyAttemptAndHealthRecovery(t *testing.T) {
	clock := &nha096Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	timer := &nha096Timer{waits: make(chan nha096TimerRequest, 8)}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, clock, timer, nha096NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	rig := nha096CloudRig(t, policy)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"relay":false}`))
	}))
	defer server.Close()
	request, err := integrations.NewCloudRequest(http.MethodGet, server.URL+"/read?legitimate=1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		response integrations.CloudResponse
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		response, err := rig.client.Observe(context.Background(), request)
		completed <- result{response, err}
	}()
	wait := nha096Wait(t, timer.waits)
	if wait.delay < 2*time.Second || count.Load() != 1 {
		t.Fatalf("429 minimum was bypassed: wait=%v requests=%d", wait.delay, count.Load())
	}
	snapshot, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if health, ok := snapshot.Health[rig.instance]; !ok {
		t.Fatal("429 did not publish degraded health")
	} else if status, _, err := health.Components(); err != nil || status != integrations.HealthDegraded {
		t.Fatal("429 health was not degraded")
	}
	clock.Advance(2 * time.Second)
	wait.fire <- clock.Now()
	got := nha096Wait(t, completed)
	if got.err != nil || got.response.StatusCode() != http.StatusOK || count.Load() != 2 {
		t.Fatalf("bounded observation retry: status=%d err=%v requests=%d", got.response.StatusCode(), got.err, count.Load())
	}
	snapshot, err = rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := snapshot.Health[rig.instance].Components(); status != integrations.HealthDegraded {
		t.Fatal("HTTP 200 alone erased degraded health")
	}
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	ids, err := rig.fixture.Resolve(context.Background(), rig.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], rig.instance, key, "switch.nha096", "switch.nha096", false)
	if err := rig.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	fakeDriver, _ := fake.NewActionDriver(time.Now())
	if err := fakeDriver.Start(context.Background(), rig.session); err != nil {
		t.Fatal(err)
	}
	binding, err := fakeDriver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(context.Background(), binding, nha016Observation(t, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := rig.client.ConfirmObservationAdmitted(context.Background(), got.response); err != nil {
		t.Fatal(err)
	}
	snapshot, err = rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := snapshot.Health[rig.instance].Components(); status != integrations.HealthReady {
		t.Fatal("admitted observation did not recover cloud health")
	}
}

func TestNHA096RetryAfterInvalidAndTooLargeDoNotRetry(t *testing.T) {
	for _, header := range []string{"nonsense", "999999999999999999999999999999"} {
		t.Run(header, func(t *testing.T) {
			rig := nha096CloudRig(t, nil)
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Retry-After", header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			request, _ := integrations.NewCloudRequest(http.MethodGet, server.URL, nil, nil)
			_, err := rig.client.Observe(context.Background(), request)
			nha096Kind(t, err, integrations.CloudQuotaDeferred)
			if count.Load() != 1 {
				t.Fatalf("invalid/overflow retry-after caused %d requests", count.Load())
			}
		})
	}
}

func TestNHA096HTTPDateBeyondBudgetNeverRetries(t *testing.T) {
	clock := &nha096Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, clock, integrations.RealCloudTimer{}, nha096NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	rig := nha096CloudRig(t, policy)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Retry-After", clock.Now().Add(time.Minute).Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodGet, server.URL, nil, nil)
	_, err = rig.client.Observe(context.Background(), request)
	failure := nha096Kind(t, err, integrations.CloudQuotaDeferred)
	if retryAt, ok := failure.RetryAt(); !ok || retryAt.Before(clock.Now().Add(time.Minute)) || count.Load() != 1 {
		t.Fatalf("HTTP-date beyond budget retried: at=%v valid=%v count=%d", retryAt, ok, count.Load())
	}
}

func TestNHA096RetryAfterHeadersFenceEvenWhenBodyBlocksOrOverflows(t *testing.T) {
	for _, body := range []string{"blocked", "oversized"} {
		t.Run(body, func(t *testing.T) {
			rig := nha096CloudRig(t, nil)
			var count atomic.Int32
			handlerDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				w.(http.Flusher).Flush()
				if body == "blocked" {
					<-r.Context().Done()
				} else {
					_, _ = w.Write(make([]byte, 2<<20))
				}
				close(handlerDone)
			}))
			defer server.Close()
			request, _ := integrations.NewCloudRequest(http.MethodGet, server.URL, nil, nil)
			_, err := rig.client.Observe(context.Background(), request)
			failure := nha096Kind(t, err, integrations.CloudQuotaDeferred)
			if retryAt, ok := failure.RetryAt(); !ok || time.Until(retryAt) < 30*time.Second || count.Load() != 1 {
				t.Fatalf("429 body bypassed header quota: retry=%v valid=%v requests=%d", retryAt, ok, count.Load())
			}
			nha096Wait(t, handlerDone)
		})
	}
}

func TestNHA096OlderSuccessCannotClearNewerCloudFailure(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer server.Close()
	failing, _ := integrations.NewCloudRequest(http.MethodGet, server.URL+"/fail", nil, nil)
	succeeding, _ := integrations.NewCloudRequest(http.MethodGet, server.URL+"/success", nil, nil)
	if _, err := rig.client.Observe(context.Background(), failing); err == nil {
		t.Fatal("credential rejection did not degrade cloud")
	}
	response, err := rig.client.Observe(context.Background(), succeeding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client.Observe(context.Background(), failing); err == nil {
		t.Fatal("second credential rejection was accepted")
	}
	nha096Kind(t, rig.client.ConfirmObservationAdmitted(context.Background(), response), integrations.CloudInvalid)
	snapshot, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := snapshot.Health[rig.instance].Components(); err != nil || status != integrations.HealthDegraded {
		t.Fatal("older successful HTTP response cleared a newer cloud failure")
	}
}

func TestNHA096RealHTTPGateAndCancellation(t *testing.T) {
	in := integrations.DefaultCloudLimits().Input()
	in.MaxInFlight = 1
	in.MaxQueued = 1
	in.ObservationAttempts = 1
	in.QueueWait = time.Second
	limits, err := integrations.AdmitCloudLimits(in)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(limits, nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, nha096NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	rig := nha096CloudRig(t, policy)
	var entered atomic.Int32
	firstEntered := make(chan struct{})
	firstDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if entered.Add(1) == 1 {
			close(firstEntered)
			<-r.Context().Done()
			close(firstDone)
			return
		}
		_, _ = w.Write([]byte(`{"observed":true}`))
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodGet, server.URL, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := rig.client.Observe(ctx, request); first <- err }()
	nha096Wait(t, firstEntered)
	second := make(chan error, 1)
	go func() { _, err := rig.client.Observe(context.Background(), request); second <- err }()
	// While the first HTTP handler is blocked, another session can still use
	// Host; releasing its transport permit requires cancellation to join I/O.
	other, err := rig.host.OpenSession(nha016Instance(t, "nha096-neighbor"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Secrets(); err != nil {
		t.Fatal(err)
	}
	if entered.Load() != 1 {
		t.Fatal("shared account concurrency limit was exceeded")
	}
	cancel()
	nha096Wait(t, firstDone)
	nha096Kind(t, nha096Wait(t, first), integrations.CloudTransportFailed)
	if err := nha096Wait(t, second); err != nil || entered.Load() != 2 {
		t.Fatalf("queued HTTP request did not progress after cancellation: %v count=%d", err, entered.Load())
	}
}

func TestNHA096ResponseBoundAndBodyReadDeadline(t *testing.T) {
	in := integrations.DefaultCloudLimits().Input()
	in.ObservationAttempts = 1
	in.ResponseBytes = 8
	in.AttemptTimeout = 40 * time.Millisecond
	in.TotalTimeout = 100 * time.Millisecond
	limits, err := integrations.AdmitCloudLimits(in)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(limits, nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, nha096NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	rig := nha096CloudRig(t, policy)
	bodyCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			_, _ = w.Write([]byte("123456789"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(bodyCanceled)
	}))
	defer server.Close()
	large, _ := integrations.NewCloudRequest(http.MethodGet, server.URL+"/large", nil, nil)
	_, err = rig.client.Observe(context.Background(), large)
	nha096Kind(t, err, integrations.CloudResponseTooLarge)
	blocked, _ := integrations.NewCloudRequest(http.MethodGet, server.URL+"/blocked", nil, nil)
	_, err = rig.client.Observe(context.Background(), blocked)
	nha096Kind(t, err, integrations.CloudTransportFailed)
	nha096Wait(t, bodyCanceled)
}

type nha096CloudDriver struct {
	client  *integrations.CloudClient
	request integrations.CloudRequest
	entries atomic.Int32
}

func (d *nha096CloudDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	d.entries.Add(1)
	_, err := d.client.Act(ctx, command, d.request)
	return err
}

func TestNHA096AdmittedCommandPostEffectTimeoutExactlyOnce(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	var effects, reads atomic.Int32
	actionEntered := make(chan struct{})
	actionDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/action":
			_, _ = io.Copy(io.Discard, r.Body)
			effects.Add(1)
			close(actionEntered)
			<-r.Context().Done()
			close(actionDone)
		case "/read":
			if reads.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				_, _ = w.Write([]byte(`{"relay":true}`))
			}
		}
	}))
	defer server.Close()
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	ids, err := rig.fixture.Resolve(context.Background(), rig.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], rig.instance, key, "switch.nha096", "switch.nha096", false)
	if err := rig.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	fakeDriver, _ := fake.NewActionDriver(time.Now())
	if err := fakeDriver.Start(context.Background(), rig.session); err != nil {
		t.Fatal(err)
	}
	actionRequest, _ := integrations.NewCloudRequest(http.MethodPost, server.URL+"/action", nil, []byte(`{"on":true}`))
	driver := &nha096CloudDriver{client: rig.client, request: actionRequest}
	if err := rig.host.AttachCommandDriver(rig.session, driver); err != nil {
		t.Fatal(err)
	}
	cursor, err := rig.fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := cursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	service, err := integrations.NewVolatileCommandService(rig.host, &nha022Generation{revision: 7}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	completed := make(chan result, 1)
	actor := nha022Actor(t)
	go func() { response, err := service.Dispatch(ctx, actor, request); completed <- result{response, err} }()
	nha096Wait(t, actionEntered)
	cancel()
	first := nha096Wait(t, completed)
	if !errors.Is(first.err, integrations.ErrCommandOutcomeIndeterminate) || first.response == nil || first.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("post-effect command was not indeterminate: %v %v", first.response, first.err)
	}
	nha096Wait(t, actionDone)
	replayed, replayErr := service.Dispatch(context.Background(), actor, request)
	if !errors.Is(replayErr, integrations.ErrCommandOutcomeIndeterminate) || replayed == nil || replayed.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || effects.Load() != 1 || driver.entries.Load() != 1 {
		t.Fatalf("duplicate effect: result=%v err=%v effects=%d driver=%d", replayed, replayErr, effects.Load(), driver.entries.Load())
	}
	observationRequest, _ := integrations.NewCloudRequest(http.MethodGet, server.URL+"/read", nil, nil)
	response, err := rig.client.Observe(context.Background(), observationRequest)
	if err != nil || response.StatusCode() != http.StatusOK || reads.Load() != 2 || effects.Load() != 1 {
		t.Fatalf("observation retry changed action count: response=%v err=%v reads=%d effects=%d", response, err, reads.Load(), effects.Load())
	}
}

func TestNHA096RenewalSingleFlightCASAndCooldown(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	handle := nha096Handle(t, rig.instance, "oauth-token")
	const oldCanary = "NHA096_old_refresh_canary"
	const newCanary = "NHA096_new_refresh_canary"
	oldValue, _ := storage.NewSecretValue([]byte(oldCanary))
	if err := rig.repo.RotateCredential(context.Background(), handle, oldValue); err != nil {
		t.Fatal(err)
	}
	observed, err := rig.client.ReadCredential(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		close(entered)
		<-release
		_, _ = w.Write([]byte(newCanary))
	}))
	defer server.Close()
	first, err := rig.client.BeginRenewal(context.Background(), observed)
	if err != nil || first.Kind() != integrations.CloudRenewalLeader {
		t.Fatalf("refresh leader: %v %v", first.Kind(), err)
	}
	lease, _ := first.Lease()
	defer lease.Close()
	request, _ := integrations.NewCloudRequest(http.MethodPost, server.URL+"/refresh", nil, []byte(oldCanary))
	type exchangeResult struct {
		response integrations.CloudResponse
		err      error
	}
	finished := make(chan exchangeResult, 1)
	go func() { response, err := lease.Exchange(request); finished <- exchangeResult{response, err} }()
	nha096Wait(t, entered)
	joined := make(chan struct {
		result integrations.CloudRenewal
		err    error
	}, 1)
	go func() {
		result, err := rig.client.BeginRenewal(context.Background(), observed)
		joined <- struct {
			result integrations.CloudRenewal
			err    error
		}{result, err}
	}()
	close(release)
	answer := nha096Wait(t, finished)
	if answer.err != nil || string(answer.response.Bytes()) != newCanary {
		t.Fatalf("single renewal exchange: %v %v", answer.response, answer.err)
	}
	newValue, _ := storage.NewSecretValue(answer.response.Bytes())
	if err := lease.Commit(newValue); err != nil {
		t.Fatal(err)
	}
	waited := nha096Wait(t, joined)
	if waited.err != nil || waited.result.Kind() != integrations.CloudRenewalJoined {
		t.Fatalf("joined refresh: %v %v", waited.result.Kind(), waited.err)
	}
	joinedValue, err := waited.result.Value()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := joinedValue.Bytes()
	if string(raw) != newCanary || exchanges.Load() != 1 {
		t.Fatalf("refresh duplicated: exchanges=%d", exchanges.Load())
	}
	// A late caller carrying the old observation also joins without another HTTP effect.
	late, err := rig.client.BeginRenewal(context.Background(), observed)
	if err != nil || late.Kind() != integrations.CloudRenewalJoined || exchanges.Load() != 1 {
		t.Fatalf("late stale refresh retried: %v %v exchanges=%d", late.Kind(), err, exchanges.Load())
	}
	newObserved, err := rig.client.ReadCredential(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rig.client.BeginRenewal(context.Background(), newObserved)
	if err != nil || second.Kind() != integrations.CloudRenewalLeader {
		t.Fatalf("new revision did not permit a new lease: %v %v", second.Kind(), err)
	}
	secondLease, _ := second.Lease()
	secondLease.Close() // definite pretransport failure starts cooldown
	_, err = rig.client.BeginRenewal(context.Background(), newObserved)
	if failure := nha096Kind(t, err, integrations.CloudCredentialFailed); failure == nil {
		t.Fatal("cooldown did not classify failure")
	} else if _, ok := failure.RetryAt(); !ok {
		t.Fatal("cooldown did not expose a retry time")
	}
}

func nha096Handle(t *testing.T, instance domain.IntegrationInstanceID, name string) storage.SecretHandle {
	t.Helper()
	secretName, err := storage.ParseSecretName(name)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := storage.NewSecretHandle(instance, secretName)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func TestNHA096LateRefreshResponseAfterRevocationCannotRotate(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	handle := nha096Handle(t, rig.instance, "late-token")
	const canary = "NHA096_original_late_token"
	value, _ := storage.NewSecretValue([]byte(canary))
	if err := rig.repo.RotateCredential(context.Background(), handle, value); err != nil {
		t.Fatal(err)
	}
	observed, err := rig.client.ReadCredential(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	foreignInstance := nha016Instance(t, "nha096-foreign")
	foreign := nha096Handle(t, foreignInstance, "late-token")
	if _, err := rig.client.ReadCredential(context.Background(), foreign); err == nil {
		t.Fatal("active client read foreign handle")
	}
	begin, err := rig.client.BeginRenewal(context.Background(), observed)
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := begin.Lease()
	defer lease.Close()
	entered := make(chan struct{})
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(handlerDone)
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodPost, server.URL, nil, []byte(canary))
	finished := make(chan error, 1)
	go func() { _, err := lease.Exchange(request); finished <- err }()
	nha096Wait(t, entered)
	if err := rig.host.Revoke(rig.session); err != nil {
		t.Fatal(err)
	}
	nha096Wait(t, handlerDone)
	if err := nha096Wait(t, finished); err == nil {
		t.Fatal("late remote response renewed retired session")
	}
	stored, err := rig.repo.Read(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != canary {
		t.Fatal("retired session changed durable credential")
	}
	_, err = rig.client.ReadCredential(context.Background(), foreign)
	if err == nil {
		t.Fatal("retired client read foreign handle")
	}
	for _, thing := range []any{request, observed, begin, err} {
		if strings.Contains(fmt.Sprintf("%+v %#v", thing, thing), canary) {
			t.Fatal("credential leaked through diagnostic")
		}
	}
}

func TestNHA096ManualRotationWinsLateRefreshCAS(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	handle := nha096Handle(t, rig.instance, "manual-race-token")
	old, _ := storage.NewSecretValue([]byte("NHA096_old_manual_race"))
	if err := rig.repo.RotateCredential(context.Background(), handle, old); err != nil {
		t.Fatal(err)
	}
	observed, err := rig.client.ReadCredential(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := rig.client.BeginRenewal(context.Background(), observed)
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := begin.Lease()
	defer lease.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("NHA096_remote_stale_response"))
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodPost, server.URL, nil, nil)
	response, err := lease.Exchange(request)
	if err != nil {
		t.Fatal(err)
	}
	manual, _ := storage.NewSecretValue([]byte("NHA096_new_manual_winner"))
	if err := rig.repo.RotateCredential(context.Background(), handle, manual); err != nil {
		t.Fatal(err)
	}
	stale, _ := storage.NewSecretValue(response.Bytes())
	if err := lease.Commit(stale); err == nil {
		t.Fatal("late refresh overwrote manual rotation")
	} else {
		nha096Kind(t, err, integrations.CloudCredentialConflict)
	}
	stored, err := rig.repo.Read(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != "NHA096_new_manual_winner" {
		t.Fatal("manual rotation was lost")
	}
	joined, err := rig.client.BeginRenewal(context.Background(), observed)
	if err != nil || joined.Kind() != integrations.CloudRenewalJoined {
		t.Fatalf("stale reader did not join newer manual credential: %v %v", joined.Kind(), err)
	}
}

func TestNHA096RetirementDuringAdmittedSQLHasNoVisibleSuccess(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	ctx := context.Background()
	handle := nha096Handle(t, rig.instance, "retired-during-sql")
	old, _ := storage.NewSecretValue([]byte("NHA096_old_SQL_barrier_canary"))
	if err := rig.repo.RotateCredential(ctx, handle, old); err != nil {
		t.Fatal(err)
	}
	observed, err := rig.client.ReadCredential(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := rig.client.BeginRenewal(ctx, observed)
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := renewal.Lease()
	defer lease.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("NHA096_new_SQL_barrier_canary"))
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodPost, server.URL, nil, nil)
	response, err := lease.Exchange(request)
	if err != nil {
		t.Fatal(err)
	}
	newValue, _ := storage.NewSecretValue(response.Bytes())
	// This deferred trigger stops CAS at its real PostgreSQL commit boundary.
	// The production predicate/commit remains the only writer authority.
	if err := rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION public.nha096_secret_commit_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(481316, 960); RETURN NEW; END $$;
			CREATE CONSTRAINT TRIGGER nha096_secret_commit_barrier AFTER UPDATE ON public.integration_secrets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha096_secret_commit_barrier()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	lockerDone := make(chan error, 1)
	go func() {
		lockerDone <- rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 960)`); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	nha096Wait(t, held)
	committed := make(chan error, 1)
	go func() { committed <- lease.Commit(newValue) }()
	waiting := false
	deadline := time.After(4 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !waiting {
		select {
		case <-tick.C:
			if err := rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
				var count int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND pid<>pg_backend_pid()`).Scan(&count); err != nil {
					return err
				}
				waiting = count > 0
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		case err := <-committed:
			t.Fatalf("CAS completed before the commit barrier: %v", err)
		case <-deadline:
			t.Fatal("CAS never reached PostgreSQL commit barrier")
		}
	}
	if err := rig.host.Revoke(rig.session); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := nha096Wait(t, lockerDone); err != nil {
		t.Fatal(err)
	}
	commitErr := nha096Wait(t, committed)
	if commitErr == nil {
		t.Fatal("retired session observed credential commit as success")
	}
	var cloudErr *integrations.CloudError
	if !errors.As(commitErr, &cloudErr) || (cloudErr.Kind() != integrations.CloudCredentialCommitUnknown && cloudErr.Kind() != integrations.CloudCredentialCommittedRetired) {
		t.Fatalf("retired SQL outcome lost its ambiguity: %v", commitErr)
	}
	stored, err := rig.repo.Read(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != "NHA096_old_SQL_barrier_canary" && string(raw) != "NHA096_new_SQL_barrier_canary" {
		t.Fatal("unexpected durable credential after ambiguous commit")
	}
}

func TestNHA096AmbiguousRemoteRefreshIsNeverReplayed(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	handle := nha096Handle(t, rig.instance, "ambiguous-token")
	initial, _ := storage.NewSecretValue([]byte("NHA096_ambiguous_old"))
	if err := rig.repo.RotateCredential(context.Background(), handle, initial); err != nil {
		t.Fatal(err)
	}
	observed, err := rig.client.ReadCredential(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close() // effect happened, reply lost
		}
	}))
	defer server.Close()
	begin, err := rig.client.BeginRenewal(context.Background(), observed)
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := begin.Lease()
	defer lease.Close()
	request, _ := integrations.NewCloudRequest(http.MethodPost, server.URL, nil, []byte("synthetic"))
	if _, err := lease.Exchange(request); err == nil {
		t.Fatal("lost refresh response was considered successful")
	}
	_, err = rig.client.BeginRenewal(context.Background(), observed)
	nha096Kind(t, err, integrations.CloudCredentialFailed)
	if effects.Load() != 1 {
		t.Fatalf("ambiguous refresh effect count = %d", effects.Load())
	}
	stored, err := rig.repo.Read(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != "NHA096_ambiguous_old" {
		t.Fatal("ambiguous remote effect was falsely committed locally")
	}
}
