package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// Pause the first time sample while logical time advances. SQL receives the
// saved pre-expiry time, and At deliberately retains callbacks like a timer
// whose goroutine has not run yet. Expiry rejection must therefore be synchronous.
type nha145DelayedExpiryClock struct {
	mu       sync.Mutex
	now      time.Time
	first    sync.Once
	sampled  chan time.Time
	released chan struct{}
	resume   func()
	timers   []*nha145DelayedExpiryTimer
}

type nha145DelayedExpiryTimer struct {
	deadline time.Time
	fire     func()
	stopped  atomic.Bool
}

func (timer *nha145DelayedExpiryTimer) Stop() bool {
	return timer.stopped.CompareAndSwap(false, true)
}

func newNHA145DelayedExpiryClock(now time.Time) *nha145DelayedExpiryClock {
	released := make(chan struct{})
	return &nha145DelayedExpiryClock{
		now: now, sampled: make(chan time.Time, 1), released: released,
		resume: sync.OnceFunc(func() { close(released) }),
	}
}

func (c *nha145DelayedExpiryClock) Now() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	c.first.Do(func() {
		c.sampled <- now
		<-c.released
	})
	return now
}

func (c *nha145DelayedExpiryClock) At(deadline time.Time, fire func()) sessionTimer {
	timer := &nha145DelayedExpiryTimer{deadline: deadline, fire: fire}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	return timer
}

func TestNHA145MCPAdmissionRechecksExpiryBeforeLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	pg, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := newAccountInput(ctx, "Expiry Admin", "Admin", "NHA145 isolated expiry password", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	id, err := mcpkeys.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := mcpkeys.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := secret.Digest()
	if err != nil {
		t.Fatal(err)
	}
	client, err := mcpkeys.ParseClientName("admission expiry fixture")
	if err != nil {
		t.Fatal(err)
	}
	// Hydrate the same native authorization projection used by MCP admission.
	if _, err := prepareLocalAuthorization(ctx, store, realSessionClock{}); err != nil {
		t.Fatal("prepare authorization", err)
	}
	record, err := store.CreateMCPKey(ctx, storage.MCPKeyCreation{
		ID: id, Account: admin.ID, Client: client, Digest: digest,
		Scopes: []authorization.Permission{}, Lifetime: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := secret.Text()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		advance time.Time
		valid   bool
	}{
		{"still valid", record.ExpiresAt.Add(-time.Microsecond), true},
		{"exact deadline", record.ExpiresAt, false},
		{"past deadline", record.ExpiresAt.Add(time.Microsecond), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newNHA145DelayedExpiryClock(now)
			defer clock.resume()
			coordinator, err := prepareLocalAuthorization(ctx, store, clock)
			if err != nil {
				t.Fatal(err)
			}
			provider := &sessionAuth{store: store, coordinator: coordinator}
			request := httptest.NewRequest(http.MethodGet, "https://fixture.invalid/nha145/expiry", nil)
			request.TLS = &tls.ConnectionState{}
			request.Header.Set("Authorization", "Bearer "+text)
			admissionCtx := context.WithValue(ctx, mcpRouteContextKey{}, RouteKey{kind: RouteKindMCP, method: RouteMethodGet, path: "/nha145/expiry"})
			type result struct {
				ctx     context.Context
				release func()
				err     error
			}
			done := make(chan result, 1)
			go func() {
				_, leaseCtx, release, err := provider.AuthenticateWithLease(admissionCtx, request)
				done <- result{leaseCtx, release, err}
			}()
			select {
			case sampled := <-clock.sampled:
				if sampled != now || !sampled.Before(record.ExpiresAt) {
					t.Fatal("SQL admission did not capture a valid credential time")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			clock.mu.Lock()
			clock.now = test.advance
			clock.mu.Unlock()
			clock.resume()
			var admitted result
			select {
			case admitted = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if admitted.release != nil {
				defer admitted.release()
			}
			clock.mu.Lock()
			timers := len(clock.timers)
			clock.mu.Unlock()
			if test.valid {
				if admitted.err != nil || admitted.ctx == nil || admitted.ctx.Err() != nil || admitted.release == nil || timers != 1 {
					t.Fatalf("valid credential refused: err=%v timers=%d", admitted.err, timers)
				}
				return
			}
			if !errors.Is(admitted.err, errSessionUnauthenticated) || admitted.ctx != nil || admitted.release != nil || timers != 0 {
				t.Fatalf("expired credential admitted before delayed timer: err=%v lease=%t release=%t timers=%d", admitted.err, admitted.ctx != nil, admitted.release != nil, timers)
			}
			coordinator.mu.Lock()
			accounts := len(coordinator.accounts)
			coordinator.mu.Unlock()
			if accounts != 0 {
				t.Fatal("expired admission retained an account lease or gate")
			}
		})
	}
}
