package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	"github.com/damione1/Homeward/gen/home/integrations/v1/integrationsv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/observability"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/encoding/protojson"
)

const nha056WorkerPanicCanary = "NHA056_WORKER_PANIC_SIGNED_URL_PASSWORD_CALENDAR_COORDS_CANARY"
const nha056WorkerChildCanary = "NHA056_WORKER_CHILD_STDOUT_STDERR_TOKEN_CANARY"

type nha056WorkerTasks struct {
	ctx    context.Context
	wg     sync.WaitGroup
	mu     sync.Mutex
	errors []error
}

func (g *nha056WorkerTasks) Go(task integrations.Task) error {
	if task == nil || g.ctx == nil {
		return integrations.ErrInvalidInput
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := task(g.ctx); err != nil {
			g.mu.Lock()
			g.errors = append(g.errors, err)
			g.mu.Unlock()
		}
	}()
	return nil
}

type nha056PanicHealthHost struct{ integrations.SessionHost }

func (h nha056PanicHealthHost) ReportHealth(ctx context.Context, report integrations.HealthReport) error {
	_, detail, err := report.Components()
	if err != nil {
		return err
	}
	if detail == nha056WorkerPanicCanary {
		panic(nha056WorkerPanicCanary)
	}
	return h.SessionHost.ReportHealth(ctx, report)
}

// This barrier forwards to the actual installed writer, without retaining any
// untrusted bytes. Completion proves the HTTP stack finished its real write.
type nha056DiagnosticBarrier struct {
	writer io.Writer
	done   chan struct{}
	once   sync.Once
}

func (w *nha056DiagnosticBarrier) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.once.Do(func() { close(w.done) })
	return n, err
}
func nha056WorkerClient(socket string) (integrationsv1connect.HostServiceClient, *http2.Transport) {
	transport := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, _ string, _ string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return integrationsv1connect.NewHostServiceClient(&http.Client{Transport: transport}, "http://localhost", connect.WithGRPC()), transport
}
func nha056WorkerRequest[T any](value *T, token string) *connect.Request[T] {
	request := connect.NewRequest(value)
	request.Header().Set("Authorization", "Bearer "+token)
	return request
}
func nha056WorkerJournal(t *testing.T) (*observability.Journal, *observability.CursorCodec) {
	t.Helper()
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := observability.NewJournal(epoch, cells)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := observability.NewCursorCodec(epoch)
	if err != nil {
		t.Fatal(err)
	}
	return journal, codec
}
func nha056WorkerLogs(t *testing.T, journal *observability.Journal, codec *observability.CursorCodec) *obs.QueryRuntimeLogsResponse {
	t.Helper()
	snapshot, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	digest[0] = 1
	binding, err := observability.NewReadBinding(1, digest)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := codec.BeginLogs(binding, snapshot.Through())
	if err != nil {
		t.Fatal(err)
	}
	size, err := observability.NewPageSize(100)
	if err != nil {
		t.Fatal(err)
	}
	request, err := observability.NewLogPageRequest(size, cursor)
	if err != nil {
		t.Fatal(err)
	}
	page, err := journal.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := page.Wire("")
	if err != nil || contracts.AdmitObservabilityResponse(response) != nil {
		t.Fatal("worker log page not admitted", err)
	}
	return response
}

// Only the test executable is launched. Start passes only a private Unix socket
// and token pipe; the helper never resolves or contacts an IP address or device.
func TestNHA056WorkerSyntheticChild(t *testing.T) {
	socket := os.Getenv("HOMEWARD_HOST_SOCKET")
	if socket == "" {
		return
	}
	tokenPipe := os.NewFile(3, "synthetic-token")
	if tokenPipe == nil {
		t.Fatal("missing synthetic token pipe")
	}
	tokenBytes, err := io.ReadAll(io.LimitReader(tokenPipe, 65))
	_ = tokenPipe.Close()
	if err != nil || len(tokenBytes) != 64 {
		t.Fatal("invalid synthetic token pipe")
	}
	fmt.Fprintln(os.Stdout, nha056WorkerChildCanary)
	fmt.Fprintln(os.Stderr, nha056WorkerChildCanary)
	client, transport := nha056WorkerClient(socket)
	defer transport.CloseIdleConnections()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	digest, err := hex.DecodeString(schemaDigestV13Hex)
	if err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	_, err = client.Handshake(callCtx, nha056WorkerRequest(&wire.HandshakeRequest{HostMajor: 1, HostMinor: 3, DriverMajor: 1, DriverMinor: 3, SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_HEALTH}}, string(tokenBytes)))
	cancel()
	if err != nil {
		t.Fatal("synthetic handshake failed")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, time.Second)
			_, err := client.Heartbeat(callCtx, nha056WorkerRequest(&wire.HeartbeatRequest{}, string(tokenBytes)))
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func TestNHA056WorkerRealHTTPDiagnosticAndChildOutputAreSafe(t *testing.T) {
	journal, codec := nha056WorkerJournal(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewConfig("nha056-worker", executable, []string{"-test.run=^TestNHA056WorkerSyntheticChild$"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	generic, err := NewPilotWithJournal(config, journal)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := generic.(*pilot)
	if !ok || p == nil {
		t.Fatal("wrapper did not retain the actual worker")
	}
	host, _, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := config.Instance()
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tasks := &nha056WorkerTasks{ctx: ctx}
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), stopTimeout+time.Second)
		defer stop()
		if err := p.Stop(stopCtx); err != nil {
			t.Error("synthetic worker failed to join", err)
		}
		done := make(chan struct{})
		go func() { tasks.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-stopCtx.Done():
			t.Error("synthetic worker tasks did not join")
		}
		tasks.mu.Lock()
		taskErrors := append([]error(nil), tasks.errors...)
		tasks.mu.Unlock()
		for _, err := range taskErrors {
			t.Error("synthetic worker task failed", err)
		}
		_ = host.CloseAdmissions()
	})
	if err := p.Start(ctx, nha056PanicHealthHost{SessionHost: session}, tasks); err != nil {
		t.Fatal("real synthetic worker Start failed", err)
	}
	p.mu.Lock()
	server, cmd, socket, token := p.server, p.process, p.directory+"/host.sock", p.token
	p.mu.Unlock()
	if server == nil || server.ErrorLog == nil {
		t.Fatal("actual worker HTTP server has no safe ErrorLog")
	}
	if cmd == nil || cmd.Stdout != io.Discard || cmd.Stderr != io.Discard {
		t.Fatal("actual child stdout/stderr are not discarded")
	}
	barrier := &nha056DiagnosticBarrier{writer: server.ErrorLog.Writer(), done: make(chan struct{})}
	server.ErrorLog.SetOutput(barrier)
	var fallback bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&fallback)
	defer log.SetOutput(previous)
	client, transport := nha056WorkerClient(socket)
	defer transport.CloseIdleConnections()
	callCtx, callCancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer callCancel()
	_, err = client.ReportHealth(callCtx, nha056WorkerRequest(&wire.ReportHealthRequest{Status: wire.HealthStatus_HEALTH_STATUS_READY, Detail: nha056WorkerPanicCanary}, token))
	if err == nil {
		t.Fatal("synthetic Host panic unexpectedly succeeded")
	}
	select {
	case <-barrier.done:
	case <-callCtx.Done():
		t.Fatal("actual HTTP stack did not publish its safe diagnostic")
	}
	response := nha056WorkerLogs(t, journal, codec)
	if len(response.GetPage().Rows) != 1 {
		t.Fatal("actual HTTP stack lost or duplicated its diagnostic")
	}
	event := response.GetPage().Rows[0].Event
	if event.Code != obs.LogCode_LOG_CODE_TRANSPORT_INTERNAL_DIAGNOSTIC || event.Level != obs.LogLevel_LOG_LEVEL_WARNING || event.Component != obs.LogComponent_LOG_COMPONENT_WORKER_TRANSPORT || event.Author.GetProcess() == nil || event.OccurredAt == nil {
		t.Fatal("worker safe positive diagnostic fields lost")
	}
	encoded, err := protojson.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{nha056WorkerPanicCanary, nha056WorkerChildCanary} {
		if bytes.Contains(encoded, []byte(canary)) || strings.Contains(fallback.String(), canary) {
			t.Fatal("worker diagnostic leaked an untrusted value")
		}
	}
}

func TestNHA056WorkerJournalFactoriesRejectInvalidPorts(t *testing.T) {
	journal, _ := nha056WorkerJournal(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config, err := NewConfig("nha056-worker-factory", executable, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := NewPilotSpecWithJournal(config, journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Instance(); err != nil {
		t.Fatal("journal spec lost admitted instance", err)
	}
	for _, bad := range []*observability.Journal{nil, {}} {
		if p, err := NewPilotWithJournal(config, bad); err == nil || p != nil {
			t.Fatal("worker admitted invalid journal")
		}
		if _, err := NewPilotSpecWithJournal(config, bad); err == nil {
			t.Fatal("worker spec admitted invalid journal")
		}
	}
	if p, err := NewPilotWithJournal(Config{}, journal); err == nil || p != nil {
		t.Fatal("worker admitted zero config")
	}
	if _, err := NewPilotSpecWithJournal(Config{}, journal); err == nil {
		t.Fatal("worker spec admitted zero config")
	}
}
