package integrations_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/server"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha175Policy(t *testing.T, change func(*integrations.CloudLimitsInput, *integrations.CloudStreamLimitsInput), bindings ...integrations.CloudAccountBinding) *integrations.CloudPolicy {
	t.Helper()
	httpLimits := integrations.DefaultCloudLimits().Input()
	streamLimits := integrations.DefaultCloudStreamLimits().Input()
	if change != nil {
		change(&httpLimits, &streamLimits)
	}
	finite, err := integrations.AdmitCloudLimits(httpLimits)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := integrations.AdmitCloudStreamLimits(streamLimits)
	if err != nil {
		t.Fatal(err)
	}
	p, err := integrations.NewCloudPolicyWithStreams(finite, streams, bindings, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, nha096NoJitter{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func nha175Transport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return nil, errors.New("synthetic transport refuses non-loopback")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
}
func nha175Client(t *testing.T, rig nha096Rig, admission ...integrations.CloudAttemptAdmission) *integrations.CloudClient {
	t.Helper()
	c, err := integrations.NewCloudClient(rig.session, rig.policy, nha175Transport(), admission...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func nha175Request(t *testing.T, kind integrations.CloudStreamKind, url string) integrations.CloudStreamRequest {
	t.Helper()
	if kind == integrations.CloudStreamWebSocket {
		url = "ws" + strings.TrimPrefix(url, "http")
	}
	r, err := integrations.NewCloudStreamRequest(kind, http.MethodGet, url, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func nha175Open(t *testing.T, c *integrations.CloudClient, request integrations.CloudStreamRequest) *integrations.CloudObservationStream {
	t.Helper()
	s, err := c.OpenObservationStream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}
func nha175Server(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(func() { s.CloseClientConnections(); s.Close() })
	return s
}
func nha175SSE(t *testing.T, payload string) *httptest.Server {
	return nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, payload)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
}
func nha175Closed(t *testing.T, s *integrations.CloudObservationStream) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not join")
	}
}
func nha175Bytes(t *testing.T, m integrations.CloudStreamMessage) string {
	t.Helper()
	data, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type nha175NilContext struct{ context.Context }

func TestNHA175AdmissionLimitsAndOwnedTypes(t *testing.T) {
	good := integrations.DefaultCloudStreamLimits().Input()
	for _, change := range []func(*integrations.CloudStreamLimitsInput){
		func(v *integrations.CloudStreamLimitsInput) { v.MaxActiveStreams = 0 }, func(v *integrations.CloudStreamLimitsInput) { v.MaxActiveStreams = 33 },
		func(v *integrations.CloudStreamLimitsInput) { v.MaxReceiveBytes = 0 }, func(v *integrations.CloudStreamLimitsInput) { v.MaxReceiveBytes = 4<<20 + 1 },
		func(v *integrations.CloudStreamLimitsInput) { v.MaxSSELineBytes = v.MaxReceiveBytes + 1 }, func(v *integrations.CloudStreamLimitsInput) { v.MaxWriteBytes = 0 },
		func(v *integrations.CloudStreamLimitsInput) { v.IdleTimeout = 0 }, func(v *integrations.CloudStreamLimitsInput) { v.WriteTimeout = 31 * time.Second }, func(v *integrations.CloudStreamLimitsInput) { v.CloseTimeout = 0 },
	} {
		bad := good
		change(&bad)
		if _, err := integrations.AdmitCloudStreamLimits(bad); err == nil {
			t.Fatal("invalid limits admitted")
		}
	}
	if (integrations.CloudStreamLimits{}).Valid() {
		t.Fatal("zero limits valid")
	}
	if _, err := integrations.NewCloudPolicyWithStreams(integrations.CloudLimits{}, integrations.DefaultCloudStreamLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, nha096NoJitter{}); err == nil {
		t.Fatal("zero finite limits admitted")
	}
	var nilClock *nha096Clock
	if _, err := integrations.NewCloudPolicyWithStreams(integrations.DefaultCloudLimits(), integrations.DefaultCloudStreamLimits(), nil, nilClock, integrations.RealCloudTimer{}, nha096NoJitter{}); err == nil {
		t.Fatal("typednil clock admitted")
	}
	for _, kind := range []integrations.CloudStreamKind{integrations.CloudStreamUnspecified, 99} {
		if _, err := integrations.NewCloudStreamRequest(kind, http.MethodGet, "http://127.0.0.1", nil, nil); err == nil {
			t.Fatal("unknown stream kind admitted")
		}
	}
	if _, err := integrations.NewCloudStreamRequest(integrations.CloudStreamWebSocket, http.MethodPost, "ws://127.0.0.1", nil, nil); err == nil {
		t.Fatal("WS POST admitted")
	}
	if _, err := integrations.NewCloudStreamRequest(integrations.CloudStreamSSE, http.MethodGet, "http://user:password@127.0.0.1", nil, nil); err == nil {
		t.Fatal("userinfo admitted")
	}
	if _, err := integrations.NewCloudStreamRequest(integrations.CloudStreamWebSocket, http.MethodGet, "ws://127.0.0.1", http.Header{"Upgrade": {"websocket"}}, nil); err == nil {
		t.Fatal("reserved header admitted")
	}
	if _, err := (integrations.CloudStreamMessage{}).Bytes(); err == nil {
		t.Fatal("zero message admitted")
	}
	if _, err := integrations.NewCloudWebSocketMessage(integrations.CloudStreamSSEEvent, nil); err == nil {
		t.Fatal("SSE write admitted")
	}
	if _, err := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketText, []byte{0xff}); err == nil {
		t.Fatal("invalid UTF8 text write admitted")
	}
	old := integrations.DefaultCloudLimits().Input()
	rig := nha096CloudRig(t, nil)
	client := nha175Client(t, rig)
	var nilContext *nha175NilContext
	_, nilErr := client.OpenObservationStream(nilContext, nha175Request(t, integrations.CloudStreamSSE, "http://127.0.0.1:1"))
	nha096Kind(t, nilErr, integrations.CloudInvalid)
	_, err := client.OpenObservationStream(context.Background(), nha175Request(t, integrations.CloudStreamSSE, "http://127.0.0.1:1"))
	nha096Kind(t, err, integrations.CloudStreamsDisabled)
	if !reflect.DeepEqual(old, integrations.DefaultCloudLimits().Input()) {
		t.Fatal("historical limits mutated")
	}
}

func TestNHA175SSEDeliversBeforeEOFAndPreservesData(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, nil))
	client := nha175Client(t, rig)
	header := http.Header{"X-Canary": {"synthetic-header"}}
	body := []byte("synthetic-query")
	var received atomic.Bool
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received.Store(r.Header.Get("X-Canary") == "synthetic-header" && string(raw) == "synthetic-query")
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "\ufeff: keepalive\r\nid: synthetic-id\r\nevent: changed\r\ndata: 0\r\ndata: false\r\n\r\ndata:\r\n\r\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	request, err := integrations.NewCloudStreamRequest(integrations.CloudStreamSSE, http.MethodPost, endpoint.URL, header, body)
	if err != nil {
		t.Fatal(err)
	}
	header.Set("X-Canary", "changed")
	body[0] = 'X'
	stream := nha175Open(t, client, request)
	message, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !received.Load() || nha175Bytes(t, message) != "0\nfalse" {
		t.Fatal("request copies or event data lost")
	}
	kind, err := message.Kind()
	if err != nil || kind != integrations.CloudStreamSSEEvent {
		t.Fatal("wrong message kind")
	}
	event, id, hasID, err := message.SSEFields()
	if err != nil || event != "changed" || id != "synthetic-id" || !hasID {
		t.Fatal("SSE fields lost")
	}
	copy, _ := message.Bytes()
	copy[0] = 'X'
	if nha175Bytes(t, message) != "0\nfalse" {
		t.Fatal("returned payload aliases internal bytes")
	}
	empty, err := stream.Receive(context.Background())
	if err != nil || nha175Bytes(t, empty) != "" {
		t.Fatal("present empty SSE data lost")
	}
	event, id, hasID, err = empty.SSEFields()
	if err != nil || event != "message" || id != "synthetic-id" || !hasID {
		t.Fatal("SSE ID inheritance lost")
	}
	for _, v := range []any{request, stream, message} {
		data, _ := json.Marshal(v)
		for _, text := range []string{fmt.Sprint(v), fmt.Sprintf("%#v", v), string(data)} {
			if strings.Contains(text, "synthetic-id") || strings.Contains(text, "synthetic-query") || strings.Contains(text, "synthetic-header") || strings.Contains(text, "false") {
				t.Fatal("stream diagnostics exposed payload")
			}
		}
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha175Closed(t, stream)
}

func TestNHA175SSEFramingLimitsAndTruncation(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, func(_ *integrations.CloudLimitsInput, s *integrations.CloudStreamLimitsInput) {
		s.MaxReceiveBytes = 48
		s.MaxSSELineBytes = 24
	}))
	client := nha175Client(t, rig)
	for _, tc := range []struct {
		name, payload string
		want          integrations.CloudFailureKind
	}{
		{"line", ":" + strings.Repeat("x", 25) + "\n\n", integrations.CloudResponseTooLarge},
		{"event", strings.Repeat("data: 0123456789\n", 4) + "\n", integrations.CloudResponseTooLarge},
		{"utf8", "data: \xff\n\n", integrations.CloudStreamProtocolFailed},
		{"truncated_line", "data: unfinished", integrations.CloudStreamProtocolFailed},
		{"truncated_event", "data: unfinished\n", integrations.CloudStreamProtocolFailed},
		{"clean_eof", ": comment\n\n", integrations.CloudStreamEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.payload)
			})
			stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
			_, err := stream.Receive(context.Background())
			nha096Kind(t, err, tc.want)
			nha175Closed(t, stream)
		})
	}
	endpoint := nha175SSE(t, "data: CR\r\r")
	stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
	message, err := stream.Receive(context.Background())
	if err != nil || nha175Bytes(t, message) != "CR" {
		t.Fatal("standalone CR framing lost")
	}
}

func TestNHA175SharedSlotsImmediateSaturationAndFiniteProgress(t *testing.T) {
	account, err := integrations.ParseCloudAccountID("synthetic-shared-account")
	if err != nil {
		t.Fatal(err)
	}
	first := nha016Instance(t, "nha096-cloud")
	second := nha016Instance(t, "synthetic-second")
	b1, _ := integrations.NewCloudAccountBinding(first, account)
	b2, _ := integrations.NewCloudAccountBinding(second, account)
	policy := nha175Policy(t, func(h *integrations.CloudLimitsInput, s *integrations.CloudStreamLimitsInput) {
		h.MaxInFlight = 1
		s.MaxActiveStreams = 1
	}, b1, b2)
	rig := nha096CloudRig(t, policy)
	client := nha175Client(t, rig)
	session2, err := rig.host.OpenSession(second)
	if err != nil {
		t.Fatal(err)
	}
	defer rig.host.Revoke(session2)
	client2, err := integrations.NewCloudClient(session2, policy, nha175Transport())
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	third := nha016Instance(t, "synthetic-independent")
	session3, err := rig.host.OpenSession(third)
	if err != nil {
		t.Fatal(err)
	}
	defer rig.host.Revoke(session3)
	client3, err := integrations.NewCloudClient(session3, policy, nha175Transport())
	if err != nil {
		t.Fatal(err)
	}
	defer client3.Close()
	var calls atomic.Int32
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/finite" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: live\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	request := nha175Request(t, integrations.CloudStreamSSE, endpoint.URL)
	stream := nha175Open(t, client, request)
	start := time.Now()
	_, err = client2.OpenObservationStream(context.Background(), request)
	nha096Kind(t, err, integrations.CloudStreamCapacity)
	if time.Since(start) > 250*time.Millisecond || calls.Load() != 1 {
		t.Fatal("stream saturation queued or entered transport")
	}
	finite, _ := integrations.NewCloudRequest(http.MethodGet, endpoint.URL+"/finite", nil, nil)
	if _, err := client2.Observe(context.Background(), finite); err != nil {
		t.Fatal("long stream monopolized HTTP permit", err)
	}
	independent := nha175Open(t, client3, request)
	if calls.Load() != 3 {
		t.Fatal("independent scope did not open")
	}
	if err := independent.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacement := nha175Open(t, client2, request)
	if err := replacement.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.Close()
	_, err = client.OpenObservationStream(context.Background(), request)
	nha096Kind(t, err, integrations.CloudStreamClosed)
}

type nha175Admission struct {
	count   atomic.Int32
	retry   time.Time
	err     error
	entered chan struct{}
	release chan struct{}
}

func (a *nha175Admission) ReserveAttempt(ctx context.Context) (time.Time, error) {
	a.count.Add(1)
	if a.entered != nil {
		close(a.entered)
		select {
		case <-a.release:
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
	}
	return a.retry, a.err
}
func TestNHA175DurableAdmissionAndSessionFence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry time.Time
		err   error
		kind  integrations.CloudFailureKind
	}{
		{"quota", time.Now().Add(time.Hour), nil, integrations.CloudLocalQuotaExhausted},
		{"commit_unknown", time.Time{}, errors.New("synthetic-private-storage-canary"), integrations.CloudQuotaStoreUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := nha096CloudRig(t, nha175Policy(t, nil))
			admission := &nha175Admission{retry: tc.retry, err: tc.err}
			client := nha175Client(t, rig, admission)
			var calls atomic.Int32
			endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			_, err := client.OpenObservationStream(context.Background(), nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL))
			nha096Kind(t, err, tc.kind)
			if calls.Load() != 0 || admission.count.Load() != 1 || strings.Contains(fmt.Sprint(err), "canary") {
				t.Fatal("admission entered transport or leaked storage error")
			}
		})
	}
	rig := nha096CloudRig(t, nha175Policy(t, nil))
	admission := &nha175Admission{entered: make(chan struct{}), release: make(chan struct{})}
	client := nha175Client(t, rig, admission)
	var calls atomic.Int32
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	finished := make(chan error, 1)
	go func() {
		_, err := client.OpenObservationStream(context.Background(), nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
		finished <- err
	}()
	<-admission.entered
	if err := rig.host.Revoke(rig.session); err != nil {
		t.Fatal(err)
	}
	close(admission.release)
	if err := nha096Wait(t, finished); err == nil || calls.Load() != 0 {
		t.Fatal("late durable admission crossed retired session")
	}
}

func TestNHA175RetryAfterBeforeBodySharedAndNoRedirect(t *testing.T) {
	for _, kind := range []integrations.CloudStreamKind{integrations.CloudStreamSSE, integrations.CloudStreamWebSocket} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			rig := nha096CloudRig(t, nha175Policy(t, nil))
			client := nha175Client(t, rig)
			var calls, followed atomic.Int32
			closed := make(chan struct{}, 1)
			endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/redirect" {
					w.Header().Set("Location", "/forbidden?token=synthetic-token")
					w.WriteHeader(http.StatusFound)
					return
				}
				if r.URL.Path == "/forbidden" {
					followed.Add(1)
					return
				}
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				closed <- struct{}{}
			})
			start := time.Now()
			_, err := client.OpenObservationStream(context.Background(), nha175Request(t, kind, endpoint.URL))
			failure := nha096Kind(t, err, integrations.CloudQuotaDeferred)
			if until, ok := failure.RetryAt(); !ok || until.Before(start.Add(59*time.Second)) {
				t.Fatal("Retry-After not preserved")
			}
			if time.Since(start) > time.Second || calls.Load() != 1 {
				t.Fatal("429 waited for body or retried")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("429 body not closed")
			}
			other := nha175Client(t, rig)
			request, _ := integrations.NewCloudRequest(http.MethodGet, endpoint.URL+"/finite", nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			_, err = other.Observe(ctx, request)
			if err == nil || calls.Load() != 1 {
				t.Fatal("shared notBefore bypassed")
			}
			// A separate scope avoids changing the 429 fence for the redirect case.
			independent := nha096CloudRig(t, nha175Policy(t, nil))
			next := nha175Client(t, independent)
			_, err = next.OpenObservationStream(context.Background(), nha175Request(t, kind, endpoint.URL+"/redirect"))
			nha096Kind(t, err, integrations.CloudHTTPFailed)
			if followed.Load() != 0 || calls.Load() != 2 || strings.Contains(fmt.Sprintf("%#v", err), "synthetic-token") {
				t.Fatal("redirect followed or leaked")
			}
		})
	}
}

func TestNHA175WebSocketTLSUsesCarrierAndOwnedMessages(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, nil))
	var protocol atomic.Int32
	finished := make(chan struct{}, 1)
	endpoint := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol.Store(int32(r.ProtoMajor))
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageBinary, payload); err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
		finished <- struct{}{}
	}))
	endpoint.EnableHTTP2 = true
	endpoint.StartTLS()
	t.Cleanup(func() { endpoint.CloseClientConnections(); endpoint.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(endpoint.Certificate())
	var dials, proxies atomic.Int32
	transport := nha175Transport()
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, address)
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.ForceAttemptHTTP2 = true
	transport.Proxy = func(*http.Request) (*url.URL, error) { proxies.Add(1); return nil, nil }
	client, err := integrations.NewCloudClient(rig.session, rig.policy, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL))
	input := []byte("synthetic-ws-canary")
	message, err := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketText, input)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	if err := stream.Write(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	received, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kind, err := received.Kind()
	if err != nil || kind != integrations.CloudStreamWebSocketBinary || nha175Bytes(t, received) != "synthetic-ws-canary" {
		t.Fatal("WS type/copy/payload lost")
	}
	if protocol.Load() != 1 || dials.Load() != 1 || proxies.Load() != 1 {
		t.Fatalf("carrier not reused: proto%d dial%d proxy%d", protocol.Load(), dials.Load(), proxies.Load())
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("server did not observe socket close")
	}
}

func TestNHA175WebSocketReceiveAndWriteBounds(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, func(_ *integrations.CloudLimitsInput, s *integrations.CloudStreamLimitsInput) {
		s.MaxReceiveBytes = 16
		s.MaxSSELineBytes = 16
		s.MaxWriteBytes = 8
	}))
	client := nha175Client(t, rig)
	for _, tc := range []struct {
		name    string
		kind    websocket.MessageType
		payload []byte
		want    integrations.CloudFailureKind
	}{
		{"text_zero", websocket.MessageText, []byte("0"), integrations.CloudFailureUnspecified},
		{"text_empty", websocket.MessageText, nil, integrations.CloudFailureUnspecified},
		{"binary_exact", websocket.MessageBinary, []byte(strings.Repeat("x", 16)), integrations.CloudFailureUnspecified},
		{"binary_oversize", websocket.MessageBinary, []byte(strings.Repeat("x", 17)), integrations.CloudResponseTooLarge},
		{"text_invalid_utf8", websocket.MessageText, []byte{0xff}, integrations.CloudStreamProtocolFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clientMessages atomic.Int32
			endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if err := conn.Write(r.Context(), tc.kind, tc.payload); err != nil {
					return
				}
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					clientMessages.Add(1)
				}
			})
			stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL))
			tooLarge, _ := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketBinary, []byte("123456789"))
			nha096Kind(t, stream.Write(context.Background(), tooLarge), integrations.CloudResponseTooLarge)
			message, err := stream.Receive(context.Background())
			if tc.want != integrations.CloudFailureUnspecified {
				nha096Kind(t, err, tc.want)
				nha175Closed(t, stream)
			} else {
				if err != nil || nha175Bytes(t, message) != string(tc.payload) {
					t.Fatal("bounded WS data lost", err)
				}
				kind, err := message.Kind()
				want := integrations.CloudStreamWebSocketText
				if tc.kind == websocket.MessageBinary {
					want = integrations.CloudStreamWebSocketBinary
				}
				if err != nil || kind != want {
					t.Fatal("WS variant lost")
				}
				if err := stream.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if clientMessages.Load() != 0 {
				t.Fatal("oversize write entered transport")
			}
		})
	}
}

func TestNHA175RemoteEOFDegenerationRequiresNewObservation(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, nil))
	client := nha175Client(t, rig)
	end := make(chan struct{})
	var endOnce sync.Once
	t.Cleanup(func() { endOnce.Do(func() { close(end) }) })
	var connections atomic.Int32
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		first := connections.Add(1) == 1
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: synthetic-snapshot\n\n")
		w.(http.Flusher).Flush()
		if first {
			select {
			case <-end:
			case <-r.Context().Done():
			}
		} else {
			<-r.Context().Done()
		}
	})
	request := nha175Request(t, integrations.CloudStreamSSE, endpoint.URL)
	stream := nha175Open(t, client, request)
	old, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	ids, err := rig.fixture.Resolve(context.Background(), rig.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], rig.instance, key, "switch.nha175_eof", "switch.nha175_eof", false)
	if err := rig.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	driver, _ := fake.NewActionDriver(time.Now())
	if err := driver.Start(context.Background(), rig.session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	admit := func(message integrations.CloudStreamMessage) {
		t.Helper()
		if err := rig.session.Publish(context.Background(), binding, nha016Observation(t, time.Now())); err != nil {
			t.Fatal(err)
		}
		if err := client.ConfirmStreamObservationAdmitted(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	health := func() integrations.HealthStatus {
		t.Helper()
		snapshot, err := rig.fixture.Sequencer().Capture()
		if err != nil {
			t.Fatal(err)
		}
		status, _, _ := snapshot.Health[rig.instance].Components()
		return status
	}
	admit(old)
	if health() != integrations.HealthReady {
		t.Fatal("initial admitted snapshot not ready")
	}
	endOnce.Do(func() { close(end) })
	_, err = stream.Receive(context.Background())
	nha096Kind(t, err, integrations.CloudStreamEnded)
	nha175Closed(t, stream)
	if health() != integrations.HealthDegraded {
		t.Fatal("remote EOF preserved healthy state")
	}
	if err := client.ConfirmStreamObservationAdmitted(context.Background(), old); err == nil {
		t.Fatal("old receipt recovered health after EOF")
	}
	replacement := nha175Open(t, client, request)
	if health() != integrations.HealthDegraded {
		t.Fatal("opening replacement recovered health")
	}
	fresh, err := replacement.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admit(fresh)
	if health() != integrations.HealthReady {
		t.Fatal("new admitted observation did not recover health")
	}
	if err := replacement.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health() != integrations.HealthReady {
		t.Fatal("voluntary close degraded health")
	}
}

type nha175GateConn struct {
	net.Conn
	block                *atomic.Bool
	entered              chan struct{}
	closed               chan struct{}
	enterOnce, closeOnce sync.Once
}

type nha175DrainConn struct {
	net.Conn
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
	err     error
}

func (c *nha175DrainConn) Close() error {
	c.once.Do(func() { close(c.entered); <-c.release; c.err = c.Conn.Close() })
	return c.err
}

func TestNHA175SharedHealthEpochPrecedesDrainAndRecoversOnce(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, nil))
	drainEntered, drainRelease := make(chan struct{}), make(chan struct{})
	var drainOnce sync.Once
	t.Cleanup(func() { drainOnce.Do(func() { close(drainRelease) }) })
	serverRelease, fresh := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		data := []byte("old-receipt")
		if r.URL.Path == "/a" {
			data = []byte{0xff}
		}
		if err := conn.Write(r.Context(), websocket.MessageText, data); err != nil {
			return
		}
		if r.URL.Path == "/b" {
			select {
			case <-fresh:
			case <-serverRelease:
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte("fresh-receipt")); err != nil {
				return
			}
		}
		<-serverRelease
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(serverRelease) }); drainOnce.Do(func() { close(drainRelease) }) })
	transport := nha175Transport()
	dial := transport.DialContext
	var dials atomic.Int32
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if dials.Add(1) == 1 {
			return &nha175DrainConn{Conn: conn, entered: drainEntered, release: drainRelease}, nil
		}
		return conn, nil
	}
	client, err := integrations.NewCloudClient(rig.session, rig.policy, transport)
	if err != nil {
		t.Fatal(err)
	}
	// Ensure cleanup releases the intentionally noncooperative Close before join.
	t.Cleanup(func() { drainOnce.Do(func() { close(drainRelease) }); client.Close() })
	a := nha175Open(t, client, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL+"/a"))
	b := nha175Open(t, client, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL+"/b"))
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	ids, err := rig.fixture.Resolve(context.Background(), rig.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], rig.instance, key, "switch.nha175_epoch", "switch.nha175_epoch", false)
	if err := rig.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	driver, _ := fake.NewActionDriver(time.Now())
	if err := driver.Start(context.Background(), rig.session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	old, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Receive(context.Background())
	nha096Kind(t, err, integrations.CloudStreamProtocolFailed)
	select {
	case <-drainEntered:
	case <-time.After(time.Second):
		t.Fatal("drain barrier not reached")
	}
	select {
	case <-a.Done():
		t.Fatal("Done preceded drain")
	default:
	}
	nha096Kind(t, client.ConfirmStreamObservationAdmitted(context.Background(), old), integrations.CloudInvalid)
	_, err = client.OpenObservationStream(context.Background(), nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL+"/extra"))
	nha096Kind(t, err, integrations.CloudStreamCapacity)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	nha096Kind(t, a.Close(ctx), integrations.CloudBudgetExhausted)
	cancel()
	close(fresh)
	current, err := b.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(context.Background(), binding, nha016Observation(t, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfirmStreamObservationAdmitted(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	health := func() integrations.HealthStatus {
		t.Helper()
		snapshot, err := rig.fixture.Sequencer().Capture()
		if err != nil {
			t.Fatal(err)
		}
		status, _, _ := snapshot.Health[rig.instance].Components()
		return status
	}
	if health() != integrations.HealthReady {
		t.Fatal("recovery blocked by another stream drain")
	}
	drainOnce.Do(func() { close(drainRelease) })
	nha175Closed(t, a)
	if err := client.ConfirmStreamObservationAdmitted(context.Background(), current); err != nil {
		t.Fatal("drain advanced epoch twice", err)
	}
	if health() != integrations.HealthReady {
		t.Fatal("drain degraded recovered health")
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health() != integrations.HealthReady {
		t.Fatal("voluntary close degraded recovered health")
	}
	releaseOnce.Do(func() { close(serverRelease) })
}

func (c *nha175GateConn) Write(p []byte) (int, error) {
	if c.block.Load() {
		c.enterOnce.Do(func() { close(c.entered) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}
func (c *nha175GateConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
func TestNHA175ClientCloseJoinsBlockedReadAndWrite(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, func(_ *integrations.CloudLimitsInput, s *integrations.CloudStreamLimitsInput) { s.MaxActiveStreams = 1 }))
	var blocked atomic.Bool
	entered := make(chan struct{})
	serverRelease := make(chan struct{})
	var releaseOnce sync.Once
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		<-serverRelease
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(serverRelease) }) })
	transport := nha175Transport()
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &nha175GateConn{Conn: conn, block: &blocked, entered: entered, closed: make(chan struct{})}, nil
	}
	client, err := integrations.NewCloudClient(rig.session, rig.policy, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL))
	blocked.Store(true)
	readDone := make(chan error, 1)
	go func() { _, err := stream.Receive(context.Background()); readDone <- err }()
	message, _ := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketText, []byte("synthetic-subscription"))
	writeDone := make(chan error, 1)
	go func() { writeDone <- stream.Write(context.Background(), message) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("write did not reach deterministic transport barrier")
	}
	second, _ := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketText, []byte("must-not-queue"))
	nha096Kind(t, stream.Write(context.Background(), second), integrations.CloudStreamBusy)
	closed := make(chan struct{})
	go func() { client.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("CloudClient.Close did not join")
	}
	if nha096Wait(t, readDone) == nil || nha096Wait(t, writeDone) == nil {
		t.Fatal("blocked I/O returned success after close")
	}
	nha175Closed(t, stream)
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacement := nha175Client(t, rig)
	other := nha175Open(t, replacement, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL))
	if err := other.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(serverRelease) })
}

func TestNHA175ContextIdleAndLateOpenRetirement(t *testing.T) {
	t.Run("caller_cancel", func(t *testing.T) {
		rig := nha096CloudRig(t, nha175Policy(t, nil))
		client := nha175Client(t, rig)
		endpoint := nha175SSE(t, ": waiting\n\n")
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := client.OpenObservationStream(ctx, nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
		if err != nil {
			t.Fatal(err)
		}
		ready, _ := integrations.NewHealthReport(integrations.HealthReady, "")
		if err := rig.session.ReportHealth(context.Background(), ready); err != nil {
			t.Fatal(err)
		}
		readDone := make(chan error, 1)
		go func() { _, err := stream.Receive(context.Background()); readDone <- err }()
		cancel()
		nha096Kind(t, nha096Wait(t, readDone), integrations.CloudStreamCanceled)
		nha175Closed(t, stream)
		snapshot, err := rig.fixture.Sequencer().Capture()
		if err != nil {
			t.Fatal(err)
		}
		if status, _, _ := snapshot.Health[rig.instance].Components(); status != integrations.HealthReady {
			t.Fatal("voluntary cancellation degraded health")
		}
	})
	t.Run("idle", func(t *testing.T) {
		rig := nha096CloudRig(t, nha175Policy(t, func(_ *integrations.CloudLimitsInput, s *integrations.CloudStreamLimitsInput) {
			s.IdleTimeout = 50 * time.Millisecond
		}))
		client := nha175Client(t, rig)
		endpoint := nha175SSE(t, ": waiting\n\n")
		stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
		_, err := stream.Receive(context.Background())
		nha096Kind(t, err, integrations.CloudStreamIdle)
		nha175Closed(t, stream)
	})
	t.Run("late_headers", func(t *testing.T) {
		rig := nha096CloudRig(t, nha175Policy(t, nil))
		client := nha175Client(t, rig)
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: too-late\n\n")
		})
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		type result struct {
			stream *integrations.CloudObservationStream
			err    error
		}
		done := make(chan result, 1)
		request := nha175Request(t, integrations.CloudStreamSSE, endpoint.URL)
		go func() {
			stream, err := client.OpenObservationStream(context.Background(), request)
			done <- result{stream, err}
		}()
		<-entered
		if err := rig.host.Revoke(rig.session); err != nil {
			t.Fatal(err)
		}
		releaseOnce.Do(func() { close(release) })
		got := nha096Wait(t, done)
		if got.err == nil || got.stream != nil {
			t.Fatal("retired session returned late live stream")
		}
	})
}

func TestNHA175OpenAndWriteTimeouts(t *testing.T) {
	t.Run("opening", func(t *testing.T) {
		rig := nha096CloudRig(t, nha175Policy(t, func(h *integrations.CloudLimitsInput, _ *integrations.CloudStreamLimitsInput) {
			h.AttemptTimeout = 50 * time.Millisecond
			h.TotalTimeout = 500 * time.Millisecond
		}))
		client := nha175Client(t, rig)
		var calls atomic.Int32
		closed := make(chan struct{})
		endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done(); close(closed) })
		start := time.Now()
		stream, err := client.OpenObservationStream(context.Background(), nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
		nha096Kind(t, err, integrations.CloudBudgetExhausted)
		if stream != nil || calls.Load() != 1 || time.Since(start) > time.Second {
			t.Fatal("opening budget/replay/handle violated")
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("opening timeout did not close HTTP request")
		}
	})
	t.Run("writing", func(t *testing.T) {
		rig := nha096CloudRig(t, nha175Policy(t, func(_ *integrations.CloudLimitsInput, s *integrations.CloudStreamLimitsInput) {
			s.WriteTimeout = 30 * time.Millisecond
		}))
		serverRelease := make(chan struct{})
		var releaseOnce sync.Once
		endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			<-serverRelease
		})
		t.Cleanup(func() { releaseOnce.Do(func() { close(serverRelease) }) })
		var block atomic.Bool
		entered := make(chan struct{})
		transport := nha175Transport()
		dial := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &nha175GateConn{Conn: conn, block: &block, entered: entered, closed: make(chan struct{})}, nil
		}
		client, err := integrations.NewCloudClient(rig.session, rig.policy, transport)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamWebSocket, endpoint.URL))
		block.Store(true)
		message, _ := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketText, []byte("synthetic-frame"))
		start := time.Now()
		err = stream.Write(context.Background(), message)
		nha096Kind(t, err, integrations.CloudBudgetExhausted)
		select {
		case <-entered:
		default:
			t.Fatal("write timeout never entered transport")
		}
		if time.Since(start) > time.Second {
			t.Fatal("write timeout failed to unblock")
		}
		nha175Closed(t, stream)
		releaseOnce.Do(func() { close(serverRelease) })
	})
}

func TestNHA175StreamHealthRequiresHostAdmissionAndCurrentReceipt(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, func(h *integrations.CloudLimitsInput, _ *integrations.CloudStreamLimitsInput) {
		h.ObservationAttempts = 1
	}))
	client := nha175Client(t, rig)
	endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fault" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "synthetic-token")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\ndata: recovered\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	stream := nha175Open(t, client, nha175Request(t, integrations.CloudStreamSSE, endpoint.URL))
	old, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failureRequest, _ := integrations.NewCloudRequest(http.MethodGet, endpoint.URL+"/fault", nil, nil)
	_, err = client.Observe(context.Background(), failureRequest)
	nha096Kind(t, err, integrations.CloudCredentialFailed)
	nha096Kind(t, client.ConfirmStreamObservationAdmitted(context.Background(), old), integrations.CloudInvalid)
	snapshot, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := snapshot.Health[rig.instance].Components(); status != integrations.HealthDegraded {
		t.Fatal("open stream erased failed HTTP health")
	}
	recovered, err := stream.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	ids, err := rig.fixture.Resolve(context.Background(), rig.instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], rig.instance, key, "switch.nha175", "switch.nha175", false)
	if err := rig.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	driver, _ := fake.NewActionDriver(time.Now())
	if err := driver.Start(context.Background(), rig.session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(context.Background(), binding, nha016Observation(t, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfirmStreamObservationAdmitted(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	snapshot, err = rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := snapshot.Health[rig.instance].Components(); status != integrations.HealthReady {
		t.Fatal("admitted observation did not recover health")
	}
}

func TestNHA175MalformedHandshakeAndEventHeadersRedactCanaries(t *testing.T) {
	rig := nha096CloudRig(t, nha175Policy(t, func(h *integrations.CloudLimitsInput, _ *integrations.CloudStreamLimitsInput) {
		h.AttemptTimeout = 100 * time.Millisecond
		h.TotalTimeout = time.Second
	}))
	client := nha175Client(t, rig)
	for _, kind := range []integrations.CloudStreamKind{integrations.CloudStreamSSE, integrations.CloudStreamWebSocket} {
		closed := make(chan struct{}, 1)
		endpoint := nha175Server(t, func(w http.ResponseWriter, r *http.Request) {
			if kind == integrations.CloudStreamSSE {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "synthetic-private-cookie=secret")
				_, _ = io.WriteString(w, "synthetic-private-body")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				closed <- struct{}{}
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: synthetic-private-header\r\n\r\nsynthetic-private-body")
			_ = rw.Flush()
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			var probe [1]byte
			_, err = conn.Read(probe[:])
			var timeout net.Error
			if err != nil && (!errors.As(err, &timeout) || !timeout.Timeout()) {
				closed <- struct{}{}
			}
		})
		request := nha175Request(t, kind, endpoint.URL+"/?token=synthetic-private-query")
		start := time.Now()
		_, err := client.OpenObservationStream(context.Background(), request)
		if err == nil {
			t.Fatal("invalid handshake admitted")
		}
		var failure *integrations.CloudError
		if !errors.As(err, &failure) || len(failure.ResponseBytes()) != 0 {
			t.Fatal("stream failure exposed library response")
		}
		data, _ := json.Marshal(failure)
		for _, text := range []string{fmt.Sprint(err), fmt.Sprintf("%#v", failure), string(data)} {
			if strings.Contains(text, "synthetic-private") {
				t.Fatal("library error leaked canary")
			}
		}
		if time.Since(start) > time.Second {
			t.Fatal("malformed response waited for library diagnostic timeout")
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("malformed body was not closed at peer")
		}
	}
}

type nha175ServerPilot struct {
	policy           *integrations.CloudPolicy
	url              string
	store            *storage.Store
	stream           *integrations.CloudObservationStream
	client           *integrations.CloudClient
	session          *integrations.Session
	started, stopped chan error
}

func (p *nha175ServerPilot) Start(ctx context.Context, host integrations.SessionHost, _ integrations.Tasks) error {
	fail := func(err error) error { p.started <- err; return err }
	var err error
	p.session = host.(*integrations.Session)
	p.client, err = integrations.NewCloudClient(host, p.policy, nha175Transport())
	if err != nil {
		return fail(err)
	}
	request, err := integrations.NewCloudStreamRequest(integrations.CloudStreamSSE, http.MethodGet, p.url, nil, nil)
	if err != nil {
		return fail(err)
	}
	p.stream, err = p.client.OpenObservationStream(ctx, request)
	if err != nil {
		return fail(err)
	}
	_, err = p.stream.Receive(ctx)
	if err != nil {
		return fail(err)
	}
	p.started <- nil
	return nil
}
func (p *nha175ServerPilot) Stop(ctx context.Context) error {
	var err error
	select {
	case <-p.session.Done():
	default:
		err = errors.New("stream Stop preceded session retirement")
	}
	if closeErr := p.stream.Close(ctx); closeErr != nil {
		err = closeErr
	}
	p.client.Close()
	if err == nil {
		err = p.store.InTransaction(ctx, func(tx pgx.Tx) error { var one int; return tx.QueryRow(ctx, "SELECT 1").Scan(&one) })
	}
	p.stopped <- err
	return err
}
func TestNHA175RunWithFactoryJoinsStreamsBeforePostgresClose(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := testpg.ShortPrivateDataDir(t)
	input := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", dataDir, cluster.Port)
	startup, err := config.NewDetachedStartup([]byte(input), "configuration.yaml", nil, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	endpoint := nha175SSE(t, "data: synthetic-observation\n\n")
	policy := nha175Policy(t, nil)
	_, identity, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "synthetic-nha175-server")
	pilot := &nha175ServerPilot{policy: policy, url: endpoint.URL, started: make(chan error, 1), stopped: make(chan error, 1)}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ context.Context, store *storage.Store) (server.RuntimePlan, error) {
		pilot.store = store
		return server.RuntimePlan{Identity: identity, Pilots: []integrations.PilotSpec{spec}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() {
		done <- server.RunWithStartupFactory(ctx, startup, lc.Listen, func(address net.Addr) { ready <- address }, factory)
	}()
	if err := nha096Wait(t, pilot.started); err != nil {
		t.Fatal(err)
	}
	_ = nha096Wait(t, ready)
	cancel()
	if err := nha096Wait(t, done); err != nil {
		t.Fatal(err)
	}
	if err := nha096Wait(t, pilot.stopped); err != nil {
		t.Fatal(err)
	}
	nha175Closed(t, pilot.stream)
}
