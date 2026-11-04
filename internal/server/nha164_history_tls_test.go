package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	history "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This fixture supplies an authenticated credential at PublicPlan's existing
// lease boundary. Native session/Run integration remains a separate final gate.
// The routes, generated Connect client, TLS, authority, Store and SQL are real.
type nha164TLSCredential struct {
	principal authorization.Principal
	authority *authorization.Authority
}

func (a nha164TLSCredential) Authenticate(context.Context, *http.Request) (domain.ActorID, error) {
	return domain.ActorID{}, authorization.ErrDenied
}
func (a nha164TLSCredential) AuthenticateWithLease(ctx context.Context, r *http.Request) (domain.ActorID, context.Context, func(), error) {
	if r.TLS == nil || r.Header.Get("Authorization") != "Bearer nha164-synthetic" {
		return domain.ActorID{}, nil, nil, authorization.ErrDenied
	}
	actor, err := a.principal.Actor()
	if err != nil {
		return domain.ActorID{}, nil, nil, err
	}
	admitted, err := authorization.WithPrincipal(ctx, a.principal)
	return actor, admitted, func() {}, err
}
func (a nha164TLSCredential) Authorize(ctx context.Context, _ domain.ActorID, key RouteKey) error {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return err
	}
	permission, _, ok := nativeRoutePermission(key)
	if !ok {
		return authorization.ErrDenied
	}
	return a.authority.WithCurrent(ctx, principal, func(current authorization.Current) error { return current.AdmitRoute(permission) })
}
func nha164TLSRequest[T any](value *T) *connect.Request[T] {
	r := connect.NewRequest(value)
	r.Header().Set("Authorization", "Bearer nha164-synthetic")
	return r
}

func TestNHA164GeneratedTLSRoutesAndActualSQLRevocation(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, _, _, _ := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	account, _ := contracts.ParseAccountID("nha164-account")
	source, err := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha164-account\ngrants:\n  - scope: {family: history, operation: read}\n    objects:\n      entities: {all: {capabilities: {all: {}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.native.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, 2)); err != nil {
		t.Fatal(err)
	}
	permissions := make([]authorization.Permission, 0, 2)
	for _, family := range []auth.ScopeFamily{auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION} {
		permission, err := authorization.NewPermission(family, auth.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			t.Fatal(err)
		}
		permissions = append(permissions, permission)
	}
	scopes, err := authorization.NewScopeSet(permissions)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := contracts.ParseMcpKeyID("nha164-tls-fixture")
	principal, err := authorization.ScopedPrincipal(account, key, scopes)
	if err != nil {
		t.Fatal(err)
	}
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha164-tls")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	driver, err := fake.NewDriver(now)
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	_, sourceKey, err := binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("switch.nha164_tls")
	record, err := integrations.NewRegistryRecord(device, entity, instance, sourceKey, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	h.native.runtime.sequence = fixture.Sequencer()
	h.native.runtime.reader = store
	home, err := h.native.runtime.names.HomeName(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.native.runtime.names = nha203HistoryNames{home: home, entities: []domain.EntityID{entity}}
	composite, err := integrations.NewCandidate(sourceKey, []*registry.CapabilityDescription{
		{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic witness A", Endpoint: "relay", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}}},
		{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic contributing B", Endpoint: "power", Constraints: &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{Unit: "W", Precision: 1, Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Discover(context.Background(), []integrations.Candidate{composite}); err != nil {
		t.Fatal(err)
	}
	catalogueCapture, err := fixture.Sequencer().CaptureEntityCatalogue(entity, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	epochText, _, err := catalogueCapture.Cursor().Components()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := storage.ParseRecorderEpoch(epochText)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.OpenRecorderEpoch(context.Background(), epoch, now); err != nil {
		t.Fatal(err)
	}
	rows := make([]storage.RecorderRecord, 0, 2)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stamp, _ := domain.NewSourceStamp("nha164-tls-source", nil)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		value := domain.NewSwitchValue(sequence == 2)
		observed, err := domain.NewObservedStateSnapshot(entity, quality, &value, domain.StateTimes{ReceivedAt: now, LastChanged: now, LastUpdated: now, LastReported: now}, stamp, sequence, nil)
		if err != nil {
			t.Fatal(err)
		}
		row, err := storage.NewRecorderRecord(sequence, instance, observed, domain.Causality{}, now, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := integrations.NewRecorderEvidence(catalogueCapture.Record(), catalogueCapture.Descriptions())
		if err != nil {
			t.Fatal(err)
		}
		row, err = row.WithEvidence(proof)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	batch, err := storage.NewRecorderBatch(epoch, 0, 2, rows, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := store.PersistRecorderBatch(context.Background(), batch); err != nil || outcome != storage.TransactionCommitted {
		t.Fatalf("actual Recorder fixture failed: %v %v", outcome, err)
	}
	traceEpoch, err := storage.ParseHistoryEpoch("00000000000000000000000000000164")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.OpenExecutionHistoryEpoch(context.Background(), traceEpoch); err != nil {
		t.Fatal(err)
	}
	traceID, err := storage.NewHistoryTraceID(traceEpoch, 1)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := storage.NewHistoryRecord(storage.HistoryRecordInput{ID: traceID, EmissionTime: now, Kind: storage.HistoryDecision, Decision: 12, Provenance: storage.HistoryUnboundFixture})
	if err != nil {
		t.Fatal(err)
	}
	traceBatch, err := storage.NewHistoryBatch(traceEpoch, 1, []storage.HistoryRecord{trace}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CommitExecutionHistoryBatch(context.Background(), traceBatch); err != nil {
		t.Fatal(err)
	}
	provider := nha164TLSCredential{principal: principal, authority: h.native.runtime.authority}
	plan, err := NewPublicPlan(provider, provider, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	if err = registerHistoryRoutes(plan, h.native.runtime); err != nil {
		t.Fatal(err)
	}
	public := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, public)
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewUnstartedServer(compiled.handler(public, newPublicAdmissions()))
	tlsServer.EnableHTTP2 = true
	tlsServer.StartTLS()
	defer tlsServer.Close()
	api := apiv2connect.NewHistoryServiceClient(tlsServer.Client(), tlsServer.URL)
	t.Run("malformed-JSON-errors-are-bounded-and-redacted", func(t *testing.T) {
		const canary = "NHA164_UNTRUSTED_JSON_KEY_CANARY"
		key := canary + strings.Repeat("k", 300<<10)
		for _, body := range []string{"{\"" + key + "\":true}", "{\"entityId\": [}"} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, tlsServer.URL+apiv2connect.HistoryServiceQueryObservationsProcedure, strings.NewReader(body))
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer nha164-synthetic")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Connect-Protocol-Version", "1")
			response, err := tlsServer.Client().Do(request)
			if err != nil {
				cancel()
				t.Fatal("malformed JSON request failed before response", err)
			}
			wire, readErr := io.ReadAll(io.LimitReader(response.Body, historyquery.MaxResponseBytes+1))
			response.Body.Close()
			cancel()
			if readErr != nil || response.StatusCode != http.StatusBadRequest || len(wire) > historyquery.MaxResponseBytes || strings.Contains(string(wire), canary) || strings.Contains(string(wire), key) {
				t.Fatalf("decoder error escaped bound/redaction: status=%d bytes=%d readError=%v", response.StatusCode, len(wire), readErr)
			}
			var rejected struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(wire, &rejected) != nil || rejected.Code != "invalid_argument" {
				t.Fatal("decoder error lost InvalidArgument")
			}
		}
	})
	period := &history.HistoryPeriod{Start: timestamppb.New(now.Add(-time.Hour)), End: timestamppb.New(now.Add(time.Hour))}
	publicName, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil {
		t.Fatal(err)
	}
	name := publicName.String()
	first, err := api.QueryObservations(context.Background(), nha164TLSRequest(&history.QueryObservationsRequest{Parent: name, Period: period, PageSize: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if contracts.AdmitHistoryV2Response(first.Msg, home) != nil || first.Msg.Scope == nil || len(first.Msg.Observations) != 1 || first.Msg.Observations[0].State.GetSwitchOn() {
		t.Fatal("actual TLS lost false or page coverage")
	}
	token := first.Msg.NextPageToken
	if token == "" {
		t.Fatal("actual TLS did not return keyset token")
	}
	second, err := api.QueryObservations(context.Background(), nha164TLSRequest(&history.QueryObservationsRequest{Parent: name, Period: period, PageSize: 2, PageToken: token}))
	if err != nil || second.Msg.Scope == nil || len(second.Msg.Observations) != 1 || !second.Msg.Observations[0].State.GetSwitchOn() {
		t.Fatalf("actual TLS continuation failed: %v", err)
	}
	hour := now.Truncate(time.Hour)
	aggregate, err := api.QueryAggregates(context.Background(), nha164TLSRequest(&history.QueryAggregatesRequest{Parent: name, Period: &history.HistoryPeriod{Start: timestamppb.New(hour), End: timestamppb.New(hour.Add(time.Hour))}}))
	if err != nil || aggregate.Msg.Scope == nil || contracts.AdmitHistoryV2Response(aggregate.Msg, home) != nil {
		t.Fatalf("actual aggregate route not admitted: %v", err)
	}
	traces, err := api.QueryExecutionTraces(context.Background(), nha164TLSRequest(&history.QueryExecutionTracesRequest{Parent: home.String(), Period: period}))
	if err != nil || traces.Msg.Scope == nil || len(traces.Msg.ExecutionTraces) != 1 || contracts.AdmitHistoryV2Response(traces.Msg, home) != nil {
		t.Fatalf("actual persisted trace route failed: %v", err)
	}
	t.Run("recorded-B-revoked-during-H2-send-with-witness-A-maintained", func(t *testing.T) {
		roots := x509.NewCertPool()
		roots.AddCert(tlsServer.Certificate())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		started := time.Now()
		blocked := nha203UnreadHistoryH2(t, ctx, tlsServer.URL, roots, apiv2connect.HistoryServiceQueryObservationsProcedure, &history.QueryObservationsRequest{Parent: name, Period: period, PageSize: 1})
		if time.Since(started) > time.Second {
			t.Fatal("fixture reached send too late to distinguish revocation from 2s deadline")
		}
		independent, err := api.QueryObservations(ctx, nha164TLSRequest(&history.QueryObservationsRequest{Parent: name, Period: period, PageSize: 1}))
		if err != nil || independent == nil {
			t.Fatal("blocked send stalled independent reader", err)
		}
		denied, err := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha164-account\ngrants:\n  - scope: {family: history, operation: read}\n    objects:\n      entities:\n        all:\n          capabilities:\n            explicit:\n              - {schema_id: core.switch.v1, endpoint: relay}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err = h.native.runtime.authority.Publish(nha164AuthorityState(t, denied, auth.AccountRole_ACCOUNT_ROLE_ADMIN, 3)); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(500 * time.Millisecond)
		for blocked.State().StreamsActive != 0 {
			if time.Now().After(until) {
				t.Fatal("withdrawal failed to interrupt unread History send")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if time.Since(started) >= historyquery.QueryTimeout {
			t.Fatal("deadline cannot stand in for revocation proof")
		}
		if err = h.native.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, 4)); err != nil {
			t.Fatal(err)
		}
	})
	blocker := nha030ServerObserver(t, cluster.Port)
	tx, err := blocker.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `LOCK TABLE public.execution_history_records IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err = tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	// Let the server's own two-second budget expire while real SQL waits.
	// The typed detail distinguishes this from a client-side cancellation.
	timeoutResponse, timeoutErr := api.QueryExecutionTraces(context.Background(), nha164TLSRequest(&history.QueryExecutionTracesRequest{Parent: home.String(), Period: period}))
	if timeoutResponse != nil || connect.CodeOf(timeoutErr) != connect.CodeDeadlineExceeded {
		t.Fatalf("actual blocked SQL deadline became denial: %v", timeoutErr)
	}
	var typedTimeout *connect.Error
	if !errors.As(timeoutErr, &typedTimeout) {
		t.Fatal("missing typed Connect deadline")
	}
	deadlineDetail := false
	for _, detail := range typedTimeout.Details() {
		value, err := detail.Value()
		if err != nil {
			t.Fatal(err)
		}
		if rejected, ok := value.(*errdetails.ErrorInfo); ok && rejected.Reason == "REQUEST_DEADLINE_EXCEEDED" {
			deadlineDetail = true
		}
	}
	if !deadlineDetail {
		t.Fatal("server deadline lost its canonical error reason")
	}
	returned := make(chan error, 1)
	go func() {
		response, err := api.QueryExecutionTraces(context.Background(), nha164TLSRequest(&history.QueryExecutionTracesRequest{Parent: home.String(), Period: period}))
		if response != nil {
			err = errors.New("revoked SQL data escaped TLS")
		}
		returned <- err
	}()
	nha162WaitSQLBlockedBy(t, context.Background(), store, blockerPID, "%FROM public.execution_history_records%")
	if err = h.native.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_USER, 5)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-returned:
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("actual SQL result escaped current authority fence: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("actual TLS SQL reader did not join")
	}
}
