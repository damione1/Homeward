package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This qualification isolates the protocol adapter with the official client;
// the actual Run tests separately qualify its shared services and key admission.
func TestNHA052SDKAdapterRejectsBeforeReadAndIsolatesPanicAndReplyLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "NHA052 adapter fixture", Version: "1"}, nil)
	var calls atomic.Int32
	schema := mcpObject(map[string]any{"pageSize": map[string]any{"type": "integer", "minimum": 1, "maximum": 256}}, "pageSize")
	mcpReadTool(server, "fixture.read", "Synthetic adapter read", schema, func(_ context.Context, r *connect.Request[state.OpenViewRequest]) (*connect.Response[state.OpenViewResponse], error) {
		calls.Add(1)
		switch r.Msg.PageSize {
		case 1:
			panic("SYNTHETIC-PANIC-SECRET-MUST-NOT-LEAK")
		case 2:
			return connect.NewResponse(&state.OpenViewResponse{SnapshotToken: strings.Repeat("x", maxMCPReadResultBytes)}), nil
		default:
			return connect.NewResponse(&state.OpenViewResponse{SnapshotToken: "valid synthetic reply"}), nil
		}
	})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", isolateMCPReads(http.NewCrossOriginProtection().Handler(transport)))
	mux.HandleFunc("/control", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	httpServer := httptest.NewTLSServer(mux)
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "NHA052 official test client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp", HTTPClient: httpServer.Client(), DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, arguments := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"pageSize":0}`), json.RawMessage(`{"pageSize":257}`), json.RawMessage(`{"pageSize":1,"actor":"admin"}`), json.RawMessage(`{"pageSize":1,"pageSize":3}`), json.RawMessage(`{"pageSize":"future-enum"}`), json.RawMessage(`null`), json.RawMessage(`[]`)} {
		r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "fixture.read", Arguments: arguments})
		if e == nil && (r == nil || !r.IsError) {
			t.Fatalf("invalid input admitted: %s", arguments)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached read callback", calls.Load())
	}
	for _, size := range []int{1, 2} {
		r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "fixture.read", Arguments: map[string]any{"pageSize": size}})
		if e != nil || r == nil || !r.IsError {
			t.Fatal("panic/oversized response not contained", size, e)
		}
		encoded, _ := json.Marshal(r)
		if len(encoded) > maxMCPReadResultBytes || strings.Contains(string(encoded), "SYNTHETIC-PANIC-SECRET") {
			t.Fatal("unsafe adapter failure")
		}
		response, e := httpServer.Client().Get(httpServer.URL + "/control")
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatal("MCP fault stopped control")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("panic and oversized reply were not exercised through the anchored SDK adapter", calls.Load())
	}
	nha052Call(t, ctx, session, "fixture.read", map[string]any{"pageSize": 3}, nil)
	if calls.Load() != 3 {
		t.Fatal("healthy read failed after contained MCP faults")
	}
	// The whole handler wrapper also contains failures before protocol dispatch.
	recorder := httptest.NewRecorder()
	isolateMCPReads(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("SYNTHETIC-HTTP-PANIC") })).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "PANIC") {
		t.Fatal("HTTP handler panic escaped")
	}
}

// Reproduce the SDK's value-preserving detached context and hold the same
// authority mutation barrier used by rights publication. No watcher, timer or
// asynchronous cancellation callback can satisfy the pre-release assertions.
func TestNHA052DetachedSDKReadKeepsSynchronousLeaseCancellationUnderBarrier(t *testing.T) {
	account, err := contracts.ParseAccountID("nha052-account")
	if err != nil {
		t.Fatal(err)
	}
	key, err := contracts.ParseMcpKeyID("nha052-key")
	if err != nil {
		t.Fatal(err)
	}
	permission, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorization.NewScopeSet([]authorization.Permission{permission})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authorization.ScopedPrincipal(account, key, scopes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.EmptySource(account)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := source.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	makeState := func(revision uint64) authorization.AccountState {
		version, e := authorization.NewVersion(revision, hash)
		if e != nil {
			t.Fatal(e)
		}
		state, e := authorization.NewAccountState(source, auth.AccountRole_ACCOUNT_ROLE_USER, auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, revision)
		if e != nil {
			t.Fatal(e)
		}
		return state
	}
	authority := authorization.NewAuthority()
	if err := authority.Hydrate([]authorization.AccountState{makeState(1)}); err != nil {
		t.Fatal(err)
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer deadlineCancel()
	original, cancelLease := context.WithCancelCause(deadlineCtx)
	defer cancelLease(context.Canceled)
	anchored := context.WithValue(original, mcpReadHTTPContextKey{}, mcpReadHTTPContext{original})
	rpcCtx := context.WithoutCancel(anchored)
	business, release, err := mcpReadBusinessContext(rpcCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wantDeadline, _ := original.Deadline()
	gotDeadline, ok := business.Deadline()
	if !ok || !gotDeadline.Equal(wantDeadline) {
		t.Fatal("SDK read lost original HTTP deadline")
	}
	mutation, err := authority.BeginMutation(account)
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Release()
	waiting := newNHA145PausedFirstErrContext(business)
	defer waiting.resume()
	var callbacks atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- authority.WithCurrent(waiting, principal, func(authorization.Current) error { callbacks.Add(1); return nil })
	}()
	select {
	case initial := <-waiting.observed:
		if initial != nil {
			t.Fatal(initial)
		}
	case <-deadlineCtx.Done():
		t.Fatal(deadlineCtx.Err())
	}
	cause := authorizationTerminationCause{reason: auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED}
	cancelLease(cause)
	// WithCancel must already observe its direct parent's cancellation here,
	// while the mutation is held; an AfterFunc-only auth bridge is insufficient.
	if business.Err() == nil || context.Cause(business) != cause {
		t.Fatal("auth lease cancellation was deferred behind an SDK callback")
	}
	select {
	case <-business.Done():
	default:
		t.Fatal("business lease Done remains open under mutation")
	}
	if err := authority.Publish(makeState(2)); err != nil {
		t.Fatal(err)
	}
	waiting.resume()
	mutation.Release()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || callbacks.Load() != 0 {
			t.Fatal("old MCP read reached newly published authority", err, callbacks.Load())
		}
	case <-deadlineCtx.Done():
		t.Fatal(deadlineCtx.Err())
	}
	if _, _, err := mcpReadBusinessContext(context.Background()); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("unanchored SDK context admitted")
	}
}

func TestNHA052RejectsLegacyBatchesBeforeSDKDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "NHA052 single-message fixture", Version: "1"}, nil)
	var calls atomic.Int32
	mcpReadTool(server, "fixture.read", "One synthetic read", mcpObject(nil), func(context.Context, *connect.Request[state.OpenViewRequest]) (*connect.Response[state.OpenViewResponse], error) {
		calls.Add(1)
		return connect.NewResponse(&state.OpenViewResponse{SnapshotToken: "single-message"}), nil
	})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	endpoint := httptest.NewTLSServer(isolateMCPReads(transport))
	defer endpoint.Close()
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture.read","arguments":{}}}`
	other := strings.Replace(call, `"id":1`, `"id":2`, 1)
	post := func(body, version string) int {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if version != "" {
			request.Header.Set("MCP-Protocol-Version", version)
		}
		response, err := endpoint.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	for _, version := range []string{"", "2025-03-26", "2025-11-25"} {
		for _, body := range []string{"[" + call + "," + other + "]", "[]", " \r\n\t[" + call + "]", "[" + call} {
			if status := post(body, version); status != http.StatusBadRequest {
				t.Fatal("legacy batch reached SDK", version, status)
			}
			if calls.Load() != 0 {
				t.Fatal("batch dispatched a read", calls.Load())
			}
		}
	}
	for _, body := range []string{"", "null", "true", `{invalid`, `{}` + call} {
		if status := post(body, ""); status < 400 {
			t.Fatal("malformed envelope admitted", status)
		}
		if calls.Load() != 0 {
			t.Fatal("malformed request dispatched read")
		}
	}
	// Ordinary initialized SDK traffic still passes the same single-object gate.
	client := mcp.NewClient(&mcp.Implementation{Name: "NHA052 official client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: endpoint.Client(), DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	nha052Call(t, ctx, session, "fixture.read", map[string]any{}, nil)
	if calls.Load() != 1 {
		t.Fatal("normal SDK object did not dispatch exactly once")
	}
	if status := post(" \r\n\t"+call, "2025-11-25"); status != http.StatusOK || calls.Load() != 2 {
		t.Fatal("valid object with JSON whitespace was rejected", status, calls.Load())
	}
	// A body with unknown length cannot force an unbounded whitespace scan.
	var entered atomic.Int32
	body := &nha052WhitespaceBody{remaining: maxMCPReadRequestBytes + 1024}
	request := httptest.NewRequest(http.MethodPost, "/mcp", body)
	request.ContentLength = -1
	response := httptest.NewRecorder()
	isolateMCPReads(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { entered.Add(1) })).ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || entered.Load() != 0 || body.read > maxMCPReadRequestBytes+1 {
		t.Fatal("unbounded MCP envelope probe", response.Code, body.read, entered.Load())
	}
	known := &nha052WhitespaceBody{remaining: 1}
	request = httptest.NewRequest(http.MethodPost, "/mcp", known)
	request.ContentLength = maxMCPReadRequestBytes + 1
	response = httptest.NewRecorder()
	isolateMCPReads(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { entered.Add(1) })).ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || entered.Load() != 0 || known.read != 0 {
		t.Fatal("oversized Content-Length read before refusal", response.Code, known.read)
	}
}

type nha052WhitespaceBody struct {
	remaining int64
	read      int64
}

func (b *nha052WhitespaceBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > b.remaining {
		n = int(b.remaining)
	}
	for i := range p[:n] {
		p[i] = ' '
	}
	b.remaining -= int64(n)
	b.read += int64(n)
	return n, nil
}
