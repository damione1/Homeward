package integrations_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha171ProxyFault uint8

const (
	nha171NoFault nha171ProxyFault = iota
	nha171HoldReleaseReply
	nha171CutReleaseTransport
)

// nha171TerminationProxy forwards PostgreSQL traffic unchanged. It holds the
// frontend's Terminate message and the server-side EOF until cleanup, so a
// client socket closing cannot release the backend's session lock by itself.
type nha171TerminationProxy struct {
	listener     net.Listener
	server       string
	held         chan struct{}
	release      chan struct{}
	replyHeld    chan struct{}
	replyRelease chan struct{}
	releaseSeen  chan struct{}
	fault        nha171ProxyFault
	errors       chan error
	heldOnce     sync.Once
	replyOnce    sync.Once
	releaseOnce  sync.Once
	stopOnce     sync.Once
	mu           sync.Mutex
	closing      bool
	conns        []net.Conn
	wg           sync.WaitGroup
}

func nha171ProxyWithFault(t *testing.T, serverPort int, fault nha171ProxyFault) *nha171TerminationProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha171TerminationProxy{
		listener:     listener,
		server:       fmt.Sprintf("127.0.0.1:%d", serverPort),
		held:         make(chan struct{}),
		release:      make(chan struct{}),
		replyHeld:    make(chan struct{}),
		replyRelease: make(chan struct{}),
		releaseSeen:  make(chan struct{}),
		fault:        fault,
		errors:       make(chan error, 16),
	}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

func (p *nha171TerminationProxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		closing := p.closing
		p.mu.Unlock()
		if closing {
			_ = client.Close()
			return
		}
		server, err := net.DialTimeout("tcp", p.server, 2*time.Second)
		if err != nil {
			_ = client.Close()
			p.report(err)
			continue
		}
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			return
		}
		p.conns = append(p.conns, client, server)
		p.wg.Add(1)
		p.mu.Unlock()
		go p.forward(client, server)
	}
}

func (p *nha171TerminationProxy) forward(client, server net.Conn) {
	defer p.wg.Done()
	serverDone := make(chan struct{})
	holdReply := &atomic.Bool{}
	go func() {
		if p.fault != nha171HoldReleaseReply {
			_, _ = io.Copy(client, server)
		} else if err := p.forwardBackend(client, server, holdReply); err != nil {
			p.report(err)
		}
		close(serverDone)
	}()
	if err := p.forwardFrontend(client, server, holdReply); err != nil {
		p.report(err)
	}
	// Even an unexpected local EOF must not become a backend EOF before the
	// witness query has checked the lock.
	<-p.release
	_ = server.Close()
	_ = client.Close()
	<-serverDone
}

func (p *nha171TerminationProxy) forwardBackend(client, server net.Conn, holdReply *atomic.Bool) error {
	for {
		var header [5]byte
		if _, err := io.ReadFull(server, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 16<<20 {
			return fmt.Errorf("invalid PostgreSQL backend message length %d", length)
		}
		packet := make([]byte, length+1)
		copy(packet, header[:])
		if _, err := io.ReadFull(server, packet[5:]); err != nil {
			return err
		}
		if holdReply.Load() && header[0] == 'D' {
			// DataRow is the real SQL result from PostgreSQL, not a proxy
			// answer. Do not deliver it to pgx until the test has inspected
			// the uncertain shutdown result.
			p.replyOnce.Do(func() { close(p.replyHeld) })
			<-p.replyRelease
		}
		if err := nha171WriteAll(client, packet); err != nil {
			return err
		}
	}
}

func (p *nha171TerminationProxy) forwardFrontend(client, server net.Conn, holdReply *atomic.Bool) error {
	// Startup has no message-type byte. The test uses sslmode=disable and
	// protocol 3.0, so no one-byte SSL/GSS negotiation response is expected.
	var startupHeader [4]byte
	if _, err := io.ReadFull(client, startupHeader[:]); err != nil {
		return err
	}
	startupLength := binary.BigEndian.Uint32(startupHeader[:])
	if startupLength < 8 || startupLength > 1<<20 {
		return fmt.Errorf("invalid PostgreSQL startup length %d", startupLength)
	}
	startup := make([]byte, startupLength)
	copy(startup, startupHeader[:])
	if _, err := io.ReadFull(client, startup[4:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(startup[4:8]) != 196608 {
		return fmt.Errorf("unexpected PostgreSQL startup protocol")
	}
	if err := nha171WriteAll(server, startup); err != nil {
		return err
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(client, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 16<<20 {
			return fmt.Errorf("invalid PostgreSQL message length %d", length)
		}
		packet := make([]byte, length+1)
		copy(packet, header[:])
		if _, err := io.ReadFull(client, packet[5:]); err != nil {
			return err
		}
		if p.fault != nha171NoFault && (header[0] == 'Q' || header[0] == 'P') && bytes.Contains(packet, []byte("pg_advisory_unlock")) {
			p.releaseOnce.Do(func() { close(p.releaseSeen) })
			switch p.fault {
			case nha171HoldReleaseReply:
				holdReply.Store(true)
			case nha171CutReleaseTransport:
				_ = client.Close()
				return nil
			}
		}
		if header[0] == 'X' {
			if length != 4 {
				return fmt.Errorf("invalid PostgreSQL Terminate length %d", length)
			}
			p.heldOnce.Do(func() { close(p.held) })
			<-p.release
		}
		if err := nha171WriteAll(server, packet); err != nil {
			return err
		}
		if header[0] == 'X' {
			return nil
		}
	}
}

func nha171WriteAll(conn net.Conn, packet []byte) error {
	for len(packet) > 0 {
		n, err := conn.Write(packet)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		packet = packet[n:]
	}
	return nil
}

func (p *nha171TerminationProxy) report(err error) {
	select {
	case p.errors <- err:
	default:
	}
}

func (p *nha171TerminationProxy) close() {
	p.stopOnce.Do(func() {
		close(p.release)
		close(p.replyRelease)
		p.mu.Lock()
		p.closing = true
		conns := append([]net.Conn(nil), p.conns...)
		p.mu.Unlock()
		_ = p.listener.Close()
		for _, conn := range conns {
			_ = conn.Close()
		}
		p.wg.Wait()
	})
}

func nha171ProxiedService(t *testing.T, fault nha171ProxyFault) (*storage.Store, *nha171TerminationProxy, *integrations.DurableCommandService, nha023Rig) {
	t.Helper()
	witness, _, cluster := nha024StoreCluster(t)
	proxy := nha171ProxyWithFault(t, cluster.Port, fault)
	settings, err := storage.ParseSettings(storage.ConnectionInput{
		Host: "127.0.0.1", Port: proxy.listener.Addr().(*net.TCPAddr).Port,
		Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable",
	}, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	guarded, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guarded.Close)
	rig := nha023SetupBare(t, &nha022Driver{})
	service, err := integrations.NewDurableCommandService(context.Background(), rig.host, rig.gen, guarded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			var releaseErr *storage.RuntimeReleaseError
			if !errors.As(err, &releaseErr) {
				t.Error(err)
			}
		}
	})
	return witness, proxy, service, rig
}

func TestNHA171CloseRequiresAcknowledgedPhysicalLockRelease(t *testing.T) {
	witness, proxy, service, _ := nha171ProxiedService(t, nha171NoFault)
	if runtime, err := witness.AcquireCommandRuntime(context.Background()); err == nil {
		defer runtime.Close()
		t.Fatal("independent witness acquired the live guard")
	} else {
		var storageErr *storage.Error
		if !errors.As(err, &storageErr) || storageErr.Kind() != storage.CommandRuntimeBusy {
			t.Fatalf("independent witness failed for another reason: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatalf("service Close: %v", err)
	}
	select {
	case <-proxy.held:
	case err := <-proxy.errors:
		t.Fatalf("proxy failed before retaining Terminate: %v", err)
	case <-ctx.Done():
		t.Fatalf("Terminate was not retained: %v", ctx.Err())
	}
	successor, err := witness.AcquireCommandRuntime(context.Background())
	if err != nil {
		t.Fatalf("successful Close left the backend lock held while Terminate and EOF were retained: %v", err)
	}
	defer successor.Close()
}

func TestNHA171ReleaseReplyMustBeObserved(t *testing.T) {
	_, proxy, service, _ := nha171ProxiedService(t, nha171HoldReleaseReply)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	err := service.Close(ctx)
	var releaseErr *storage.RuntimeReleaseError
	if !errors.As(err, &releaseErr) {
		t.Fatalf("unacknowledged SQL release claimed success or a different result: %v", err)
	}
	select {
	case <-proxy.releaseSeen:
	case <-ctx.Done():
		t.Fatalf("SQL release was not sent: %v", ctx.Err())
	}
	select {
	case <-proxy.replyHeld:
	case <-ctx.Done():
		t.Fatalf("real PostgreSQL response was not retained: %v", ctx.Err())
	}
	if again := service.Close(context.Background()); again != err {
		t.Fatalf("repeat Close changed an uncertain outcome: first=%v next=%v", err, again)
	}
}

func TestNHA171ReleaseTransportFailureKeepsGuardUnavailable(t *testing.T) {
	witness, proxy, service, _ := nha171ProxiedService(t, nha171CutReleaseTransport)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := service.Close(ctx)
	var releaseErr *storage.RuntimeReleaseError
	if !errors.As(err, &releaseErr) {
		t.Fatalf("failed SQL transport claimed success or a different result: %v", err)
	}
	select {
	case <-proxy.releaseSeen:
	case <-ctx.Done():
		t.Fatalf("SQL release was not attempted: %v", ctx.Err())
	}
	if runtime, err := witness.AcquireCommandRuntime(context.Background()); err == nil {
		defer runtime.Close()
		t.Fatal("backend guard released after the release query was cut before forwarding")
	} else {
		var storageErr *storage.Error
		if !errors.As(err, &storageErr) || storageErr.Kind() != storage.CommandRuntimeBusy {
			t.Fatalf("independent witness failed for another reason: %v", err)
		}
	}
	if again := service.Close(context.Background()); again != err {
		t.Fatalf("repeat Close changed a transport failure: first=%v next=%v", err, again)
	}
}

func TestNHA171SupervisorRecordsLateReleaseFailure(t *testing.T) {
	_, proxy, service, rig := nha171ProxiedService(t, nha171HoldReleaseReply)
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	err := supervisor.Stop(ctx)
	var releaseErr *storage.RuntimeReleaseError
	if !errors.Is(err, integrations.ErrPilotJoinTimeout) || !errors.As(err, &releaseErr) {
		t.Fatalf("supervisor lost the late release failure after its first timeout: %v", err)
	}
	select {
	case <-proxy.replyHeld:
	case <-ctx.Done():
		t.Fatalf("real PostgreSQL release response was not retained: %v", ctx.Err())
	}
	if again := service.Close(context.Background()); !errors.As(again, &releaseErr) {
		t.Fatalf("service lost release failure after Done: %v", again)
	}
	if again := supervisor.Stop(context.Background()); !errors.As(again, &releaseErr) {
		t.Fatalf("repeat Stop lost the late release failure: %v", again)
	}
}

func TestNHA171CanceledCloseDrainsWithoutReplay(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	rig := nha023SetupBare(t, driver)
	service := nha024Service(t, rig, store)
	var releaseOnce sync.Once
	releaseDriver := func() { releaseOnce.Do(func() { close(driver.release) }) }
	finished := make(chan error, 1)
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		response, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		if err == nil && (response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED) {
			err = fmt.Errorf("unexpected dispatched response: %v", response)
		}
		finished <- err
	}()
	t.Cleanup(func() {
		releaseDriver()
		select {
		case <-dispatchDone:
		case <-time.After(5 * time.Second):
			t.Error("fake dispatch did not join during cleanup")
		}
	})
	select {
	case <-driver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fake dispatch did not enter")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Close = %v", err)
	}
	deadline, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if err := service.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out Close = %v", err)
	}
	if got, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request); got != nil || !errors.Is(err, integrations.ErrDurableLedgerUnavailable) {
		t.Fatalf("new admission after Close = %v %v", got, err)
	}
	secondRig := nha023SetupBare(t, &nha022Driver{})
	if _, err := integrations.NewDurableCommandService(context.Background(), secondRig.host, secondRig.gen, store); !errors.Is(err, integrations.ErrDurableRuntimeBusy) {
		t.Fatalf("successor admitted while fake dispatch lived: %v", err)
	}
	releaseDriver()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake dispatch did not drain")
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := nha024Service(t, secondRig, store)
	replay, err := second.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || replay == nil || replay.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || secondRig.driver.count() != 0 || driver.count() != 1 {
		t.Fatalf("replay after drain = %v %v old=%d new=%d", replay, err, driver.count(), secondRig.driver.count())
	}
}
