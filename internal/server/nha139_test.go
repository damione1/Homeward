package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"golang.org/x/net/http2"
)

type nha139Authenticator struct {
	zero atomic.Bool
}

func (a *nha139Authenticator) Authenticate(_ context.Context, request *http.Request) (domain.ActorID, error) {
	if request.Header.Get("Authorization") != "Bearer fixture" {
		return domain.ActorID{}, errors.New("test credential denied")
	}
	if a.zero.Load() {
		return domain.ActorID{}, nil
	}
	return domain.ParseActorID("nha139-test-actor")
}

type nha139Authorizer struct{ deny atomic.Bool }

func (a *nha139Authorizer) Authorize(_ context.Context, _ domain.ActorID, _ RouteKey) error {
	if a.deny.Load() {
		return errors.New("test permission denied")
	}
	return nil
}

type nha139Auditor struct {
	mu     sync.Mutex
	fail   atomic.Bool
	events []AuditEvent
}

func (a *nha139Auditor) Record(_ context.Context, event AuditEvent) error {
	if a.fail.Load() {
		return errors.New("private audit failure")
	}
	a.mu.Lock()
	a.events = append(a.events, event)
	a.mu.Unlock()
	return nil
}
func (a *nha139Auditor) snapshot() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.events...)
}

type nha139StateFixture struct {
	streamEnded chan struct{}
	streamFail  *atomic.Bool
	delay       *atomic.Bool
	openCalls   *atomic.Int32
}

func (s nha139StateFixture) OpenView(ctx context.Context, _ *connect.Request[statev1.OpenViewRequest]) (*connect.Response[statev1.OpenViewResponse], error) {
	s.openCalls.Add(1)
	if _, ok := ActorFromContext(ctx); !ok {
		return nil, errors.New("transport did not admit actor")
	}
	if s.delay.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return connect.NewResponse(&statev1.OpenViewResponse{}), nil
}
func (nha139StateFixture) ReadPage(context.Context, *connect.Request[statev1.ReadPageRequest]) (*connect.Response[statev1.ReadPageResponse], error) {
	return connect.NewResponse(&statev1.ReadPageResponse{}), nil
}
func (s nha139StateFixture) WatchView(ctx context.Context, _ *connect.Request[statev1.WatchViewRequest], stream *connect.ServerStream[statev1.WatchViewResponse]) error {
	defer func() { s.streamEnded <- struct{}{} }()
	if _, ok := ActorFromContext(ctx); !ok {
		return errors.New("transport did not admit stream actor")
	}
	if err := stream.Send(&statev1.WatchViewResponse{StreamSequence: 1}); err != nil {
		return err
	}
	if s.streamFail.Load() {
		return connect.NewError(connect.CodePermissionDenied, errors.New("fixture stream denied"))
	}
	<-ctx.Done()
	return ctx.Err()
}

type nha139Generation struct{}

func (nha139Generation) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

func nha139Config(t *testing.T, port int, public string) config.Config {
	t.Helper()
	yaml := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npublic:\n  url: 'http://127.0.0.1:8787'\n  max_request_bytes: 128\n  max_concurrent_requests: 1\n  request_timeout: 80ms\n  stream_timeout: 2s\n  shutdown_timeout: 1s\n%s"+
		"postgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n",
		shortPrivateDataDir(t), public, port)
	value, err := config.ParseWithEnv([]byte(yaml), func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nha139DirectClient() (*http.Client, *http2.Transport) {
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}, transport
}

// Observe the actual compressed wire body before the common HTTP transport.
// Both requests in a fixture must fit under that cap, so a ResourceExhausted
// result on the second can only come from the decoded Connect message limit.
type nha139BodyMeter struct {
	next     http.RoundTripper
	max      int
	requests atomic.Int32
}

func (m *nha139BodyMeter) RoundTrip(request *http.Request) (*http.Response, error) {
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	_ = request.Body.Close()
	if len(data) >= m.max {
		return nil, fmt.Errorf("compressed wire body reached HTTP cap: %d", len(data))
	}
	copyOfRequest := request.Clone(request.Context())
	copyOfRequest.Body = io.NopCloser(bytes.NewReader(data))
	copyOfRequest.ContentLength = int64(len(data))
	m.requests.Add(1)
	return m.next.RoundTrip(copyOfRequest)
}

func nha139Raw(t *testing.T, client *http.Client, method, url, token, body string) (int, string, int) {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", token)
	}
	request.Header.Set("X-Secret", "private-audit-canary")
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	request.Header.Set("X-Operator", "true")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	output, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(output), response.ProtoMajor
}

func nha139WaitFree(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		status, _, _ := nha139Raw(t, client, http.MethodGet, base+"/mcp/anonymous", "", "")
		if status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport did not release request slot: %d", status)
		}
		time.Sleep(time.Millisecond)
	}
}

func nha139Stream(t *testing.T, client *http.Client, base string, grpc bool, ended <-chan struct{}) {
	t.Helper()
	var opts []connect.ClientOption
	if grpc {
		opts = append(opts, connect.WithGRPC())
	}
	state := statev1connect.NewStateServiceClient(client, base, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	request := connect.NewRequest(&statev1.WatchViewRequest{})
	request.Header().Set("Authorization", "Bearer fixture")
	stream, err := state.WatchView(ctx, request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().GetStreamSequence() != 1 {
		cancel()
		t.Fatalf("first stream frame grpc=%t: %v", grpc, stream.Err())
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatalf("stream did not cancel grpc=%t", grpc)
	}
	nha139WaitFree(t, client, base)
}

func TestNHA139RealRunSharedTransportLimitsTLSAndShutdown(t *testing.T) {
	cluster := testpg.Start(t)
	settings := nha139Config(t, cluster.Port, "")
	authn := &nha139Authenticator{}
	authz := &nha139Authorizer{}
	audit := &nha139Auditor{}
	plan, err := NewPublicPlan(authn, authz, audit)
	if err != nil {
		t.Fatal(err)
	}
	streamEnded := make(chan struct{}, 4)
	streamFail := new(atomic.Bool)
	delayRPC := new(atomic.Bool)
	openCalls := new(atomic.Int32)
	slowEnded := make(chan struct{}, 2)
	windowEnded := make(chan struct{}, 1)
	windowBytes := new(atomic.Int64)
	windowBlocked := new(atomic.Bool)
	holdEntered := make(chan struct{}, 1)
	holdRelease := make(chan struct{})
	if err := plan.RegisterConnect(
		func(opts ...connect.HandlerOption) (string, http.Handler) {
			return statev1connect.NewStateServiceHandler(
				nha139StateFixture{streamEnded: streamEnded, streamFail: streamFail, delay: delayRPC, openCalls: openCalls}, opts...,
			)
		},
		ConnectProcedure{Path: statev1connect.StateServiceOpenViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportOptionalTLS},
		ConnectProcedure{Path: statev1connect.StateServiceWatchViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowServerStream, Transport: RouteTransportOptionalTLS},
	); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		kind    RouteKind
		method  RouteMethod
		path    string
		access  RouteAccess
		flow    RouteFlow
		handler http.Handler
	}{
		{RouteKindMCP, RouteMethodGet, "/mcp/anonymous", RouteAccessAnonymous, RouteFlowUnary, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "anonymous") })},
		{RouteKindMCP, RouteMethodPost, "/mcp/test", RouteAccessProtected, RouteFlowUnary, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := ActorFromContext(r.Context()); !ok {
				t.Error("MCP missing actor")
			}
			io.WriteString(w, "mcp")
		})},
		{RouteKindMedia, RouteMethodGet, "/media/test", RouteAccessProtected, RouteFlowUnary, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := ActorFromContext(r.Context()); !ok {
				t.Error("media missing actor")
			}
			io.WriteString(w, "media")
		})},
		{RouteKindMCP, RouteMethodGet, "/mcp/wait", RouteAccessProtected, RouteFlowUnary, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })},
		{RouteKindMCP, RouteMethodGet, "/mcp/hold", RouteAccessProtected, RouteFlowServerStream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			holdEntered <- struct{}{}
			select {
			case <-holdRelease:
				w.WriteHeader(http.StatusNoContent)
			case <-r.Context().Done():
			}
		})},
		{RouteKindMedia, RouteMethodGet, "/media/slow", RouteAccessProtected, RouteFlowServerStream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { slowEnded <- struct{}{} }()
			w.Header().Set("Content-Type", "application/octet-stream")
			chunk := make([]byte, 32*1024)
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				if r.Context().Err() != nil {
					return
				}
			}
		})},
		{RouteKindMedia, RouteMethodGet, "/media/window", RouteAccessProtected, RouteFlowServerStream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { windowEnded <- struct{}{} }()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			chunk := make([]byte, 32*1024)
			for {
				writing := time.Now()
				n, err := w.Write(chunk)
				windowBytes.Add(int64(n))
				if err != nil && time.Since(writing) >= 500*time.Millisecond && r.Context().Err() != nil {
					windowBlocked.Store(true)
				}
				if err != nil || r.Context().Err() != nil {
					return
				}
			}
		})},
	} {
		if err := plan.Register(item.kind, item.method, item.path, item.access, item.flow, RouteTransportOptionalTLS, item.handler); err != nil {
			t.Fatal(err)
		}
	}
	var listeners atomic.Int32
	var lc net.ListenConfig
	listen := func(ctx context.Context, network, address string) (net.Listener, error) {
		listeners.Add(1)
		return lc.Listen(ctx, network, address)
	}
	serviceReady := make(chan *integrations.DurableCommandService, 1)
	factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan, Configure: func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
			service, err := integrations.NewDurableCommandService(ctx, host, nha139Generation{}, store)
			if err == nil {
				serviceReady <- service
			}
			return err
		}}, nil
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	ready := make(chan net.Addr, 1)
	runDone := make(chan error, 1)
	go func() {
		runDone <- RunWithFactory(runCtx, settings, listen, func(address net.Addr) { ready <- address }, factory)
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-runDone:
		t.Fatalf("Run failed before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not start public listener")
	}
	service := <-serviceReady
	if listeners.Load() != 1 {
		t.Fatalf("public listener count=%d", listeners.Load())
	}
	if err := plan.Register(RouteKindMCP, RouteMethodGet, "/mcp/late", RouteAccessAnonymous, RouteFlowUnary, RouteTransportOptionalTLS, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); !errors.Is(err, ErrPublicPlanFrozen) {
		t.Fatalf("route plan mutable after serving: %v", err)
	}
	base := "http://" + address.String()
	direct, transport := nha139DirectClient()
	defer transport.CloseIdleConnections()
	upgradeClient := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
	upgrade, err := http.NewRequest(http.MethodPost, base+"/mcp/test", strings.NewReader(strings.Repeat("x", 512)))
	if err != nil {
		t.Fatal(err)
	}
	upgrade.Header.Set("Upgrade", "h2c")
	upgrade.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	upgrade.Header.Set("HTTP2-Settings", "AAEAAQAA")
	upgradeResponse, err := upgradeClient.Do(upgrade)
	if err != nil {
		t.Fatal(err)
	}
	upgradeResponse.Body.Close()
	if upgradeResponse.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("h2c upgrade diagnostic: %d", upgradeResponse.StatusCode)
	}
	for _, item := range []struct {
		method     string
		path       string
		token      string
		body       string
		wantStatus int
		wantBody   string
	}{
		{http.MethodGet, "/mcp/anonymous", "", "", 200, "anonymous"},
		{http.MethodPost, "/mcp/test", "Bearer fixture", "ok", 200, "mcp"},
		{http.MethodGet, "/media/test", "Bearer fixture", "", 200, "media"},
		{http.MethodGet, "/home.auth.v1.OperatorAccountService/BootstrapAdmin", "Bearer fixture", "", 404, ""},
	} {
		status, body, major := nha139Raw(t, direct, item.method, base+item.path, item.token, item.body)
		if status != item.wantStatus || !strings.Contains(body, item.wantBody) || major != 2 {
			t.Fatalf("%s: status=%d body=%q HTTP/%d", item.path, status, body, major)
		}
	}
	state := statev1connect.NewStateServiceClient(direct, base)
	grpc := statev1connect.NewStateServiceClient(direct, base, connect.WithGRPC())
	for _, client := range []statev1connect.StateServiceClient{state, grpc} {
		request := connect.NewRequest(&statev1.OpenViewRequest{})
		request.Header().Set("Authorization", "Bearer fixture")
		if _, err := client.OpenView(context.Background(), request); err != nil {
			t.Fatalf("generated unary client: %v", err)
		}
		request = connect.NewRequest(&statev1.OpenViewRequest{})
		if _, err := client.OpenView(context.Background(), request); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("missing identity: %v", err)
		}
	}
	streamFail.Store(true)
	for _, client := range []statev1connect.StateServiceClient{state, grpc} {
		request := connect.NewRequest(&statev1.WatchViewRequest{})
		request.Header().Set("Authorization", "Bearer fixture")
		stream, err := client.WatchView(context.Background(), request)
		if err != nil {
			t.Fatalf("stream error setup: %v", err)
		}
		if !stream.Receive() || stream.Msg().GetStreamSequence() != 1 || stream.Receive() ||
			connect.CodeOf(stream.Err()) != connect.CodePermissionDenied {
			t.Fatalf("stream protocol error/trailer: %v", stream.Err())
		}
		select {
		case <-streamEnded:
		case <-time.After(time.Second):
			t.Fatal("stream error handler did not return")
		}
	}
	streamFail.Store(false)
	authz.deny.Store(true)
	denied := connect.NewRequest(&statev1.OpenViewRequest{})
	denied.Header().Set("Authorization", "Bearer fixture")
	if _, err := state.OpenView(context.Background(), denied); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("authorization denial: %v", err)
	}
	authz.deny.Store(false)
	authn.zero.Store(true)
	zero := connect.NewRequest(&statev1.OpenViewRequest{})
	zero.Header().Set("Authorization", "Bearer fixture")
	if _, err := state.OpenView(context.Background(), zero); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("zero actor admitted: %v", err)
	}
	authn.zero.Store(false)
	audit.fail.Store(true)
	noAudit := connect.NewRequest(&statev1.OpenViewRequest{})
	noAudit.Header().Set("Authorization", "Bearer fixture")
	if _, err := state.OpenView(context.Background(), noAudit); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("audit failure admitted: %v", err)
	}
	audit.fail.Store(false)
	for _, client := range []statev1connect.StateServiceClient{state, grpc} {
		oversized := connect.NewRequest(&statev1.OpenViewRequest{Filter: &statev1.ViewFilter{EntityIds: []string{strings.Repeat("x", 512)}}})
		oversized.Header().Set("Authorization", "Bearer fixture")
		if _, err := client.OpenView(context.Background(), oversized); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Fatalf("generated oversized RPC: %v", err)
		}
	}
	for _, protocol := range []struct {
		grpc bool
	}{
		{false}, {true},
	} {
		bounded := &nha139BodyMeter{next: transport, max: 128}
		client := &http.Client{Transport: bounded, Timeout: 3 * time.Second}
		opts := []connect.ClientOption{connect.WithSendGzip()}
		if protocol.grpc {
			opts = append(opts, connect.WithGRPC())
		}
		compressed := statev1connect.NewStateServiceClient(client, base, opts...)
		before := openCalls.Load()
		under := connect.NewRequest(&statev1.OpenViewRequest{Filter: &statev1.ViewFilter{EntityIds: []string{strings.Repeat("x", 20)}}})
		under.Header().Set("Authorization", "Bearer fixture")
		if _, err := compressed.OpenView(context.Background(), under); err != nil {
			t.Fatalf("compressed under bound grpc=%t: %v", protocol.grpc, err)
		}
		if openCalls.Load() != before+1 {
			t.Fatalf("compressed under bound did not invoke handler grpc=%t", protocol.grpc)
		}
		before = openCalls.Load()
		over := connect.NewRequest(&statev1.OpenViewRequest{Filter: &statev1.ViewFilter{EntityIds: []string{strings.Repeat("x", 512)}}})
		over.Header().Set("Authorization", "Bearer fixture")
		if _, err := compressed.OpenView(context.Background(), over); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Fatalf("decompressed over bound grpc=%t: %v", protocol.grpc, err)
		}
		if openCalls.Load() != before || bounded.requests.Load() != 2 {
			t.Fatalf("decoded size bypass grpc=%t: calls=%d requests=%d", protocol.grpc, openCalls.Load(), bounded.requests.Load())
		}
	}
	delayRPC.Store(true)
	for _, client := range []statev1connect.StateServiceClient{state, grpc} {
		request := connect.NewRequest(&statev1.OpenViewRequest{})
		request.Header().Set("Authorization", "Bearer fixture")
		started := time.Now()
		if _, err := client.OpenView(context.Background(), request); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
			t.Fatalf("generated RPC deadline: %v", err)
		}
		if time.Since(started) > time.Second {
			t.Fatalf("generated RPC deadline exceeded bounded write grace: %s", time.Since(started))
		}
	}
	delayRPC.Store(false)
	status, body, _ := nha139Raw(t, direct, http.MethodPost, base+"/mcp/test", "Bearer fixture", strings.Repeat("x", 512))
	if status != http.StatusTooManyRequests || !strings.Contains(body, "resource_exhausted") {
		t.Fatalf("oversized MCP: status=%d body=%s", status, body)
	}
	started := time.Now()
	status, body, _ = nha139Raw(t, direct, http.MethodGet, base+"/mcp/wait", "Bearer fixture", "")
	if status != http.StatusGatewayTimeout || !strings.Contains(body, "deadline_exceeded") || time.Since(started) > time.Second {
		t.Fatalf("deadline: status=%d elapsed=%s body=%s", status, time.Since(started), body)
	}
	holdDone := make(chan struct{}, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, base+"/mcp/hold", nil)
		request.Header.Set("Authorization", "Bearer fixture")
		response, err := direct.Do(request)
		if err == nil {
			response.Body.Close()
		}
		holdDone <- struct{}{}
	}()
	select {
	case <-holdEntered:
	case <-time.After(time.Second):
		t.Fatal("hold route did not start")
	}
	status, body, _ = nha139Raw(t, direct, http.MethodGet, base+"/media/test", "Bearer fixture", "")
	if status != http.StatusTooManyRequests || !strings.Contains(body, "resource_exhausted") {
		t.Fatalf("concurrency: status=%d body=%s", status, body)
	}
	close(holdRelease)
	<-holdDone
	nha139WaitFree(t, direct, base)
	windowClient := &http.Client{Transport: transport}
	windowRequest, err := http.NewRequest(http.MethodGet, base+"/media/window", nil)
	if err != nil {
		t.Fatal(err)
	}
	windowRequest.Header.Set("Authorization", "Bearer fixture")
	windowResponse, err := windowClient.Do(windowRequest)
	if err != nil {
		t.Fatal(err)
	}
	if windowResponse.StatusCode != http.StatusOK {
		windowResponse.Body.Close()
		t.Fatalf("window stream status: %d", windowResponse.StatusCode)
	}
	select {
	case <-windowEnded:
	case <-time.After(3 * time.Second):
		windowResponse.Body.Close()
		t.Fatal("unread HTTP/2 flow-control window retained writer after stream deadline")
	}
	windowResponse.Body.Close()
	// x/net/http2 v0.43.0 Transport advertises 4 MiB per stream and
	// buffers that amount without a reader (transport.go:45-48).
	if windowBytes.Load() < 4<<20 || !windowBlocked.Load() {
		t.Fatalf("window stream did not prove a blocked Write released by deadline: %d bytes, blocked=%t", windowBytes.Load(), windowBlocked.Load())
	}
	nha139WaitFree(t, direct, base)
	for _, grpcMode := range []bool{false, true} {
		nha139Stream(t, direct, base, grpcMode, streamEnded)
	}
	slowCtx, cancelSlow := context.WithCancel(context.Background())
	slowReq, _ := http.NewRequestWithContext(slowCtx, http.MethodGet, base+"/media/slow", nil)
	slowReq.Header.Set("Authorization", "Bearer fixture")
	slowResponse, err := direct.Do(slowReq)
	if err != nil {
		t.Fatal(err)
	}
	if slowResponse.ProtoMajor != 2 {
		t.Fatalf("slow route protocol: %s", slowResponse.Proto)
	}
	time.Sleep(20 * time.Millisecond)
	cancelSlow()
	slowResponse.Body.Close()
	select {
	case <-slowEnded:
	case <-time.After(time.Second):
		t.Fatal("unread slow client handler remained active")
	}
	nha139WaitFree(t, direct, base)

	target, _ := url.Parse(base)
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = transport
	proxy := httptest.NewUnstartedServer(reverse)
	proxy.EnableHTTP2 = true
	proxy.StartTLS()
	defer proxy.Close()
	secure := proxy.Client()
	secure.Timeout = 3 * time.Second
	status, body, major := nha139Raw(t, secure, http.MethodGet, proxy.URL+"/media/test", "Bearer fixture", "")
	if status != 200 || body != "media" || major != 2 {
		t.Fatalf("TLS media HTTP/%d status=%d body=%s", major, status, body)
	}
	status, _, major = nha139Raw(t, secure, http.MethodGet, proxy.URL+"/home.auth.v1.OperatorAccountService/BootstrapAdmin", "Bearer fixture", "")
	if status != http.StatusNotFound || major != 2 {
		t.Fatalf("proxy/client headers granted operator authority: HTTP/%d status=%d", major, status)
	}
	for _, client := range []statev1connect.StateServiceClient{
		statev1connect.NewStateServiceClient(secure, proxy.URL),
		statev1connect.NewStateServiceClient(secure, proxy.URL, connect.WithGRPC()),
	} {
		request := connect.NewRequest(&statev1.OpenViewRequest{})
		request.Header().Set("Authorization", "Bearer fixture")
		if _, err := client.OpenView(context.Background(), request); err != nil {
			t.Fatalf("TLS generated RPC: %v", err)
		}
	}
	for _, grpcMode := range []bool{false, true} {
		nha139Stream(t, secure, proxy.URL, grpcMode, streamEnded)
	}
	untrusted := &http.Client{Timeout: time.Second}
	response, err := untrusted.Get(proxy.URL + "/mcp/anonymous")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())
	wrongHost := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "wrong.invalid"}, ForceAttemptHTTP2: true}, Timeout: time.Second}
	response, err = wrongHost.Get(proxy.URL + "/mcp/anonymous")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("wrong-host TLS certificate accepted")
	}

	// Keep a separate HTTP/2 connection idle after its SETTINGS exchange.
	// net/http does not own h2c's hijacked socket at Shutdown.
	idleConn, err := net.DialTimeout("tcp", address.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idleConn.Close()
	idleConn.SetDeadline(time.Now().Add(time.Second))
	if _, err := idleConn.Write(append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), []byte{0, 0, 0, 4, 0, 0, 0, 0, 0}...)); err != nil {
		t.Fatal(err)
	}
	frameHeader := make([]byte, 9)
	if _, err := io.ReadFull(idleConn, frameHeader); err != nil || frameHeader[3] != 4 {
		t.Fatalf("idle h2c SETTINGS exchange: %x %v", frameHeader, err)
	}
	ending := connect.NewRequest(&statev1.WatchViewRequest{})
	ending.Header().Set("Authorization", "Bearer fixture")
	active, err := state.WatchView(context.Background(), ending)
	if err != nil {
		t.Fatalf("shutdown stream start: %v", err)
	}
	if !active.Receive() {
		t.Fatalf("shutdown stream first frame: %v", active.Err())
	}
	cancelRun()
	select {
	case <-streamEnded:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown retained Connect stream")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run retained listener or handler goroutine")
	}
	idleConn.SetReadDeadline(time.Now().Add(time.Second))
	for {
		buffer := make([]byte, 4096)
		if _, err := idleConn.Read(buffer); err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("idle h2c connection survived Run shutdown")
			}
			break
		}
	}
	select {
	case <-service.Done():
	default:
		t.Fatal("Run returned before durable drain")
	}
	connection, err := net.DialTimeout("tcp", address.String(), 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("public listener remained open")
	}
	if listeners.Load() != 1 {
		t.Fatalf("opened %d public listeners", listeners.Load())
	}
	events := audit.snapshot()
	var anonymousAdmitted, protectedAdmitted, deniedRecorded bool
	for _, event := range events {
		if event.Route.Path() == "/mcp/anonymous" && event.Outcome == AuditOutcomeAdmitted && !event.HasActor {
			anonymousAdmitted = true
		}
		if event.Route.Path() == "/media/test" && event.Outcome == AuditOutcomeAdmitted && event.HasActor {
			actor, err := event.Actor.Value()
			protectedAdmitted = err == nil && actor == "nha139-test-actor"
		}
		if event.Route.Path() == statev1connect.StateServiceOpenViewProcedure && event.Outcome == AuditOutcomeDenied {
			deniedRecorded = true
		}
	}
	if len(events) < 8 || !anonymousAdmitted || !protectedAdmitted || !deniedRecorded ||
		strings.Contains(fmt.Sprintf("%+v", events), "private-audit-canary") {
		t.Fatalf("audit missing identity/denial or contains request secret: %d events", len(events))
	}
}

func TestNHA139PlanAdmissionAndMissingProviderCloseRoutes(t *testing.T) {
	var typedNil *nha139Authenticator
	if _, err := NewPublicPlan(typedNil, nil, nil); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("typed-nil authenticator: %v", err)
	}
	var typedNilAuthorizer *nha139Authorizer
	if _, err := NewPublicPlan(nil, typedNilAuthorizer, nil); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("typed-nil authorizer: %v", err)
	}
	var typedNilAuditor *nha139Auditor
	if _, err := NewPublicPlan(nil, nil, typedNilAuditor); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("typed-nil auditor: %v", err)
	}
	plan, err := NewPublicPlan(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handlerCalls := new(atomic.Int32)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { handlerCalls.Add(1); w.WriteHeader(200) })
	if err := plan.RegisterConnect(
		func(...connect.HandlerOption) (string, http.Handler) {
			return "/home.auth.v1.OperatorAccountService/", handler
		},
		ConnectProcedure{Path: "/home.auth.v1.OperatorAccountService/BootstrapAdmin", Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportOptionalTLS},
	); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("public operator service registration: %v", err)
	}
	if err := plan.Register(RouteKindMedia, RouteMethodGet, "/media/closed", RouteAccessProtected, RouteFlowUnary, RouteTransportOptionalTLS, handler); err != nil {
		t.Fatal(err)
	}
	if err := plan.Register(RouteKindMedia, RouteMethodGet, "/media/closed", RouteAccessProtected, RouteFlowUnary, RouteTransportOptionalTLS, handler); !errors.Is(err, ErrPublicRouteConflict) {
		t.Fatalf("route conflict: %v", err)
	}
	if err := plan.Register(RouteKindMCP, RouteMethodGet, "/mcp/anonymous", RouteAccessAnonymous, RouteFlowUnary, RouteTransportOptionalTLS, handler); err != nil {
		t.Fatal(err)
	}
	if err := (&PublicPlan{}).Register(RouteKindMCP, RouteMethodGet, "/mcp/zero", RouteAccessAnonymous, RouteFlowUnary, RouteTransportOptionalTLS, handler); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("zero plan registered route: %v", err)
	}
	if err := plan.Register(RouteKindConnect, RouteMethodPost, statev1connect.StateServiceOpenViewProcedure, RouteAccessProtected, RouteFlowUnary, RouteTransportOptionalTLS, handler); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("opaque Connect handler bypassed common read bound: %v", err)
	}
	collision, err := NewPublicPlan(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := collision.Register(RouteKindMCP, RouteMethodPost, statev1connect.StateServiceOpenViewProcedure, RouteAccessAnonymous, RouteFlowUnary, RouteTransportOptionalTLS, handler); err != nil {
		t.Fatal(err)
	}
	if err := collision.RegisterConnect(
		func(...connect.HandlerOption) (string, http.Handler) { return "/home.state.v1.StateService/", handler },
		ConnectProcedure{Path: statev1connect.StateServiceOpenViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportOptionalTLS},
	); !errors.Is(err, ErrPublicRouteConflict) {
		t.Fatalf("Connect procedure route collision: %v", err)
	}
	cluster := testpg.Start(t)
	settings := nha139Config(t, cluster.Port, "")
	badPrefix, err := NewPublicPlan(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := badPrefix.RegisterConnect(
		func(...connect.HandlerOption) (string, http.Handler) { return "/other.Service/", handler },
		ConnectProcedure{Path: statev1connect.StateServiceOpenViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportOptionalTLS},
	); err != nil {
		t.Fatal(err)
	}
	publicSettings, err := settings.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compilePublicPlan(badPrefix, publicSettings); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("generated service prefix mismatch: %v", err)
	}
	compiled, err := compilePublicPlan(plan, publicSettings)
	if err != nil {
		t.Fatal(err)
	}
	// A raw plan without providers still fails closed at the actual handler.
	// Run now composes its session provider, so that case is distinct below.
	missingProvider := httptest.NewServer(compiled.handler(publicSettings, newPublicAdmissions()))
	for _, route := range []string{"/media/closed", "/mcp/anonymous"} {
		status, body, major := nha139Raw(t, missingProvider.Client(), http.MethodGet, missingProvider.URL+route, "Bearer fixture", "")
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "unavailable") || major != 1 {
			t.Fatalf("missing provider %s: status=%d body=%s HTTP/%d", route, status, body, major)
		}
	}
	missingProvider.Close()
	if handlerCalls.Load() != 0 {
		t.Fatal("missing provider reached protected or anonymous handler")
	}
	runPlan, err := NewPublicPlan(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runPlan.Register(RouteKindMedia, RouteMethodGet, "/media/closed", RouteAccessProtected, RouteFlowUnary, RouteTransportOptionalTLS, handler); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, settings, lc.Listen, func(address net.Addr) { ready <- address }, func(context.Context, *storage.Store) (RuntimePlan, error) {
			return RuntimePlan{Identity: unconfiguredIdentity{}, Public: runPlan}, nil
		})
	}()
	var address net.Addr
	select {
	case address = <-ready:
		t.Fatalf("Run exposed OptionalTLS route with builtin session provider: %s", address)
	case err := <-done:
		if !errors.Is(err, ErrInvalidPublicPlan) {
			t.Fatalf("Run accepted incompatible session transport plan: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("incompatible session transport plan did not fail before ready")
	}
	direct, transport := nha139DirectClient()
	defer transport.CloseIdleConnections()
	defaultCtx, cancelDefault := context.WithCancel(context.Background())
	defer cancelDefault()
	defaultReady := make(chan net.Addr, 1)
	defaultDone := make(chan error, 1)
	go func() {
		defaultDone <- Run(defaultCtx, settings, lc.Listen, func(address net.Addr) { defaultReady <- address })
	}()
	select {
	case address = <-defaultReady:
	case err := <-defaultDone:
		t.Fatalf("default Run before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("default Run did not start")
	}
	status, _, major := nha139Raw(t, direct, http.MethodGet, "http://"+address.String()+"/mcp/anonymous", "", "")
	if status != http.StatusNotFound || major != 2 {
		t.Fatalf("default Run exposed a public route: HTTP/%d status=%d", major, status)
	}
	cancelDefault()
	select {
	case err := <-defaultDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("default Run did not stop")
	}
}
