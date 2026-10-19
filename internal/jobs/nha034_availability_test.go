package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// A loopback-only connection outage, with a real temporary PostgreSQL behind
// it. No ambient database, household endpoint, or administrative PG is used.
type nha034PGProxy struct {
	mu          sync.Mutex
	listener    net.Listener
	target      string
	unavailable bool
	connections map[net.Conn]struct{}
	done        chan struct{}
	workers     sync.WaitGroup
}

func nha034Proxy(t *testing.T, port int) *nha034PGProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &nha034PGProxy{listener: listener, target: fmt.Sprintf("127.0.0.1:%d", port), connections: make(map[net.Conn]struct{}), done: make(chan struct{})}
	go func() {
		defer close(proxy.done)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.mu.Lock()
			blocked := proxy.unavailable
			if !blocked {
				proxy.connections[client] = struct{}{}
			}
			proxy.mu.Unlock()
			if blocked {
				_ = client.Close()
				continue
			}
			proxy.workers.Add(1)
			go proxy.forward(client)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-proxy.done
		proxy.outage(true)
		proxy.workers.Wait()
	})
	return proxy
}
func (p *nha034PGProxy) forward(client net.Conn) {
	defer p.workers.Done()
	defer client.Close()
	defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	server, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer server.Close()
	finished := make(chan struct{})
	go func() { _, _ = io.Copy(server, client); _ = server.Close(); close(finished) }()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	<-finished
}
func (p *nha034PGProxy) outage(unavailable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unavailable = unavailable
	if unavailable {
		for connection := range p.connections {
			_ = connection.Close()
		}
	}
}

type nha034PollGate struct {
	duration time.Duration
	resume   chan struct{}
}
type nha034GateTimer struct{ entered chan nha034PollGate }

func (t *nha034GateTimer) Wait(ctx context.Context, duration time.Duration) error {
	gate := nha034PollGate{duration, make(chan struct{})}
	select {
	case t.entered <- gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-gate.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func nha034Poll(t *testing.T, ctx context.Context, timer *nha034GateTimer) nha034PollGate {
	t.Helper()
	select {
	case gate := <-timer.entered:
		return gate
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nha034PollGate{}
	}
}

func TestNHA034PostgresOutageBackoffResumesSameSchedulerWithoutRAMFallback(t *testing.T) {
	cluster := testpg.Start(t)
	proxy := nha034Proxy(t, cluster.Port)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: proxy.listener.Addr().(*net.TCPAddr).Port, Database: "postgres", User: "homeward_test", PasswordEnv: "NHA034_PROXY_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "NHA034_PROXY_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repository, _ := NewRepository(store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := &nha034Clock{now: now}
	timer := &nha034GateTimer{entered: make(chan nha034PollGate, 4)}
	backup := &nha034Backup{}
	executor, _ := NewExecutor(nil, backup, clock)
	scheduler, _ := NewScheduler(repository, executor, clock, timer)
	if err := scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := scheduler.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	first := nha034Poll(t, ctx, timer)
	second := nha034Poll(t, ctx, timer)
	proxy.outage(true)
	missingOccurrence, missingKey := nha034IDs("unacknowledged-outage")
	if result, outcome, err := scheduler.Schedule(ctx, nha034Definition(t, "unacknowledged-outage", now), missingOccurrence, missingKey); result != CreateUnspecified || outcome == storage.TransactionCommitted || err == nil {
		t.Fatalf("outage produced durable ACK: %d %d %v", result, outcome, err)
	}
	close(first.resume)
	degraded := nha034Poll(t, ctx, timer)
	if degraded.duration != PollInterval || scheduler.Status().Phase() != SchedulerStorageDegraded {
		t.Fatal("SQL outage did not suspend jobs with explicit degraded state")
	}
	for _, expected := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		close(degraded.resume)
		degraded = nha034Poll(t, ctx, timer)
		if degraded.duration != expected {
			t.Fatalf("DB backoff=%s want=%s", degraded.duration, expected)
		}
	}
	select {
	case <-scheduler.Done():
		t.Fatal("transient DB outage terminated scheduler")
	default:
	}
	proxy.outage(false)
	close(degraded.resume)
	recovered := nha034Poll(t, ctx, timer)
	if recovered.duration != PollInterval || scheduler.Status().Phase() != SchedulerRunning {
		t.Fatalf("same scheduler did not recover: phase=%d delay=%s", scheduler.Status().Phase(), recovered.duration)
	}
	if _, found, err := repository.Get(ctx, missingOccurrence); err != nil || found {
		t.Fatalf("RAM fallback survived SQL outage: found=%v err=%v", found, err)
	}
	definition := nha034Definition(t, "recovery-witness", now)
	occurrence, key := nha034IDs("recovery-witness")
	if result, outcome, err := scheduler.Schedule(ctx, definition, occurrence, key); result != Created || outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("recovered durable creation=%d %d %v", result, outcome, err)
	}
	close(second.resume)
	_ = nha034Poll(t, ctx, timer)
	if record := nha034Get(t, repository, occurrence); record.State() != Succeeded || backup.effects.Load() != 1 {
		t.Fatalf("recovered execution=%d effects=%d", record.State(), backup.effects.Load())
	}
	if err := scheduler.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := scheduler.Schedule(ctx, definition, occurrence, key); outcome != storage.TransactionNotCommitted || !errors.Is(err, failure(Closed)) {
		t.Fatalf("closed job admissions accepted: %d %v", outcome, err)
	}
}

func TestNHA034CorruptDefinitionClosesOnlyJobAdmissionsWithExplicitFailure(t *testing.T) {
	store, repository, _ := nha034Store(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	definition := nha034Definition(t, "corrupt", now)
	occurrence := nha034Schedule(t, repository, definition, "corrupt")
	// Isolated corruption fixture: production has both the immutable trigger
	// and closed SQL constraint, already qualified by the schema test.
	nha034Exec(t, store, `ALTER TABLE public.scheduled_job_definitions DISABLE TRIGGER scheduled_job_definition_immutable;
        UPDATE public.scheduled_job_definitions SET timezone='Homeward/Unknown' WHERE job_id='corrupt'`)
	clock := &nha034Clock{now: now}
	executor, _ := NewExecutor(nil, &nha034Backup{}, clock)
	scheduler, _ := NewScheduler(repository, executor, clock, RealTimer{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scheduler.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if status := scheduler.Status(); status.Phase() != SchedulerFailed || status.Reason() != Corrupt {
		t.Fatalf("integrity failure hidden: phase=%d reason=%d", status.Phase(), status.Reason())
	}
	if err := scheduler.Close(ctx); !errors.Is(err, failure(Corrupt)) {
		t.Fatalf("integrity close outcome=%v", err)
	}
	_, key := nha034IDs("corrupt")
	if _, outcome, err := scheduler.Schedule(ctx, definition, occurrence, key); outcome != storage.TransactionNotCommitted || !errors.Is(err, failure(Closed)) {
		t.Fatalf("failed scheduler accepted creation: %d %v", outcome, err)
	}
}
