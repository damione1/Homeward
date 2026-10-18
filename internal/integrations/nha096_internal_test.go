package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha096BadJitter struct{}

func (nha096BadJitter) Offset(limit time.Duration) time.Duration { return limit + 1 }

func TestNHA096PolicyLimitsScopesAndZeroValues(t *testing.T) {
	if (CloudLimits{}).Valid() {
		t.Fatal("zero limits admitted")
	}
	defaults := DefaultCloudLimits()
	if !defaults.Valid() || defaults.value.MaxInFlight != 4 || defaults.value.MaxQueued != 16 || defaults.value.ObservationAttempts != 3 || defaults.value.ResponseBytes != 1<<20 {
		t.Fatal("default cloud limits drifted")
	}
	bad := defaults.value
	bad.MaxInFlight = 33
	if _, err := AdmitCloudLimits(bad); err == nil {
		t.Fatal("oversized concurrency admitted")
	}
	var nilClock *RealCloudClock
	if _, err := NewCloudPolicy(defaults, nil, nilClock, RealCloudTimer{}, RandomJitter{}); err == nil {
		t.Fatal("typed nil clock admitted")
	}
	first, _ := domain.ParseIntegrationInstanceID("nha096-first")
	second, _ := domain.ParseIntegrationInstanceID("nha096-second")
	third, _ := domain.ParseIntegrationInstanceID("nha096-third")
	account, _ := ParseCloudAccountID("synthetic-shared-account")
	a, _ := NewCloudAccountBinding(first, account)
	b, _ := NewCloudAccountBinding(second, account)
	p, err := NewCloudPolicy(defaults, []CloudAccountBinding{a, b}, RealCloudClock{}, RealCloudTimer{}, RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	_, firstState, _ := p.scope(first)
	_, secondState, _ := p.scope(second)
	_, thirdState, _ := p.scope(third)
	if firstState != secondState || firstState == thirdState {
		t.Fatal("account quota was not shared or instance quota was aliased")
	}
	if _, err := NewCloudPolicy(defaults, []CloudAccountBinding{a, a}, RealCloudClock{}, RealCloudTimer{}, RandomJitter{}); err == nil {
		t.Fatal("duplicate quota binding admitted")
	}
	if _, err := NewCloudRequest(http.MethodGet, "https://example.invalid/private?query=allowed", nil, nil); err != nil {
		t.Fatal("query parameters were incorrectly banned")
	}
	for _, raw := range []string{"https://user:secret@example.invalid/path", "https://example.invalid/path#fragment"} {
		if _, err := NewCloudRequest(http.MethodGet, raw, nil, nil); err == nil {
			t.Fatal("unsafe URL component admitted")
		}
	}
	if got := (&CloudError{}).Kind(); got != CloudFailureUnspecified {
		t.Fatalf("zero error kind = %v", got)
	}
	var nilError *CloudError
	if nilError.Error() == "" || nilError.Kind() != CloudFailureUnspecified {
		t.Fatal("typed nil cloud error is unsafe")
	}
}

func TestNHA096RetryAfterSecondsDatesPastInvalidAndOverflow(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	seconds, valid := parseCloudRetryAfter("7", now)
	if !valid || seconds != now.Add(7*time.Second) {
		t.Fatal("integer retry-after parsed incorrectly")
	}
	date, valid := parseCloudRetryAfter("Sun, 18 Oct 2026 12:01:00 GMT", now)
	if !valid || date != now.Add(time.Minute) {
		t.Fatal("HTTP-date retry-after parsed incorrectly")
	}
	past, valid := parseCloudRetryAfter("Sun, 18 Oct 2026 11:59:00 GMT", now)
	if !valid || past != now {
		t.Fatal("past retry-after was not clamped")
	}
	for _, raw := range []string{"", "-1", "1.5", "tomorrow", "9999999999999999999999999999999999"} {
		if _, valid := parseCloudRetryAfter(raw, now); valid {
			t.Fatalf("invalid retry-after admitted: %q", raw)
		}
	}
}

func TestNHA096GateBoundsQueueAndCancellation(t *testing.T) {
	in := DefaultCloudLimits().value
	in.MaxInFlight = 1
	in.MaxQueued = 1
	in.QueueWait = time.Second
	limits, _ := AdmitCloudLimits(in)
	p, err := NewCloudPolicy(limits, nil, RealCloudClock{}, RealCloudTimer{}, RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha096-gate")
	_, state, err := p.scope(instance)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	release, err := p.acquire(context.Background(), done, state)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() { _, err := p.acquire(ctx, done, state); waiting <- err }()
	deadline := time.After(2 * time.Second)
	for {
		p.mu.Lock()
		queued := state.queued
		p.mu.Unlock()
		if queued == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("bounded waiter never queued")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := p.acquire(context.Background(), done, state); err == nil {
		t.Fatal("third request exceeded queue bound")
	} else {
		var cloudErr *CloudError
		if !errors.As(err, &cloudErr) || cloudErr.Kind() != CloudQueueFull {
			t.Fatalf("queue overflow classification: %v", err)
		}
	}
	cancel()
	select {
	case err := <-waiting:
		if err == nil {
			t.Fatal("canceled waiter entered transport gate")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter did not join")
	}
	release()
	p.mu.Lock()
	inFlight, queued := state.inFlight, state.queued
	p.mu.Unlock()
	if inFlight != 0 || queued != 0 {
		t.Fatalf("gate leaked permits: inflight=%d queued=%d", inFlight, queued)
	}
}

func TestNHA096RejectsOutOfRangeInjectedJitter(t *testing.T) {
	p, err := NewCloudPolicy(DefaultCloudLimits(), nil, RealCloudClock{}, RealCloudTimer{}, nha096BadJitter{})
	if err != nil {
		t.Fatal(err)
	}
	c := &CloudClient{policy: p}
	if _, err := c.backoff(0); err == nil {
		t.Fatal("jitter outside admitted range accepted")
	}
}

func nha096InternalCredentialClient(t *testing.T) (*CloudClient, *storage.SecretRepository, storage.SecretHandle) {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo, err := store.BootstrapSecrets(context.Background(), filepath.Join(t.TempDir(), "private-data"))
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha096-internal-race")
	name, _ := storage.ParseSecretName("race-token")
	handle, _ := storage.NewSecretHandle(instance, name)
	// This unit harness exercises the real scoped secret provider and PG. The
	// external NHA096 tests separately use the constructed Host and router.
	host := &Manager{admission: admissionOpen, active: make(map[domain.IntegrationInstanceID]*Session), secrets: repo}
	session := &Session{host: host, instance: instance, id: "synthetic-session", active: true, done: make(chan struct{})}
	host.active[instance] = session
	policy, err := NewCloudPolicy(DefaultCloudLimits(), nil, RealCloudClock{}, RealCloudTimer{}, RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewCloudClient(session, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, repo, handle
}

func TestNHA096SnapshotDelayedPastLeaderCommitCannotExchangeAgain(t *testing.T) {
	client, repo, handle := nha096InternalCredentialClient(t)
	ctx := context.Background()
	initial, _ := storage.NewSecretValue([]byte("NHA096_old_delayed_canary"))
	if err := repo.RotateCredential(ctx, handle, initial); err != nil {
		t.Fatal(err)
	}
	observed, err := client.ReadCredential(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately hold A's exact scoped SQL snapshot before its policy lock.
	oldValue, oldRevision, err := client.secrets.readVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		_, _ = w.Write([]byte("NHA096_new_delayed_canary"))
	}))
	defer server.Close()
	leader, err := client.BeginRenewal(ctx, observed)
	if err != nil || leader.Kind() != CloudRenewalLeader {
		t.Fatalf("leader B was not admitted: %v %v", leader.Kind(), err)
	}
	lease, _ := leader.Lease()
	defer lease.Close()
	request, _ := NewCloudRequest(http.MethodPost, server.URL, nil, nil)
	response, err := lease.Exchange(request)
	if err != nil {
		t.Fatal(err)
	}
	newValue, _ := storage.NewSecretValue(response.Bytes())
	if err := lease.Commit(newValue); err != nil {
		t.Fatal(err)
	}
	// A now reaches admission with r1, after B durably installed r2. This is
	// the exact post-read/pre-lock interleaving; no scheduler timing is used.
	joined, err := client.beginRenewalAfterRead(ctx, observed, oldValue, oldRevision)
	if err != nil || joined.Kind() != CloudRenewalJoined || effects.Load() != 1 {
		t.Fatalf("stale snapshot launched a second exchange: %v %v effects=%d", joined.Kind(), err, effects.Load())
	}
	value, _ := joined.Value()
	raw, _ := value.Bytes()
	if string(raw) != "NHA096_new_delayed_canary" {
		t.Fatal("delayed caller did not join durable successor")
	}
	// Even if a subsequent generation has already started, an old waiter
	// waking from its own done channel cannot expire or close that generation.
	state := client.policy.refresh[handle]
	oldGeneration := state.generation
	oldDone := make(chan struct{})
	close(oldDone)
	client.policy.refreshMu.Lock()
	state.generation++
	state.active = true
	state.done = make(chan struct{})
	state.expiresAt = time.Now().Add(-time.Second)
	state.waiters = 1
	client.policy.refreshMu.Unlock()
	if _, err := client.waitRenewal(ctx, handle, oldRevision, state, oldDone, time.Now().Add(-time.Second), oldGeneration); err != nil {
		t.Fatal(err)
	}
	client.policy.refreshMu.Lock()
	stillActive := state.active
	newDone := state.done
	client.policy.refreshMu.Unlock()
	if !stillActive {
		t.Fatal("old waiter expired a newer refresh generation")
	}
	select {
	case <-newDone:
		t.Fatal("old waiter closed a newer generation's done channel")
	default:
	}
}
