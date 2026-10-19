package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	"github.com/damione1/Homeward/gen/home/integrations/v1/integrationsv1connect"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Transport controls remain separate from the shared-worker admission oracles.
// TestNHA185AdmissionHeaders keeps the transport client that was actually red
// on the archived parent. Separate oracles exercise the real shared worker helper.
type nha185Call uint8

const (
	nha185InvalidCall nha185Call = iota
	nha185First
	nha185Probe
)

func nha185ParseCall(header string) (nha185Call, error) {
	switch header {
	case "first":
		return nha185First, nil
	case "probe":
		return nha185Probe, nil
	default:
		return nha185InvalidCall, fmt.Errorf("nha185: invalid test call identity %q", header)
	}
}

type nha185Mode string

type nha185Client uint8

const (
	nha185TransportClient nha185Client = iota + 1
	nha185WorkerClient
)

const (
	nha185Headers              nha185Mode = "headers"
	nha185Old                  nha185Mode = "old"
	nha185Admission            nha185Mode = "admission"
	nha185AdmissionConformance nha185Mode = "admission_conformance"
	nha185BadAuth              nha185Mode = "bad_auth"
	nha185NoCapability         nha185Mode = "no_capability"
	nha185LargeFrame           nha185Mode = "large_frame"
	nha185CancelPending        nha185Mode = "cancel_pending"
	nha185CancelProbe          nha185Mode = "cancel_probe"
	nha185DelayedRefusal       nha185Mode = "delayed_refusal"
	nha185Assertion            nha185Mode = "assertion"
	nha185MetadataError        nha185Mode = "metadata_error"
)

type nha185Event struct {
	Event     string `json:"event"`
	Code      string `json:"code,omitempty"`
	FirstCode string `json:"first_code,omitempty"`
	ProbeCode string `json:"probe_code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Size      int    `json:"size,omitempty"`
}

func (e nha185Event) valid() bool {
	switch e.Event {
	case "connected", "first_created", "metadata_waiting", "metadata_received", "headers", "refused", "probe_started",
		"old_result", "ready", "cancelled", "failure":
		return true
	default:
		return false
	}
}

type nha185CleanupReport struct {
	Event         string `json:"event"`
	ThreadsJoined bool   `json:"threads_joined"`
	CallsEnded    bool   `json:"calls_ended"`
	Messages      int    `json:"messages"`
	Phase         string `json:"phase"`
	Error         string `json:"error"`
}

type nha185SyntheticHeaders struct {
	integrationsv1connect.UnimplementedHostServiceHandler
	allow  <-chan struct{}
	refuse <-chan struct{}
}

func (s *nha185SyntheticHeaders) Commands(ctx context.Context, _ *connect.Request[wire.CommandsRequest], stream *connect.ServerStream[wire.CommandsResponse]) error {
	select {
	case <-s.allow:
	case <-ctx.Done():
		return ctx.Err()
	}
	// nil must reach Connect as nil, not as an empty CommandsResponse message.
	if err := stream.Send(nil); err != nil {
		return err
	}
	if s.refuse != nil {
		select {
		case <-s.refuse:
			return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("nha185: controlled terminal refusal"))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

type nha185ProbeReturn struct {
	contextErr error
	serviceErr error
}

// Only the old-algorithm control delays Connect's publication of the real
// probe result. The pilot still acquires ownership, expires and runs its defers.
type nha185OldProbeGate struct {
	*pilot
	stopped     <-chan struct{}
	observed    chan nha185ProbeReturn
	release     chan struct{}
	returned    chan struct{}
	releaseOnce sync.Once
}

func (g *nha185OldProbeGate) Commands(ctx context.Context, request *connect.Request[wire.CommandsRequest], stream *connect.ServerStream[wire.CommandsResponse]) error {
	call, err := nha185ParseCall(request.Header().Get("X-Nha185-Call"))
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if call != nha185Probe {
		return g.pilot.Commands(ctx, request, stream)
	}
	defer close(g.returned)
	err = g.pilot.Commands(ctx, request, stream)
	g.observed <- nha185ProbeReturn{contextErr: ctx.Err(), serviceErr: err}
	select {
	case <-g.release:
	case <-g.stopped:
	}
	return err
}

func (g *nha185OldProbeGate) releaseReturn() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// Preserve the actual HTTP/2 Flusher while counting application-body bytes.
type nha185ResponseWriter struct {
	http.ResponseWriter
	body *atomic.Int64
}

func (w *nha185ResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.body.Add(int64(n))
	return n, err
}

func (w *nha185ResponseWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
}

type nha185Rig struct {
	client          nha185Client
	phase           context.Context
	phaseCancel     context.CancelFunc
	lifeCancel      context.CancelFunc
	directory       string
	listener        *trackedListener
	server          *http.Server
	serveDone       chan struct{}
	control         net.Listener
	connection      net.Conn
	readerDone      chan struct{}
	readerStop      chan struct{}
	events          chan nha185Event
	readErrors      chan error
	firstHeld       chan struct{}
	release         chan struct{}
	terminalRelease chan struct{}
	releaseOnce     sync.Once
	heldOnce        sync.Once
	pilot           *pilot
	oldProbeGate    *nha185OldProbeGate
	process         *exec.Cmd
	processDone     chan struct{}
	processErr      error // read only after processDone closes
	stdout          bytes.Buffer
	stderr          bytes.Buffer
	bodyBytes       atomic.Int64
	mu              sync.Mutex
	closing         bool
	delegated       map[nha185Call]int
	handlers        sync.WaitGroup
	closeOnce       sync.Once
}

func nha185NewRig(t *testing.T, transport string, mode nha185Mode, synthetic, holdFirst bool) *nha185Rig {
	return nha185StartRig(t, transport, mode, synthetic, holdFirst, nha185TransportClient)
}

func nha185NewSharedRig(t *testing.T, transport string, mode nha185Mode, synthetic, holdFirst bool) *nha185Rig {
	return nha185StartRig(t, transport, mode, synthetic, holdFirst, nha185WorkerClient)
}

func nha185StartRig(t *testing.T, transport string, mode nha185Mode, synthetic, holdFirst bool, client nha185Client) *nha185Rig {
	t.Helper()
	var program string
	switch client {
	case nha185TransportClient:
		program = nha185Python
	case nha185WorkerClient:
		program = nha185SharedPython
	default:
		t.Fatalf("invalid test client %d", client)
	}
	switch mode {
	case nha185Headers, nha185DelayedRefusal, nha185Assertion:
		if !synthetic || holdFirst {
			t.Fatal("header-only fixture requires the synthetic handler without an ownership gate")
		}
	case nha185Old, nha185Admission, nha185AdmissionConformance, nha185BadAuth, nha185NoCapability, nha185LargeFrame, nha185CancelProbe, nha185MetadataError:
		if synthetic {
			t.Fatal("ownership and admission cases require the real pilot")
		}
	case nha185CancelPending:
		if synthetic && holdFirst {
			t.Fatal("synthetic cancellation uses the header gate, not the ownership gate")
		}
	default:
		t.Fatalf("invalid test mode %q", mode)
	}
	phase, phaseCancel := context.WithTimeout(context.Background(), startupTimeout)
	life, lifeCancel := context.WithCancel(context.Background())
	r := &nha185Rig{
		client: client,
		phase:  phase, phaseCancel: phaseCancel, lifeCancel: lifeCancel,
		serveDone: make(chan struct{}), readerDone: make(chan struct{}),
		readerStop: make(chan struct{}), events: make(chan nha185Event, 16),
		readErrors: make(chan error, 1), firstHeld: make(chan struct{}),
		release: make(chan struct{}), delegated: make(map[nha185Call]int),
	}
	t.Cleanup(func() { r.close(t) })
	var err error
	r.directory, err = os.MkdirTemp("/tmp", "h185-")
	if err != nil {
		t.Fatal(err)
	}
	var service integrationsv1connect.HostServiceHandler
	if synthetic {
		allow := make(chan struct{})
		if mode == nha185Headers || mode == nha185DelayedRefusal {
			close(allow)
		}
		if mode == nha185DelayedRefusal {
			r.terminalRelease = make(chan struct{})
		}
		service = &nha185SyntheticHeaders{allow: allow, refuse: r.terminalRelease}
	} else {
		host, _, fixtureErr := fake.NewFixture()
		if fixtureErr != nil {
			t.Fatal(fixtureErr)
		}
		instance, parseErr := domain.ParseIntegrationInstanceID("nha185-synthetic-only")
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		session, sessionErr := host.OpenSession(instance)
		if sessionErr != nil {
			t.Fatal(sessionErr)
		}
		p := newPilot(Config{})
		p.host, p.ctx, p.active, p.handshaken = session, life, true, true
		p.token, p.frameLimit, p.lastHeartbeat = "nha185-local-token", 1024, time.Now()
		p.capabilities = map[wire.Capability]bool{wire.Capability_CAPABILITY_COMMAND: true}
		if mode == nha185NoCapability {
			p.capabilities = make(map[wire.Capability]bool)
		}
		r.pilot, service = p, p
		if mode == nha185Old {
			r.oldProbeGate = &nha185OldProbeGate{
				pilot: p, stopped: life.Done(), observed: make(chan nha185ProbeReturn, 1),
				release: make(chan struct{}), returned: make(chan struct{}),
			}
			service = r.oldProbeGate
		}
	}
	path, handler := integrationsv1connect.NewHostServiceHandler(service,
		connect.WithReadMaxBytes(maxFrame), connect.WithSendMaxBytes(maxFrame))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		if r.closing {
			r.mu.Unlock()
			http.Error(w, "test server closing", http.StatusServiceUnavailable)
			return
		}
		r.handlers.Add(1)
		r.mu.Unlock()
		defer r.handlers.Done()
		if request.ProtoMajor != 2 || !strings.HasPrefix(request.Header.Get("Content-Type"), "application/grpc") {
			http.Error(w, "gRPC required", http.StatusUnsupportedMediaType)
			return
		}
		if request.URL.Path != integrationsv1connect.HostServiceCommandsProcedure {
			http.Error(w, "only synthetic Commands allowed", http.StatusNotFound)
			return
		}
		call, callErr := nha185ParseCall(request.Header.Get("X-Nha185-Call"))
		if callErr != nil {
			http.Error(w, callErr.Error(), http.StatusBadRequest)
			return
		}
		if holdFirst && call == nha185First {
			r.heldOnce.Do(func() { close(r.firstHeld) })
			select {
			case <-r.release:
			case <-request.Context().Done():
				return
			case <-life.Done():
				return
			}
		}
		r.mu.Lock()
		if r.closing || request.Context().Err() != nil {
			r.mu.Unlock()
			return
		}
		r.delegated[call]++
		r.mu.Unlock()
		mux.ServeHTTP(&nha185ResponseWriter{ResponseWriter: w, body: &r.bodyBytes}, request)
	})
	var raw net.Listener
	var target string
	switch transport {
	case "unix":
		socket := filepath.Join(r.directory, "host.sock")
		raw, err = net.Listen("unix", socket)
		target = "unix:" + socket
	case "tcp":
		raw, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			target = raw.Addr().String()
		}
	default:
		t.Fatalf("invalid test transport %q", transport)
	}
	if err != nil {
		t.Fatal(err)
	}
	r.listener = newTrackedListener(raw)
	r.server = &http.Server{
		ReadHeaderTimeout: rpcTimeout, IdleTimeout: leaseTimeout, MaxHeaderBytes: 8192,
		Handler: h2c.NewHandler(wrapped, &http2.Server{MaxConcurrentStreams: maxRPCs}),
	}
	go func() {
		defer close(r.serveDone)
		_ = r.server.Serve(r.listener)
	}()
	controlPath := filepath.Join(r.directory, "control.sock")
	r.control, err = net.Listen("unix", controlPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ := phase.Deadline()
	if err := r.control.(*net.UnixListener).SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	python := os.Getenv("HOMEWARD_NHA185_PYTHON")
	if python == "" {
		pointer, pointerErr := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
		if pointerErr != nil {
			t.Fatal(pointerErr)
		}
		python = strings.TrimSpace(string(pointer))
	}
	if !filepath.IsAbs(python) {
		t.Fatal("nha185 requires an explicit absolute Python path")
	}
	r.process = exec.Command(python, "-u", "-c", program, root, target, controlPath,
		string(mode), strconv.FormatInt(deadline.UnixNano(), 10))
	r.process.Env = []string{"PYTHONDONTWRITEBYTECODE=1"}
	r.process.Stdout, r.process.Stderr = &r.stdout, &r.stderr
	if err := configureProcess(r.process); err != nil {
		t.Fatal(err)
	}
	if err := r.process.Start(); err != nil {
		r.process = nil
		t.Fatal(err)
	}
	r.processDone = make(chan struct{})
	go func() {
		r.processErr = r.process.Wait()
		close(r.processDone)
	}()
	r.connection, err = r.control.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go r.readControl()
	return r
}

func (r *nha185Rig) readControl() {
	defer close(r.readerDone)
	reader := bufio.NewReaderSize(r.connection, 4096)
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			select {
			case r.readErrors <- err:
			default:
			}
			return
		}
		var event nha185Event
		if err := json.Unmarshal(line, &event); err != nil || !event.valid() {
			if err == nil {
				err = fmt.Errorf("unknown test event %q", event.Event)
			}
			r.readErrors <- err
			return
		}
		select {
		case r.events <- event:
		case <-r.readerStop:
			return
		}
	}
}

func (r *nha185Rig) event(wanted string) (nha185Event, error) {
	for {
		select {
		case event := <-r.events:
			if event.Event == wanted {
				return event, nil
			}
			if event.Event == "failure" {
				return event, fmt.Errorf("Python phase failure: %s", event.Reason)
			}
			if event.Event == "ready" {
				return event, fmt.Errorf("unexpected readiness while waiting for %s", wanted)
			}
		case err := <-r.readErrors:
			return nha185Event{}, fmt.Errorf("control read: %w", err)
		case <-r.phase.Done():
			return nha185Event{}, r.phase.Err()
		}
	}
}

func (r *nha185Rig) await(t *testing.T, wanted string) nha185Event {
	t.Helper()
	event, err := r.event(wanted)
	if err != nil {
		t.Fatalf("waiting for %s: %v", wanted, err)
	}
	t.Logf("Python event: %+v", event)
	return event
}

func (r *nha185Rig) command(t *testing.T, op string) {
	t.Helper()
	deadline, _ := r.phase.Deadline()
	if err := r.connection.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(r.connection).Encode(map[string]string{"op": op}); err != nil {
		t.Fatal(err)
	}
}

func (r *nha185Rig) wait(t *testing.T, channel <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-channel:
	case <-r.phase.Done():
		t.Fatalf("%s: %v", what, r.phase.Err())
	}
}

func (r *nha185Rig) releaseFirst() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// Every join below uses this same absolute stop deadline, never a fresh 5 s.
func nha185Join(deadline time.Time, done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (r *nha185Rig) close(t *testing.T) {
	t.Helper()
	r.closeOnce.Do(func() {
		deadline := time.Now().Add(stopTimeout)
		if r.connection != nil {
			_ = r.connection.SetWriteDeadline(deadline)
			_ = json.NewEncoder(r.connection).Encode(map[string]any{
				"op": "stop", "deadline_ns": deadline.UnixNano(),
			})
		}
		r.mu.Lock()
		r.closing = true
		r.mu.Unlock()
		r.releaseFirst()
		if r.oldProbeGate != nil {
			r.oldProbeGate.releaseReturn()
		}
		r.lifeCancel()
		if r.pilot != nil {
			r.pilot.mu.Lock()
			r.pilot.active = false
			r.pilot.mu.Unlock()
		}
		if r.listener != nil {
			r.listener.CloseAll()
		}
		if r.server != nil {
			_ = r.server.Close()
		}
		if r.control != nil {
			_ = r.control.Close()
		}
		if r.connection != nil {
			_ = r.connection.Close()
		}
		close(r.readerStop)
		joinedHandlers := make(chan struct{})
		go func() { r.handlers.Wait(); close(joinedHandlers) }()
		joinedRPCs := make(chan struct{})
		go func() {
			if r.pilot != nil {
				r.pilot.rpcWG.Wait()
			}
			close(joinedRPCs)
		}()
		if r.processDone != nil {
			// Leave the last second of the one stop budget for TERM/KILL and Wait.
			grace := deadline.Add(-time.Second)
			if !nha185Join(grace, r.processDone) {
				t.Error("Python required process-group termination instead of explicit joins")
				_ = signalProcessGroup(r.process.Process.Pid, syscall.SIGTERM)
				if !nha185Join(deadline.Add(-500*time.Millisecond), r.processDone) {
					_ = signalProcessGroup(r.process.Process.Pid, syscall.SIGKILL)
				}
			}
			if !nha185Join(deadline, r.processDone) {
				t.Error("Python process did not join within the shared stop deadline")
			} else {
				t.Logf("Python stdout: %s; stderr: %s", r.stdout.String(), r.stderr.String())
				if r.processErr != nil {
					t.Errorf("Python process: %v", r.processErr)
				}
				var report nha185CleanupReport
				if err := json.Unmarshal(bytes.TrimSpace(r.stdout.Bytes()), &report); err != nil {
					t.Errorf("missing cleanup report: %v", err)
				} else if report.Event != "cleaned" || !report.ThreadsJoined || !report.CallsEnded || report.Messages != 0 || report.Error != "" ||
					r.client == nha185TransportClient && report.Phase != "failed" && report.Phase != "cancelled" ||
					r.client == nha185WorkerClient && report.Phase != "closed" {
					t.Errorf("Python cleanup/zero-message proof: %+v", report)
				}
			}
		}
		for name, done := range map[string]<-chan struct{}{
			"handlers": joinedHandlers, "reserved RPCs": joinedRPCs,
		} {
			if !nha185Join(deadline, done) {
				t.Errorf("%s did not join within the shared stop deadline", name)
			}
		}
		if r.server != nil && !nha185Join(deadline, r.serveDone) {
			t.Error("HTTP server did not join")
		}
		if r.connection != nil && !nha185Join(deadline, r.readerDone) {
			t.Error("control reader did not join")
		}
		if r.listener != nil && !nha185Join(deadline, r.listener.Done()) {
			t.Error("tracked HTTP/2 connections did not join")
		}
		if r.bodyBytes.Load() != 0 {
			t.Errorf("readiness emitted %d body bytes", r.bodyBytes.Load())
		}
		if r.pilot != nil {
			r.pilot.mu.Lock()
			streaming := r.pilot.streaming
			slots, pending, queued, causes := len(r.pilot.rpcSlots), len(r.pilot.pending), len(r.pilot.queue), len(r.pilot.correlations)
			r.pilot.mu.Unlock()
			if streaming || slots != 0 || pending != 0 || queued != 0 || causes != 0 {
				t.Errorf("pilot cleanup: streaming=%v slots=%d pending=%d queued=%d correlations=%d", streaming, slots, pending, queued, causes)
			}
		}
		r.phaseCancel()
		if r.directory != "" {
			if err := os.RemoveAll(r.directory); err != nil {
				t.Errorf("remove owned test socket directory: %v", err)
			}
		}
	})
}

func TestNHA185PreflightHeaders(t *testing.T) {
	for _, transport := range []string{"unix", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			r := nha185NewRig(t, transport, nha185Headers, true, false)
			r.await(t, "headers")
			r.command(t, "cancel")
			if event := r.await(t, "cancelled"); event.Code != "CANCELLED" {
				t.Fatalf("header-only stream cancellation: %+v", event)
			}
		})
	}
}

func TestNHA185PreflightOldSecondOwner(t *testing.T) {
	nha185OldSecondOwner(t, nha185NewRig)
}

func TestNHA185SharedOldSecondOwner(t *testing.T) {
	nha185OldSecondOwner(t, nha185NewSharedRig)
}

type nha185RigConstructor func(*testing.T, string, nha185Mode, bool, bool) *nha185Rig

func nha185OldSecondOwner(t *testing.T, newRig nha185RigConstructor) {
	t.Helper()
	for _, transport := range []string{"unix", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			r := newRig(t, transport, nha185Old, false, true)
			r.wait(t, r.firstHeld, "first handler held before delegation")
			r.await(t, "first_created")
			r.command(t, "probe")
			r.wait(t, r.pilot.streamReady, "second handler acquired real pilot ownership")
			r.mu.Lock()
			first, probe := r.delegated[nha185First], r.delegated[nha185Probe]
			r.mu.Unlock()
			r.pilot.mu.Lock()
			streaming, ever := r.pilot.streaming, r.pilot.streamEver
			r.pilot.mu.Unlock()
			if first != 0 || probe != 1 || !streaming || !ever {
				t.Fatalf("second owner not forced: first=%d probe=%d streaming=%v ever=%v", first, probe, streaming, ever)
			}
			t.Log("real second handler owns Commands while first remains held")
			r.releaseFirst()
			result := r.await(t, "old_result")
			if result.FirstCode != "ALREADY_EXISTS" || result.ProbeCode != "DEADLINE_EXCEEDED" {
				t.Fatalf("old algorithm not characterized: %+v", result)
			}
			var observed nha185ProbeReturn
			select {
			case observed = <-r.oldProbeGate.observed:
			case <-r.phase.Done():
				t.Fatalf("real probe service return: %v", r.phase.Err())
			}
			if observed.serviceErr != nil {
				t.Fatalf("real probe service returned error: %v", observed.serviceErr)
			}
			var cause string
			switch observed.contextErr {
			case context.DeadlineExceeded:
				cause = "deadline_exceeded"
			case context.Canceled:
				cause = "canceled"
			default:
				t.Fatalf("real probe context cause is neither deadline nor cancellation: %v", observed.contextErr)
			}
			r.oldProbeGate.releaseReturn()
			r.wait(t, r.oldProbeGate.returned, "real probe facade returned after client deadline")
			r.wait(t, r.pilot.transportLost, "expired owning stream ended transport")
			t.Logf("real probe return gated until client deadline: context=%s service_error=nil; facade joined", cause)
		})
	}
}

func TestNHA185PreflightRefusalAndCancellation(t *testing.T) {
	nha185RefusalAndCancellation(t, nha185NewRig)
}

func TestNHA185SharedRefusalAndCancellation(t *testing.T) {
	nha185RefusalAndCancellation(t, nha185NewSharedRig)
}

func nha185RefusalAndCancellation(t *testing.T, newRig nha185RigConstructor) {
	t.Helper()
	for _, transport := range []string{"unix", "tcp"} {
		for _, test := range []struct {
			mode nha185Mode
			code string
		}{
			{nha185BadAuth, "UNAUTHENTICATED"},
			{nha185NoCapability, "PERMISSION_DENIED"},
			{nha185LargeFrame, "RESOURCE_EXHAUSTED"},
		} {
			t.Run(transport+"/"+string(test.mode), func(t *testing.T) {
				r := newRig(t, transport, test.mode, false, false)
				event := r.await(t, "refused")
				if event.Code != test.code || test.mode == nha185LargeFrame && event.Size <= 1024 {
					t.Fatalf("refusal before property: %+v", event)
				}
				r.pilot.mu.Lock()
				streaming, ever := r.pilot.streaming, r.pilot.streamEver
				r.pilot.mu.Unlock()
				select {
				case <-r.pilot.streamReady:
					t.Fatal("refused stream closed streamReady")
				default:
				}
				if streaming || ever {
					t.Fatal("refused request acquired stream ownership")
				}
			})
		}
		for _, synthetic := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cancel_pending/synthetic=%v", transport, synthetic), func(t *testing.T) {
				r := newRig(t, transport, nha185CancelPending, synthetic, false)
				r.await(t, "metadata_waiting")
				if !synthetic {
					r.wait(t, r.pilot.streamReady, "real stream admitted before cancellation")
					if r.client == nha185WorkerClient {
						r.await(t, "metadata_received")
						r.mu.Lock()
						probe := r.delegated[nha185Probe]
						r.mu.Unlock()
						if probe != 0 {
							t.Fatal("probe started before held metadata returned to the shared helper")
						}
					}
				}
				r.command(t, "cancel")
				if event := r.await(t, "cancelled"); event.Code != "CANCELLED" {
					t.Fatalf("metadata cancellation: %+v", event)
				}
				if !synthetic {
					r.wait(t, r.pilot.transportLost, "real admitted cancellation signals transport loss")
				}
			})
		}
		t.Run(transport+"/headers_before_terminal_refusal", func(t *testing.T) {
			r := newRig(t, transport, nha185DelayedRefusal, true, false)
			r.await(t, "headers")
			close(r.terminalRelease)
			if event := r.await(t, "refused"); event.Code != "PERMISSION_DENIED" {
				t.Fatalf("headers were confused with admission: %+v", event)
			}
		})
		t.Run(transport+"/cancel_before_property", func(t *testing.T) {
			r := newRig(t, transport, nha185CancelPending, false, true)
			r.wait(t, r.firstHeld, "cancelled first handler held before property")
			r.await(t, "metadata_waiting")
			r.command(t, "cancel")
			if event := r.await(t, "cancelled"); event.Code != "CANCELLED" {
				t.Fatalf("cancellation before property: %+v", event)
			}
			select {
			case <-r.pilot.streamReady:
				t.Fatal("cancellation before property closed streamReady")
			default:
			}
		})
		t.Run(transport+"/cancel_probe", func(t *testing.T) {
			r := newRig(t, transport, nha185CancelProbe, false, true)
			r.wait(t, r.firstHeld, "first handler held before probe cancellation")
			r.await(t, "first_created")
			r.command(t, "probe")
			r.wait(t, r.pilot.streamReady, "probe owns real stream before cancellation")
			r.await(t, "probe_started")
			r.releaseFirst()
			r.command(t, "cancel_probe")
			if result := r.await(t, "old_result"); result.FirstCode != "ALREADY_EXISTS" || result.ProbeCode != "CANCELLED" {
				t.Fatalf("cancellation during probe: %+v", result)
			}
			r.wait(t, r.pilot.transportLost, "cancelled owning probe signals transport loss")
		})
		t.Run(transport+"/assertion_cleanup", func(t *testing.T) {
			r := newRig(t, transport, nha185Assertion, true, false)
			if event := r.await(t, "failure"); !strings.Contains(event.Reason, "controlled_python_assertion") {
				t.Fatalf("unexpected controlled failure: %+v", event)
			}
			// Cleanup must cancel and join the already-started metadata waiter,
			// even though the client assertion failed before receiving headers.
		})
	}
}

func TestNHA185AdmissionHeaders(t *testing.T) {
	nha185AdmissionHeaders(t, nha185Admission, nha185NewRig)
}

func TestNHA185SharedAdmissionHeaders(t *testing.T) {
	nha185AdmissionHeaders(t, nha185Admission, nha185NewSharedRig)
}

func TestNHA185SharedConformanceAdmission(t *testing.T) {
	nha185AdmissionHeaders(t, nha185AdmissionConformance, nha185NewSharedRig)
}

func TestNHA185SharedMetadataFailure(t *testing.T) {
	for _, transport := range []string{"unix", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			r := nha185NewSharedRig(t, transport, nha185MetadataError, false, false)
			r.wait(t, r.pilot.streamReady, "first stream admitted before metadata failure")
			if event := r.await(t, "failure"); !strings.Contains(event.Reason, "metadata_failed") {
				t.Fatalf("metadata failure was hidden or admitted: %+v", event)
			}
		})
	}
}

func nha185AdmissionHeaders(t *testing.T, mode nha185Mode, newRig nha185RigConstructor) {
	t.Helper()
	for _, transport := range []string{"unix", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			r := newRig(t, transport, mode, false, true)
			r.wait(t, r.firstHeld, "first handler held")
			r.await(t, "metadata_waiting")
			r.mu.Lock()
			probeBeforeRelease := r.delegated[nha185Probe]
			r.mu.Unlock()
			if probeBeforeRelease != 0 {
				t.Fatal("probe reached server before first metadata barrier")
			}
			r.releaseFirst()
			r.wait(t, r.pilot.streamReady, "real first stream ownership")
			if _, err := r.event("headers"); err != nil {
				t.Fatalf("first_headers_missing on real admitted idle Commands: %v", err)
			}
			if event := r.await(t, "ready"); event.ProbeCode != "ALREADY_EXISTS" {
				t.Fatalf("readiness without rejected probe: %+v", event)
			}
			r.command(t, "cancel")
			r.await(t, "cancelled")
			r.wait(t, r.pilot.transportLost, "first stream cancellation signals transport loss")
		})
	}
}

// This minimal probe imports the actual generated Python clients. Its two-call
// old path is an explicit negative control, not a replacement worker runtime.
const nha185PythonPrelude = `
import enum
import json
import pathlib
import socket
import sys
import threading
import time
import traceback

sys.path.insert(0, str(pathlib.Path(sys.argv[1]) / "testworker" / "generated"))
import grpc
from home.integrations.v1 import host_pb2, host_pb2_grpc


class Phase(enum.Enum):
    PENDING = "pending"
    METADATA_OBSERVED = "metadata_observed"
    PROBE_REJECTED = "probe_rejected"
    FAILED = "failed"
    CANCELLED = "cancelled"


target, control_path, mode = sys.argv[2:5]
phase_deadline = int(sys.argv[5]) / 1_000_000_000
control = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
control.settimeout(max(0.001, phase_deadline - time.time()))
control.connect(control_path)
reader = control.makefile("rb")
write_lock = threading.Lock()
phase = Phase.PENDING
threads = []
calls = []
messages = []
channel = None
shared_admission = None
shared_metadata_release = threading.Event()
shared_stopping = threading.Event()


def remaining():
    value = phase_deadline - time.time()
    if value <= 0:
        raise TimeoutError("phase_deadline")
    return value


def advance(before, after):
    global phase
    if phase not in before:
        raise RuntimeError("invalid_admission_transition:" + phase.value + ":" + after.value)
    phase = after


def send(event, **fields):
    encoded = json.dumps(dict(event=event, **fields), separators=(",", ":")).encode() + b"\n"
    assert len(encoded) <= 4096
    with write_lock:
        control.sendall(encoded)


def instruction():
    line = reader.readline(4097)
    if not line or len(line) > 4096 or not line.endswith(b"\n"):
        raise RuntimeError("invalid_control_frame")
    return json.loads(line)


def expect(op):
    control.settimeout(remaining())
    value = instruction()
    if value.get("op") != op:
        raise RuntimeError("unexpected_control_instruction:" + str(value.get("op")))


def terminal_code(call):
    if not call.done():
        raise RuntimeError("code_requested_while_live")
    return call.code().name


def live(call):
    if call.done():
        raise RuntimeError("first_stream_ended:" + terminal_code(call))


def waiter(call):
    done = threading.Event()
    result = []
    errors = []

    def receive_metadata():
        try:
            result.append(tuple(call.initial_metadata() or ()))
        except BaseException as error:
            errors.append(error)
        finally:
            done.set()

    thread = threading.Thread(target=receive_metadata, daemon=False)
    threads.append(thread)
    thread.start()
    send("metadata_waiting")
    return done, result, errors, thread


def metadata(call, pending):
    done, result, errors, thread = pending
    if not done.wait(remaining()):
        raise TimeoutError("first_headers_missing")
    if errors:
        raise errors[0]
    if len(result) != 1:
        raise RuntimeError("metadata_result_missing")
    thread.join(remaining())
    if thread.is_alive():
        raise RuntimeError("metadata_waiter_not_joined")
    advance((Phase.PENDING,), Phase.METADATA_OBSERVED)


def ended(call, terminal):
    if not call.done() and not terminal.wait(remaining()):
        raise TimeoutError("terminal_status_missing")
    return terminal_code(call)


`

const nha185Python = nha185PythonPrelude + `

try:
    send("connected")
    channel = grpc.insecure_channel(target, options=[
        ("grpc.max_receive_message_length", 1 << 20),
        ("grpc.max_send_message_length", 1 << 20),
    ])
    grpc.channel_ready_future(channel).result(timeout=remaining())
    stub = host_pb2_grpc.HostServiceStub(channel)
    token = "wrong-local-token" if mode == "bad_auth" else "nha185-local-token"
    first_metadata = (("authorization", "Bearer " + token), ("x-nha185-call", "first"))
    request = host_pb2.CommandsRequest()
    if mode == "large_frame":
        request.ParseFromString(b"\x0a\x80\x10" + b"x" * 2048)
    first = stub.Commands(request, metadata=first_metadata)
    calls.append(first)
    terminal = threading.Event()
    first.add_done_callback(lambda unused: terminal.set())
    send("first_created")

    if mode in ("old", "cancel_probe"):
        expect("probe")
        probe = stub.Commands(host_pb2.CommandsRequest(), metadata=(
            ("authorization", "Bearer nha185-local-token"), ("x-nha185-call", "probe")), timeout=2)
        calls.append(probe)
        if mode == "cancel_probe":
            probe_done = threading.Event()
            probe_result = []

            def receive_probe():
                try:
                    next(probe)
                    probe_result.append(RuntimeError("unexpected_probe_message"))
                except grpc.RpcError:
                    probe_result.append(terminal_code(probe))
                except BaseException as error:
                    probe_result.append(error)
                finally:
                    probe_done.set()

            probe_thread = threading.Thread(target=receive_probe, daemon=False)
            threads.append(probe_thread)
            probe_thread.start()
            send("probe_started")
            expect("cancel_probe")
            probe.cancel()
            if not probe_done.wait(remaining()):
                raise TimeoutError("cancelled_probe_not_joined")
            if len(probe_result) != 1:
                raise RuntimeError("cancelled_probe_result_missing")
            if isinstance(probe_result[0], BaseException):
                raise probe_result[0]
            probe_code = probe_result[0]
        else:
            try:
                next(probe)
                raise AssertionError("unexpected_probe_message")
            except grpc.RpcError:
                probe_code = terminal_code(probe)
        first_code = ended(first, terminal)
        advance((Phase.PENDING,), Phase.FAILED)
        send("old_result", first_code=first_code, probe_code=probe_code)
    else:
        pending = waiter(first)
        if mode == "assertion":
            raise AssertionError("controlled_python_assertion")
        if mode == "cancel_pending":
            expect("cancel")
            first.cancel()
            metadata(first, pending)
            advance((Phase.METADATA_OBSERVED,), Phase.CANCELLED)
            send("cancelled", code=ended(first, terminal))
        elif mode in ("bad_auth", "no_capability", "large_frame"):
            metadata(first, pending)
            advance((Phase.METADATA_OBSERVED,), Phase.FAILED)
            send("refused", code=ended(first, terminal), size=request.ByteSize())
        elif mode == "delayed_refusal":
            metadata(first, pending)
            live(first)
            send("headers")
            code = ended(first, terminal)
            advance((Phase.METADATA_OBSERVED,), Phase.FAILED)
            send("refused", code=code)
        elif mode in ("headers", "admission"):
            metadata(first, pending)
            live(first)

            def receive_frames():
                try:
                    for frame in first:
                        messages.append(frame)
                except grpc.RpcError:
                    pass

            receiver = threading.Thread(target=receive_frames, daemon=False)
            threads.append(receiver)
            receiver.start()
            send("headers")
            if mode == "admission":
                probe = stub.Commands(host_pb2.CommandsRequest(), metadata=(
                    ("authorization", "Bearer nha185-local-token"), ("x-nha185-call", "probe")), timeout=2)
                calls.append(probe)
                try:
                    next(probe)
                    raise AssertionError("unexpected_probe_message")
                except grpc.RpcError:
                    probe_code = terminal_code(probe)
                if probe_code != "ALREADY_EXISTS":
                    raise RuntimeError("unexpected_probe_status:" + probe_code)
                live(first)
                advance((Phase.METADATA_OBSERVED,), Phase.PROBE_REJECTED)
                send("ready", probe_code=probe_code)
            expect("cancel")
            first.cancel()
            advance((Phase.METADATA_OBSERVED, Phase.PROBE_REJECTED), Phase.CANCELLED)
            send("cancelled", code=ended(first, terminal))
        else:
            raise RuntimeError("unknown_probe_mode:" + mode)
except BaseException as error:
    phase = Phase.FAILED
    traceback.print_exc(file=sys.stderr)
    try:
        send("failure", reason=type(error).__name__ + ":" + str(error))
    except OSError:
        pass
` + nha185PythonCleanup

const nha185PythonCleanup = `
finally:
    # One absolute stop deadline is supplied by the Go owner. The fallback is
    # clipped by the original phase + stop budgets if control itself broke.
    stop_deadline = min(time.time() + 5, phase_deadline + 5)
    cleanup_error = ""
    try:
        control.settimeout(max(0.001, stop_deadline - time.time()))
        stop = instruction()
        if stop.get("op") != "stop":
            raise RuntimeError("expected_stop_instruction")
        stop_deadline = min(stop_deadline, int(stop["deadline_ns"]) / 1_000_000_000)
    except (OSError, ValueError, KeyError, RuntimeError):
        # A broken control must still cancel and join every already-created RPC.
        pass
    if shared_admission is not None:
        shared_stopping.set()
        shared_admission.cancel()
        shared_metadata_release.set()
        try:
            shared_admission.close(time.monotonic() + max(0, stop_deadline - time.time()))
        except BaseException as error:
            cleanup_error = "shared_helper_cleanup:" + type(error).__name__ + ":" + str(error)
        with shared_admission._lock:
            threads.extend(shared_admission._threads)
        phase = shared_admission.phase
    for call in calls:
        call.cancel()
    if channel is not None:
        channel.close()
    for thread in threads:
        thread.join(max(0, stop_deadline - time.time()))
    joined = all(not thread.is_alive() for thread in threads)
    if not joined:
        cleanup_error = "thread_join_deadline"
    calls_ended = all(call.done() for call in calls)
    reader.close()
    control.close()
    print(json.dumps(dict(event="cleaned", threads_joined=joined, phase=phase.value,
                          calls_ended=calls_ended, messages=len(messages), error=cleanup_error)), flush=True)
    if not joined or not calls_ended:
        sys.exit(2)
`

// This driver imports the real shared helper. The proxy only labels actual RPCs
// and controls the metadata boundary; it never implements admission itself.
const nha185SharedPython = nha185PythonPrelude + `
sys.path.insert(0, str(pathlib.Path(sys.argv[1]) / "testworker"))
import fake_worker as worker

metadata_started = threading.Event()
open_done = threading.Event()
terminals = []


class ObservedCall:
    def __init__(self, call):
        self.call = call

    def __getattr__(self, name):
        return getattr(self.call, name)

    def __iter__(self):
        return iter(self.call)

    def __next__(self):
        return next(self.call)

    def initial_metadata(self):
        send("metadata_waiting")
        metadata_started.set()
        if mode in ("old", "cancel_probe"):
            if not shared_metadata_release.wait(remaining()):
                raise TimeoutError("negative_control_not_released")
            # Deliberately bypass ONLY the metadata barrier in the real helper.
            # Go must observe the actual second owner before releasing first.
            return ()
        value = self.call.initial_metadata()
        if mode == "metadata_error":
            raise RuntimeError("controlled_metadata_failure")
        if mode == "cancel_pending" and not self.call.done():
            # Also qualify cancel after real headers but before returning them
            # to the helper. The synthetic fixture instead blocks in C-core.
            live(self.call)
            send("metadata_received")
            if not shared_metadata_release.wait(remaining()):
                raise TimeoutError("metadata_return_not_released")
        if mode in ("admission", "admission_conformance", "delayed_refusal"):
            send("headers")
        return value


class ObservedStub:
    def __init__(self, stub, request_factory):
        self.stub, self.request_factory = stub, request_factory
        self.payloads = []

    def Commands(self, request, **kwargs):
        first = not calls
        # The real helper constructs a fresh empty request for both calls.
        # Fault injection must preserve that first/probe equivalence too.
        assert request.ByteSize() == 0
        request = self.request_factory()
        payload = request.SerializeToString()
        if self.payloads:
            assert payload == self.payloads[0]
        self.payloads.append(bytes(payload))
        if first:
            assert "timeout" not in kwargs
        else:
            assert kwargs["timeout"] == (5 if mode == "admission_conformance" else 2)
        identity = "first" if first else "probe"
        kwargs["metadata"] = tuple(kwargs["metadata"]) + (("x-nha185-call", identity),)
        call = self.stub.Commands(request, **kwargs)
        calls.append(call)
        terminal = threading.Event()
        call.add_done_callback(lambda unused: terminal.set())
        terminals.append(terminal)
        if first:
            send("first_created")
        elif mode == "cancel_probe":
            send("probe_started")
        return ObservedCall(call)


def open_shared():
    try:
        first = shared_admission.open()
        if mode not in ("admission", "admission_conformance"):
            raise AssertionError("unexpected_shared_admission")
        live(first)

        def receive_frames():
            try:
                for frame in first:
                    messages.append(frame)
            except grpc.RpcError:
                pass

        receiver = threading.Thread(target=receive_frames, daemon=False)
        threads.append(receiver)
        receiver.start()
        send("ready", probe_code=terminal_code(calls[1]))
    except BaseException as error:
        if shared_stopping.is_set():
            return
        try:
            if mode in ("old", "cancel_probe"):
                if not isinstance(error, worker._CommandStreamError):
                    raise error
                send("old_result", first_code=ended(calls[0], terminals[0]),
                     probe_code=ended(calls[1], terminals[1]))
            elif mode == "cancel_pending":
                if not isinstance(error, worker._CommandStreamError):
                    raise error
                send("cancelled", code=ended(calls[0], terminals[0]))
            elif mode in ("bad_auth", "no_capability", "large_frame", "delayed_refusal"):
                if not isinstance(error, worker._CommandStreamError) or error.code is None:
                    raise error
                diagnostic = {}
                if mode == "large_frame" and error.code is not grpc.StatusCode.RESOURCE_EXHAUSTED:
                    diagnostic["reason"] = error.reason.value
                    for field, call in zip(("first_code", "probe_code"), calls):
                        if call.done():
                            diagnostic[field] = terminal_code(call)
                send("refused", code=error.code.name, size=len(stub.payloads[0]), **diagnostic)
            else:
                raise error
        except BaseException as failure:
            send("failure", reason=type(failure).__name__ + ":" + str(failure))
    finally:
        open_done.set()


try:
    send("connected")
    channel = grpc.insecure_channel(target, options=[
        ("grpc.max_receive_message_length", 1 << 20),
        ("grpc.max_send_message_length", 1 << 20),
    ])
    grpc.channel_ready_future(channel).result(timeout=remaining())
    token = "wrong-local-token" if mode == "bad_auth" else "nha185-local-token"
    def request_factory():
        request = host_pb2.CommandsRequest()
        if mode == "large_frame":
            request.ParseFromString(b"\x0a\x80\x10" + b"x" * 2048)
        return request

    stub = ObservedStub(host_pb2_grpc.HostServiceStub(channel), request_factory)
    shared_admission = worker._CommandStream(stub, (("authorization", "Bearer " + token),),
        5 if mode == "admission_conformance" else 2,
        deadline=time.monotonic() + remaining())
    opener = threading.Thread(target=open_shared, daemon=False)
    threads.append(opener)
    opener.start()
    if mode in ("old", "cancel_probe"):
        expect("probe")
        shared_metadata_release.set()
        if mode == "cancel_probe":
            expect("cancel_probe")
            calls[1].cancel()
    elif mode == "cancel_pending":
        expect("cancel")
        shared_admission.cancel()
        shared_metadata_release.set()
    elif mode == "assertion":
        if not metadata_started.wait(remaining()):
            raise TimeoutError("shared_metadata_waiter_not_started")
        raise AssertionError("controlled_python_assertion")
    if not open_done.wait(remaining()):
        raise TimeoutError("shared_helper_not_joined")
    if mode in ("admission", "admission_conformance") and shared_admission.phase is worker._CommandPhase.ADMITTED:
        expect("cancel")
        shared_admission.cancel()
        send("cancelled", code=ended(calls[0], terminals[0]))
except BaseException as error:
    phase = Phase.FAILED
    traceback.print_exc(file=sys.stderr)
    try:
        send("failure", reason=type(error).__name__ + ":" + str(error))
    except OSError:
        pass
` + nha185PythonCleanup
