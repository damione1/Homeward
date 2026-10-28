package server

// Test-only, explicitly enabled NHF020 surface for the deferred NHA163 API.
// This fixture records synthetic transitions; it never evaluates a routine,
// invokes an action service, starts a provider or registers production routes.
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	logic "github.com/damione1/Homeward/gen/home/logic/v1"
	"github.com/damione1/Homeward/gen/home/logic/v1/logicv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf020DefinitionID string
type nhf020RunID string
type nhf020RequestID string
type nhf020PageKind uint8

const (
	nhf020Definitions nhf020PageKind = iota
	nhf020Runs
	nhf020Trace
)

type nhf020Page struct {
	kind        nhf020PageKind
	scope       string
	context     *logic.Context
	offset      int
	expires     time.Time
	definitions []*logic.Definition
	runs        []*logic.Run
	run         *logic.Run
	rows        []*logic.TraceRow
	capture     string
}
type nhf020Counts struct {
	Starts        uint64 `json:"starts"`
	Cancellations uint64 `json:"cancellations"`
	Lookups       uint64 `json:"lookups"`
	LostReplies   uint64 `json:"lost_replies"`
	Restarts      uint64 `json:"restarts"`
}
type nhf020Logic struct {
	logicv1connect.UnimplementedLogicServiceHandler
	mu            sync.Mutex
	documents     configurationv1connect.ConfigurationServiceClient
	accounts      authv1connect.AccountServiceClient
	context       *logic.Context
	definitions   []*logic.Definition
	runs          []*logic.Run
	requests      map[nhf020RequestID]nhf020RunID
	cancellations map[nhf020RequestID]nhf020RunID
	pages         map[string]*nhf020Page
	counts        nhf020Counts
	out           string
	t             *testing.T
}

func nhf020Error(code connect.Code, message string) error {
	return connect.NewError(code, errors.New(message))
}
func nhf020Opaque() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}
func nhf020Message(message proto.Message) bool {
	return proto.Size(message) <= 1<<20 && len(message.ProtoReflect().GetUnknown()) == 0
}
func nhf020ID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character <= ' ' || character == '\u007f' {
			return false
		}
	}
	return true
}
func nhf020Hash(value string) []byte { hash := sha256.Sum256([]byte(value)); return hash[:] }
func nhf020Complete() *logic.Continuation {
	return &logic.Continuation{Result: &logic.Continuation_Complete{Complete: &emptypb.Empty{}}}
}
func nhf020Clone[T proto.Message](value T) T { return proto.Clone(value).(T) }

func (h *nhf020Logic) authorize(ctx context.Context, header http.Header) error {
	admin := connect.NewRequest(&auth.ListAccountsRequest{})
	admin.Header().Set("Authorization", header.Get("Authorization"))
	if _, err := h.accounts.ListAccounts(ctx, admin); err != nil {
		return err
	}
	request := connect.NewRequest(&cfg.ReadDocumentRequest{Id: &cfg.DocumentId{Value: "operational.main"}})
	request.Header().Set("Authorization", header.Get("Authorization"))
	// Fresh real authorization, with no synthetic privilege header or keeper
	// credential forwarded on behalf of the requesting client.
	response, err := h.documents.ReadDocument(ctx, request)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.context == nil || !proto.Equal(h.context.Generation, response.Msg.ActiveGeneration) {
		h.reset(response.Msg.ActiveGeneration)
	}
	return nil
}
func (h *nhf020Logic) reset(generation *cfg.ActiveGeneration) {
	h.context = &logic.Context{Deployment: logic.Deployment_DEPLOYMENT_SYNTHETIC_ONLY,
		Epoch: nhf020Opaque(), CatalogRevision: 1, Generation: nhf020Clone(generation)}
	h.runs = nil
	h.requests = make(map[nhf020RequestID]nhf020RunID)
	h.cancellations = make(map[nhf020RequestID]nhf020RunID)
	h.pages = make(map[string]*nhf020Page)
	// One retained historical record demonstrates inspection without opening
	// the screen dispatching anything. Its actor is explicitly withheld.
	h.runs = append(h.runs, &logic.Run{Id: "synthetic-history", Definition: nhf020Clone(h.definitions[0].Reference),
		Generation: nhf020Clone(generation), Actor: &logic.Actor{Observation: &logic.Actor_Redacted{Redacted: &emptypb.Empty{}}},
		Mode: logic.RunMode_RUN_MODE_SINGLE, Status: logic.RunStatus_RUN_STATUS_COMPLETED,
		Reason: logic.RunReason_RUN_REASON_EXPLICIT_REQUEST, CreatedAt: timestamppb.Now(),
		StartRequestId: "synthetic-history-request", Simulated: true})
}
func nhf020NewLogic(t *testing.T, out string, documents configurationv1connect.ConfigurationServiceClient, accounts authv1connect.AccountServiceClient) *nhf020Logic {
	h := &nhf020Logic{t: t, out: out, documents: documents, accounts: accounts}
	families := []plan.SourceFamily{plan.SourceFamily_SOURCE_FAMILY_AUTOMATION, plan.SourceFamily_SOURCE_FAMILY_SCRIPT, plan.SourceFamily_SOURCE_FAMILY_SCENE}
	modes := []logic.RunMode{logic.RunMode_RUN_MODE_SINGLE, logic.RunMode_RUN_MODE_RESTART, logic.RunMode_RUN_MODE_QUEUED, logic.RunMode_RUN_MODE_PARALLEL}
	for index := 0; index < 17; index++ {
		id := fmt.Sprintf("synthetic-definition-%02d", index+1)
		definition := &logic.Definition{Reference: &logic.DefinitionRef{Id: id, Version: &cfg.DocumentVersion{Revision: 1, SourceSha256: nhf020Hash(id)}},
			Title: fmt.Sprintf("Définition synthétique %02d", index+1), Family: families[index%len(families)],
			SourceDocument: &cfg.DocumentId{Value: "synthetic.logic." + id}, ProtectedSourceReference: nhf020Hash("protected-" + id),
			SchemaVersion: "synthetic-logic-v1", Compatibility: logic.Compatibility_COMPATIBILITY_SUPPORTED,
			Mode: modes[index%len(modes)], CanRun: true, CanSimulate: true}
		switch index {
		case 14:
			definition.Compatibility = logic.Compatibility_COMPATIBILITY_PARTIAL
			definition.Diagnostics = []logic.Diagnostic{logic.Diagnostic_DIAGNOSTIC_EXCLUDED_REFERENCE}
		case 15:
			definition.Compatibility = logic.Compatibility_COMPATIBILITY_UNSUPPORTED
			definition.Diagnostics = []logic.Diagnostic{logic.Diagnostic_DIAGNOSTIC_UNSUPPORTED_EXTENSION}
			definition.CanRun = false
		case 16:
			definition.Compatibility = logic.Compatibility_COMPATIBILITY_UNSUPPORTED
			definition.Diagnostics = []logic.Diagnostic{logic.Diagnostic_DIAGNOSTIC_INVALID_SCHEMA, logic.Diagnostic_DIAGNOSTIC_SOURCE_UNAVAILABLE}
			definition.CanRun = false
			definition.ProtectedSourceReference = nil
		}
		h.definitions = append(h.definitions, definition)
	}
	return h
}
func (h *nhf020Logic) same(expected *logic.Context) error {
	if expected == nil || !proto.Equal(expected, h.context) {
		return nhf020Error(connect.CodeFailedPrecondition, "synthetic context changed; reload explicitly")
	}
	return nil
}
func (h *nhf020Logic) first(kind nhf020PageKind, scope string) *nhf020Page {
	return &nhf020Page{kind: kind, scope: scope, context: nhf020Clone(h.context), expires: time.Now().Add(time.Minute), capture: nhf020Opaque()}
}
func (h *nhf020Logic) page(token *string, kind nhf020PageKind, scope string, size uint32) (*nhf020Page, error) {
	if size == 0 || size > 100 {
		return nil, nhf020Error(connect.CodeInvalidArgument, "page size outside 1..100")
	}
	if token == nil {
		return h.first(kind, scope), nil
	}
	if len(*token) == 0 || len(*token) > 512 {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid cursor")
	}
	page, found := h.pages[*token]
	if !found || page.kind != kind || page.scope != scope || !time.Now().Before(page.expires) || !proto.Equal(page.context, h.context) {
		return nil, nhf020Error(connect.CodeAborted, "synthetic capture expired or mismatched")
	}
	return page, nil
}
func (h *nhf020Logic) continuation(page *nhf020Page, next, total int) (*logic.Continuation, error) {
	if next == total {
		// Completed captures must release all their cursor slots. Counting each
		// successfully consumed page until TTL can exhaust capacity through
		// ordinary navigation, even with one sequential client and no leak.
		for key, previous := range h.pages {
			if previous.capture == page.capture {
				delete(h.pages, key)
			}
		}
		return nhf020Complete(), nil
	}
	for key, previous := range h.pages {
		if !time.Now().Before(previous.expires) {
			delete(h.pages, key)
		}
	}
	if len(h.pages) >= 32 {
		return nil, nhf020Error(connect.CodeResourceExhausted, "synthetic capture capacity")
	}
	copy := *page
	copy.offset = next
	token := nhf020Opaque()
	h.pages[token] = &copy
	return &logic.Continuation{Result: &logic.Continuation_NextPageToken{NextPageToken: token}}, nil
}
func (h *nhf020Logic) ListDefinitions(ctx context.Context, request *connect.Request[logic.ListDefinitionsRequest]) (*connect.Response[logic.ListDefinitionsResponse], error) {
	if !nhf020Message(request.Msg) {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid request")
	}
	if err := h.authorize(ctx, request.Header()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	page, err := h.page(request.Msg.PageToken, nhf020Definitions, "", request.Msg.PageSize)
	if err != nil {
		return nil, err
	}
	if request.Msg.PageToken == nil {
		for _, value := range h.definitions {
			page.definitions = append(page.definitions, nhf020Clone(value))
		}
	}
	end := min(page.offset+int(request.Msg.PageSize), len(page.definitions))
	next, err := h.continuation(page, end, len(page.definitions))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&logic.ListDefinitionsResponse{Context: nhf020Clone(page.context), Definitions: page.definitions[page.offset:end],
		RetainedCount: uint32(len(page.definitions)), ReportedCount: 72, Continuation: next}), nil
}
func (h *nhf020Logic) ListRuns(ctx context.Context, request *connect.Request[logic.ListRunsRequest]) (*connect.Response[logic.ListRunsResponse], error) {
	if !nhf020Message(request.Msg) || !nhf020ID(request.Msg.DefinitionId) {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid request")
	}
	if err := h.authorize(ctx, request.Header()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.same(request.Msg.Context); err != nil {
		return nil, err
	}
	page, err := h.page(request.Msg.PageToken, nhf020Runs, request.Msg.DefinitionId, request.Msg.PageSize)
	if err != nil {
		return nil, err
	}
	if request.Msg.PageToken == nil {
		for _, run := range h.runs {
			if run.Definition.Id == request.Msg.DefinitionId {
				page.runs = append(page.runs, nhf020Clone(run))
			}
		}
	}
	end := min(page.offset+int(request.Msg.PageSize), len(page.runs))
	next, err := h.continuation(page, end, len(page.runs))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&logic.ListRunsResponse{Context: nhf020Clone(page.context), Runs: page.runs[page.offset:end], Continuation: next}), nil
}
func nhf020Active(run *logic.Run) bool {
	switch run.Status {
	case logic.RunStatus_RUN_STATUS_RUNNING, logic.RunStatus_RUN_STATUS_QUEUED:
		return true
	case logic.RunStatus_RUN_STATUS_COMPLETED, logic.RunStatus_RUN_STATUS_CANCELED, logic.RunStatus_RUN_STATUS_INTERRUPTED, logic.RunStatus_RUN_STATUS_FAILED, logic.RunStatus_RUN_STATUS_UNSPECIFIED:
		return false
	default:
		panic("invalid synthetic run status")
	}
}
func (h *nhf020Logic) find(id nhf020RunID) *logic.Run {
	for _, run := range h.runs {
		if nhf020RunID(run.Id) == id {
			return run
		}
	}
	return nil
}
func (h *nhf020Logic) StartRun(ctx context.Context, request *connect.Request[logic.StartRunRequest]) (*connect.Response[logic.StartRunResponse], error) {
	if !nhf020Message(request.Msg) || !nhf020ID(request.Msg.RequestId) || request.Msg.Definition == nil {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid request")
	}
	if err := h.authorize(ctx, request.Header()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.same(request.Msg.Expected); err != nil {
		return nil, err
	}
	key := nhf020RequestID(request.Msg.RequestId)
	if _, found := h.requests[key]; found {
		return nil, nhf020Error(connect.CodeAlreadyExists, "attempt exists; use GetRun")
	}
	var definition *logic.Definition
	for _, value := range h.definitions {
		if proto.Equal(value.Reference, request.Msg.Definition) {
			definition = value
			break
		}
	}
	if definition == nil || !definition.CanRun {
		return nil, nhf020Error(connect.CodeFailedPrecondition, "definition unavailable or changed")
	}
	if len(h.runs) >= 64 {
		return nil, nhf020Error(connect.CodeResourceExhausted, "synthetic retained run capacity")
	}
	run := &logic.Run{Id: nhf020Opaque(), Definition: nhf020Clone(definition.Reference), Generation: nhf020Clone(h.context.Generation),
		// The public authorization check does not return caller identity. Keep
		// it honestly unrecorded, never infer it from the UI or a token string.
		Actor: &logic.Actor{Observation: &logic.Actor_NotRecorded{NotRecorded: &emptypb.Empty{}}},
		Mode:  definition.Mode, Status: logic.RunStatus_RUN_STATUS_RUNNING, Reason: logic.RunReason_RUN_REASON_EXPLICIT_REQUEST,
		CreatedAt: timestamppb.Now(), StartRequestId: request.Msg.RequestId, CanCancel: true, Simulated: true}
	for _, previous := range h.runs {
		if nhf020DefinitionID(previous.Definition.Id) != nhf020DefinitionID(definition.Reference.Id) || !nhf020Active(previous) {
			continue
		}
		switch definition.Mode {
		case logic.RunMode_RUN_MODE_SINGLE:
			run.Status = logic.RunStatus_RUN_STATUS_FAILED
			run.Reason = logic.RunReason_RUN_REASON_SINGLE_BUSY
			run.CanCancel = false
		case logic.RunMode_RUN_MODE_RESTART:
			previous.Status = logic.RunStatus_RUN_STATUS_CANCELED
			previous.Reason = logic.RunReason_RUN_REASON_RESTART_REPLACED
			previous.CanCancel = false
		case logic.RunMode_RUN_MODE_QUEUED:
			run.Status = logic.RunStatus_RUN_STATUS_QUEUED
			run.Reason = logic.RunReason_RUN_REASON_WAITING_FOR_SLOT
		case logic.RunMode_RUN_MODE_PARALLEL:
		case logic.RunMode_RUN_MODE_UNSPECIFIED:
			return nil, nhf020Error(connect.CodeInternal, "invalid synthetic run mode")
		default:
			return nil, nhf020Error(connect.CodeInternal, "unknown synthetic run mode")
		}
	}
	h.runs = append(h.runs, run)
	h.requests[key] = nhf020RunID(run.Id)
	h.counts.Starts++
	return connect.NewResponse(&logic.StartRunResponse{Context: nhf020Clone(h.context), Run: nhf020Clone(run)}), nil
}
func (h *nhf020Logic) GetRun(ctx context.Context, request *connect.Request[logic.GetRunRequest]) (*connect.Response[logic.GetRunResponse], error) {
	if !nhf020Message(request.Msg) {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid request")
	}
	if err := h.authorize(ctx, request.Header()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.same(request.Msg.Context); err != nil {
		return nil, err
	}
	var id nhf020RunID
	switch selector := request.Msg.Selector.(type) {
	case *logic.GetRunRequest_RunId:
		if !nhf020ID(selector.RunId) {
			return nil, nhf020Error(connect.CodeInvalidArgument, "invalid run")
		}
		id = nhf020RunID(selector.RunId)
	case *logic.GetRunRequest_StartRequestId:
		if !nhf020ID(selector.StartRequestId) {
			return nil, nhf020Error(connect.CodeInvalidArgument, "invalid attempt")
		}
		id = h.requests[nhf020RequestID(selector.StartRequestId)]
	case nil:
		return nil, nhf020Error(connect.CodeInvalidArgument, "missing selector")
	default:
		return nil, nhf020Error(connect.CodeInvalidArgument, "unknown selector")
	}
	run := h.find(id)
	if run == nil {
		return nil, nhf020Error(connect.CodeNotFound, "run not retained; no resubmission")
	}
	h.counts.Lookups++
	return connect.NewResponse(&logic.GetRunResponse{Context: nhf020Clone(h.context), Run: nhf020Clone(run)}), nil
}
func (h *nhf020Logic) CancelRun(ctx context.Context, request *connect.Request[logic.CancelRunRequest]) (*connect.Response[logic.CancelRunResponse], error) {
	if !nhf020Message(request.Msg) || !nhf020ID(request.Msg.RunId) || !nhf020ID(request.Msg.RequestId) {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid request")
	}
	if err := h.authorize(ctx, request.Header()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.same(request.Msg.Expected); err != nil {
		return nil, err
	}
	run := h.find(nhf020RunID(request.Msg.RunId))
	if run == nil {
		return nil, nhf020Error(connect.CodeNotFound, "run unavailable")
	}
	if previous, found := h.cancellations[nhf020RequestID(request.Msg.RequestId)]; found {
		if previous != nhf020RunID(run.Id) {
			return nil, nhf020Error(connect.CodeAlreadyExists, "cancellation bound to another run")
		}
		return nil, nhf020Error(connect.CodeAlreadyExists, "cancellation exists; read run without repeating")
	}
	if len(h.cancellations) >= 64 {
		return nil, nhf020Error(connect.CodeResourceExhausted, "cancellation capacity")
	}
	outcome := logic.Cancellation_CANCELLATION_ALREADY_TERMINAL
	if nhf020Active(run) {
		run.Status = logic.RunStatus_RUN_STATUS_CANCELED
		run.Reason = logic.RunReason_RUN_REASON_EXPLICIT_CANCELLATION
		run.CanCancel = false
		outcome = logic.Cancellation_CANCELLATION_FUTURE_STEPS_STOPPED
		// A queued successor is a server-owned synthetic transition only.
		// No client continuation and no physical dispatch occur here.
		if run.Mode == logic.RunMode_RUN_MODE_QUEUED {
			running := false
			for _, other := range h.runs {
				if other.Definition.Id == run.Definition.Id && other.Status == logic.RunStatus_RUN_STATUS_RUNNING {
					running = true
				}
			}
			if !running {
				for _, other := range h.runs {
					if other.Definition.Id == run.Definition.Id && other.Status == logic.RunStatus_RUN_STATUS_QUEUED {
						other.Status = logic.RunStatus_RUN_STATUS_RUNNING
						other.Reason = logic.RunReason_RUN_REASON_EXPLICIT_REQUEST
						break
					}
				}
			}
		}
	}
	h.cancellations[nhf020RequestID(request.Msg.RequestId)] = nhf020RunID(run.Id)
	h.counts.Cancellations++
	return connect.NewResponse(&logic.CancelRunResponse{Context: nhf020Clone(h.context), Run: nhf020Clone(run), Cancellation: outcome, DispatchedEffectsReversed: false}), nil
}
func nhf020Rows(run *logic.Run) []*logic.TraceRow {
	rows := make([]*logic.TraceRow, 0, 480)
	ordinal := uint64(1)
	for index := 0; index < 480; index++ {
		row := &logic.TraceRow{Ordinal: ordinal, At: nhf020Clone(run.CreatedAt)}
		switch {
		case index == 10:
			row.Event = &logic.TraceRow_Gap{Gap: &logic.Gap{Reason: logic.GapReason_GAP_REASON_EVICTED, KnownMissingCount: 7}}
			ordinal += 6
		case index == 30:
			row.Event = &logic.TraceRow_Command{Command: &logic.Command{CommandId: "synthetic-dispatched-command", Outcome: history.CommandOutcome_COMMAND_OUTCOME_DISPATCHED}}
		case index == 45:
			row.Event = &logic.TraceRow_Gap{Gap: &logic.Gap{Reason: logic.GapReason_GAP_REASON_REDACTED, CountUnknown: true}}
			ordinal += 9
		case index == 479:
			row.Event = &logic.TraceRow_Lifecycle{Lifecycle: run.Reason}
		default:
			node := &logic.Node{Kind: obs.TraceActionKind_TRACE_ACTION_KIND_SEQUENCE, Stage: obs.TraceNodeStage_TRACE_NODE_STAGE_COMPLETED}
			if index%3 == 0 {
				node.Source = &logic.Node_Redacted{Redacted: &emptypb.Empty{}}
			} else {
				node.Source = &logic.Node_Position{Position: &logic.SourcePosition{File: "synthetic-routine.yaml", Path: fmt.Sprintf("steps[%d]", index), Line: uint32(index + 1), Column: 1}}
			}
			row.Event = &logic.TraceRow_Node{Node: node}
		}
		ordinal++
		rows = append(rows, row)
	}
	return rows
}
func (h *nhf020Logic) ReadTrace(ctx context.Context, request *connect.Request[logic.ReadTraceRequest]) (*connect.Response[logic.ReadTraceResponse], error) {
	if !nhf020Message(request.Msg) || !nhf020ID(request.Msg.RunId) {
		return nil, nhf020Error(connect.CodeInvalidArgument, "invalid request")
	}
	if err := h.authorize(ctx, request.Header()); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.same(request.Msg.Context); err != nil {
		return nil, err
	}
	page, err := h.page(request.Msg.PageToken, nhf020Trace, request.Msg.RunId, request.Msg.PageSize)
	if err != nil {
		return nil, err
	}
	if request.Msg.PageToken == nil {
		run := h.find(nhf020RunID(request.Msg.RunId))
		if run == nil {
			return nil, nhf020Error(connect.CodeNotFound, "run unavailable")
		}
		page.run = nhf020Clone(run)
		page.rows = nhf020Rows(page.run)
		page.capture = nhf020Opaque()
	}
	end := min(page.offset+int(request.Msg.PageSize), len(page.rows))
	next, err := h.continuation(page, end, len(page.rows))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&logic.ReadTraceResponse{Context: nhf020Clone(page.context), Run: nhf020Clone(page.run), CaptureId: page.capture, Rows: page.rows[page.offset:end], Continuation: next}), nil
}

func nhf020StartProxy(t *testing.T, out, data, origin string, roots *x509.CertPool, h *nhf020Logic) string {
	t.Helper()
	target, err := url.Parse(origin)
	if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" {
		t.Fatal("isolated TLS upstream required", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	mux := http.NewServeMux()
	path, handler := logicv1connect.NewLogicServiceHandler(h, connect.WithReadMaxBytes(1<<20), connect.WithSendMaxBytes(1<<20))
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logicv1connect.LogicServiceStartRunProcedure {
			handler.ServeHTTP(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join(out, "lose-start-reply"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "synthetic fault unavailable", 500)
			return
		}
		if len(raw) == 0 {
			handler.ServeHTTP(w, r)
			return
		}
		// Only the response disappears. The handler already recorded the fake
		// run. The client must resolve by the SAME request ID without retry.
		recorded := httptest.NewRecorder()
		handler.ServeHTTP(recorded, r)
		if recorded.Code != http.StatusOK {
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = io.Copy(w, recorded.Body)
			return
		}
		h.mu.Lock()
		h.counts.LostReplies++
		h.mu.Unlock()
		panic(http.ErrAbortHandler)
	}))
	mux.Handle("/", proxy)
	server := httptest.NewUnstartedServer(mux)
	identity, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL
}
