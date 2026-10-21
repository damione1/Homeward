package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

type nha056ClosingListener struct {
	net.Listener
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	allow   sync.Once
}

func (l *nha056ClosingListener) Close() error {
	l.once.Do(func() { close(l.entered) })
	<-l.release
	return l.Listener.Close()
}
func (l *nha056ClosingListener) unblock() { l.allow.Do(func() { close(l.release) }) }

// This seam retains the real composed owners; it does not replace a driver,
// generation, admission gate, listener handler, or observability response.
func nha056ObservedDefaultRun(t *testing.T, cfg config.Config) (*nha162Running, *observationRuntime, *nha056ClosingListener) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	run := &nha162Running{stop: stop, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	owners := make(chan *observationRuntime, 1)
	listener := &nha056ClosingListener{entered: make(chan struct{}), release: make(chan struct{})}
	listen := func(ctx context.Context, network, address string) (net.Listener, error) {
		inner, err := (&net.ListenConfig{}).Listen(ctx, network, address)
		if err != nil {
			return nil, err
		}
		listener.Listener = inner
		return listener, nil
	}
	go func() {
		run.done <- runWithDependencies(ctx, cfg, listen, func(addr net.Addr) { ready <- addr }, nil, runDependencies{observeRuntime: func(runtime *observationRuntime) { owners <- runtime }})
	}()
	t.Cleanup(func() {
		listener.unblock()
		if !run.joined {
			if err := run.join(t); err != nil {
				t.Error(err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("observed Run failed before listener", err)
	case <-time.After(12 * time.Second):
		t.Fatal("observed Run listener absent")
	}
	return run, <-owners, listener
}

func TestNHA056RealRunCloudFailuresKeepReadyAndCoreDrainsBeforeListenerClose(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	revision := nha056ActivateLocalGeneration(t, cfg)
	run, runtime, listener := nha056ObservedDefaultRun(t, cfg)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 completion synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 completion admin", password)
	token := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, base), "NHA056 completion admin", password).AccessToken
	reader := observabilityv1connect.NewObservabilityServiceClient(client, base)
	assertReady := func(t *testing.T) {
		t.Helper()
		response, err := reader.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, token))
		if err != nil || contracts.AdmitObservabilityResponse(response.Msg) != nil || response.Msg.Health.Liveness != obs.Liveness_LIVENESS_LIVE || response.Msg.Health.Server.Readiness != obs.ServerReadiness_SERVER_READINESS_READY || response.Msg.Health.Server.Generation.GetPresent().GetValue() != revision {
			t.Fatal("cloud failure changed actual Run readiness", err)
		}
	}
	assertReady(t)
	for _, mode := range []string{"429", "timeout", "closed-socket"} {
		t.Run(mode, func(t *testing.T) {
			instance, err := domain.ParseIntegrationInstanceID("nha056-cloud-" + mode)
			if err != nil {
				t.Fatal(err)
			}
			session, err := runtime.host.OpenSession(instance)
			if err != nil {
				t.Fatal(err)
			}
			input := integrations.DefaultCloudLimits().Input()
			input.ObservationAttempts, input.AttemptTimeout = 1, 100*time.Millisecond
			limits, err := integrations.AdmitCloudLimits(input)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := integrations.NewCloudPolicy(limits, nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
			if err != nil {
				t.Fatal(err)
			}
			if err := policy.AttachObservation(runtime.cells); err != nil {
				t.Fatal(err)
			}
			cloud, err := integrations.NewCloudClient(session, policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cloud.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "429":
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, "NHA056_CLOUD_SECRET_CANARY")
				case "timeout":
					<-r.Context().Done()
				case "closed-socket":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}
			}))
			defer upstream.Close()
			request, err := integrations.NewCloudRequest(http.MethodGet, upstream.URL+"/private?token=NHA056_CLOUD_SECRET_CANARY", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cloud.Observe(ctx, request); err == nil {
				t.Fatal("synthetic cloud failure succeeded")
			}
			health, err := reader.GetIntegrationHealth(ctx, nha056SessionRequest(&obs.GetIntegrationHealthRequest{IntegrationInstanceId: "nha056-cloud-" + mode}, token))
			if err != nil || health.Msg.Report.GetPresent().GetStatus() != wire.HealthStatus_HEALTH_STATUS_DEGRADED {
				t.Fatal("actual cloud capacity not degraded", err)
			}
			assertReady(t)
		})
	}
	if err := runtime.host.CloseAdmissions(); err != nil {
		t.Fatal(err)
	}
	unavailable, err := reader.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, token))
	if err != nil || unavailable.Msg.Health.Server.Readiness != obs.ServerReadiness_SERVER_READINESS_CORE_UNAVAILABLE || unavailable.Msg.Health.Liveness != obs.Liveness_LIVENESS_LIVE {
		t.Fatal("actual closed core reported Ready", err)
	}
	run.stop()
	select {
	case <-listener.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("listener close was not reached")
	}
	draining, err := runtime.healthSnapshot()
	if err != nil || draining.Health.Server.Readiness != obs.ServerReadiness_SERVER_READINESS_DRAINING || draining.Health.Server.Generation.GetPresent().GetValue() != revision {
		t.Fatal("listener closed before Draining was published", err)
	}
	listener.unblock()
	if err := run.join(t); err != nil {
		t.Fatal(err)
	}
}

func TestNHA056RealNativeReaderRejectsRevokedAndExpired(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	run, runtime, _ := nha056ObservedDefaultRun(t, cfg)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 native synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 native admin", password)
	api := authv1connect.NewAuthServiceClient(client, base)
	owner := nha162RunLogin(t, ctx, api, "NHA056 native admin", password)
	reader := observabilityv1connect.NewObservabilityServiceClient(client, base)
	if _, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, owner.AccessToken)); err != nil {
		t.Fatal("active native session rejected", err)
	}
	if _, err := api.RevokeSession(ctx, nha056SessionRequest(&auth.RevokeSessionRequest{SessionId: owner.Session.Id}, owner.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, owner.AccessToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked native session admitted", err)
	}
	expired := nha162RunLogin(t, ctx, api, "NHA056 native admin", password)
	if err := runtime.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.auth_access_credentials SET expires_at=now()-interval '1 second' WHERE session_id=$1`, expired.Session.Id.Value)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, expired.AccessToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("expired access credential admitted", err)
	}
	// MCP bearer issuance/validation (145) is absent from this base. The
	// admitted scoped-principal ceiling is covered by the separate service
	// permission test; no fictitious MCP credential is presented here.
}

func TestNHA056RealNativeTLSRejectsOpaqueBearerBeforeReaderAdmission(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	run := nha162StartDefaultRun(t, cfg)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 bearer boundary synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 bearer boundary admin", password)
	owner := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, base), "NHA056 bearer boundary admin", password)
	reader := observabilityv1connect.NewObservabilityServiceClient(client, base)
	if response, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, owner.AccessToken)); err != nil || response == nil {
		t.Fatal("native access-token positive control did not reach its reader", err)
	}
	// This controlled opaque value was never issued as a native access token.
	// It is not a claimed MCP credential: no 145 issuer or key validation is
	// supplied by this fixture. ScopedPrincipal ceilings remain tested separately.
	const opaqueBearer = "NHA056_CONTROLLED_NON_NATIVE_OPAQUE_BEARER"
	if response, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, opaqueBearer)); response != nil || connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("opaque non-native bearer obtained a metrics response", err)
	}
	// Admission of this body occurs first inside QueryAudits, before any SQL
	// reader. The native positive control must reach that invalid-argument
	// oracle, while the opaque bearer must be rejected by the common TLS guard
	// before it reaches the same handler admission.
	zero := uint32(0)
	invalidRead := func() *obs.QueryAuditsRequest {
		return &obs.QueryAuditsRequest{Source: obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION, PageSize: &zero}
	}
	if response, err := reader.QueryAudits(ctx, nha056SessionRequest(invalidRead(), owner.AccessToken)); response != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("native audit positive control did not reach reader admission", err)
	}
	if response, err := reader.QueryAudits(ctx, nha056SessionRequest(invalidRead(), opaqueBearer)); response != nil || connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("opaque bearer reached audit reader admission", err)
	}
}

func TestNHA056ActualOperatorErrorLogDiscardsPanicBytes(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	// Retain the actual Run store/coordinator while exercising a second local
	// operator listener in another private directory, with physical I/O absent.
	run, runtime, _ := nha056ObservedDefaultRun(t, cfg)
	// A plaintext request on the real TLS listener forces net/http's actual
	// public ErrorLog path; the read completes only after the diagnostic.
	connection, err := net.DialTimeout("tcp", run.address.String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(connection, "GET /NHA056_PUBLIC_SIGNED_URI_CANARY HTTP/1.1\r\nHost: localhost\r\n\r\n")
	_, _ = io.Copy(io.Discard, connection)
	_ = connection.Close()
	coordinator, err := newSessionCoordinator(runtime.store)
	if err != nil {
		t.Fatal(err)
	}
	operatorData := shortPrivateDataDir(t)
	operator, err := startOperatorListenerObserved(context.Background(), operatorData, runtime.store, coordinator, runtime.journal)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := operator.stop(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	const canary = "NHA056_OPERATOR_PASSWORD_SIGNED_URI_PANIC_CANARY"
	// No connection is admitted before replacing this synthetic handler.
	operator.server.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(canary) })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(operatorData, operatorSocketName))
	}}
	defer transport.CloseIdleConnections()
	response, requestErr := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get("http://operator/panic")
	if response != nil {
		response.Body.Close()
	}
	if requestErr == nil {
		t.Fatal("synthetic panic did not reach net/http")
	}
	snapshot, err := runtime.journal.Snapshot()
	if err != nil || snapshot.Through() < 2 {
		t.Fatal("actual operator ErrorLog did not publish a diagnostic", err)
	}
	var digest [32]byte
	digest[0] = 1
	binding, err := observability.NewReadBinding(1, digest)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := runtime.codec.BeginLogs(binding, snapshot.Through())
	if err != nil {
		t.Fatal(err)
	}
	size, _ := observability.NewPageSize(100)
	request, _ := observability.NewLogPageRequest(size, cursor)
	page, err := runtime.journal.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := page.Wire("")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := protojson.Marshal(logs)
	if err != nil || bytes.Contains(encoded, []byte(canary)) {
		t.Fatal("operator panic escaped real ErrorLog", err)
	}
	operatorPositive, publicPositive := false, false
	for _, row := range logs.GetPage().Rows {
		if row.Event.Code != obs.LogCode_LOG_CODE_TRANSPORT_INTERNAL_DIAGNOSTIC {
			continue
		}
		if row.Event.Component == obs.LogComponent_LOG_COMPONENT_OPERATOR_TRANSPORT {
			operatorPositive = true
		}
		if row.Event.Component == obs.LogComponent_LOG_COMPONENT_PUBLIC_TRANSPORT {
			publicPositive = true
		}
	}
	if !operatorPositive || !publicPositive || bytes.Contains(encoded, []byte("NHA056_PUBLIC_SIGNED_URI_CANARY")) {
		t.Fatal("actual transport safe diagnostics absent or unexpurgated")
	}
}
