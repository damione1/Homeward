package server

import (
	"bytes"
	"connectrpc.com/connect"
	"context"
	obsv2 "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA056TLSClientReusesPublishedActualControllerHandle(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	triggerPlan, runPlan := nha041ServerPlans(t, "single")
	entered := make(chan automationruns.Run, 1)
	composition, err := NewAutomationRunsComposition(AutomationRunsInput{Plan: runPlan, Preparer: nha041ServerPreparer{}, Executor: nha041ServerExecutor(func(ctx context.Context, _ ir.Automation, run automationruns.Run) error {
		select {
		case entered <- run:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	})})
	if err != nil {
		t.Fatal(err)
	}
	factory := RuntimeFactory(func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, AutomationTriggers: &triggerPlan, AutomationRuns: composition}, nil
	})
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() {
		done <- RunWithStartupFactory(runCtx, cfg.startup, (&net.ListenConfig{}).Listen, func(addr net.Addr) { ready <- addr }, factory)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("actual trace runtime did not join")
		}
	})
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal("actual trace runtime failed before ready", err)
	case <-time.After(12 * time.Second):
		t.Fatal("actual trace listener did not start")
	}
	var actual automationruns.Run
	select {
	case actual = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual trigger did not reach synthetic executor")
	}
	client := nha162RunClient(t, roots, address)
	base := "https://" + address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 trace synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 trace admin", password)
	token := nha162RunLogin(t, ctx, apiv2connect.NewAuthServiceClient(client, base), "NHA056 trace admin", password).AccessToken
	reader := apiv2connect.NewObservabilityServiceClient(client, base)
	home := nha203ObservationHome(t, ctx, client, base, token)
	first, err := reader.GetMetrics(ctx, nha056SessionRequest(&obsv2.GetMetricsRequest{Name: home + "/metrics"}, token))
	if err != nil || contracts.AdmitObservationResponse(first.Msg) != nil {
		t.Fatal("actual controller metrics unavailable", err)
	}
	handle := first.Msg.Runs.GetInstalled().GetController()
	controller, parseErr := apitypes.ParseRuntimeControllerName(handle)
	if parseErr != nil {
		t.Fatal("client did not receive the actual Controller identity")
	}
	trace, err := reader.QueryRunTrace(ctx, nha056SessionRequest(&obsv2.QueryRunTraceRequest{Parent: nha203ObservationRun(t, handle, uint64(actual.ID()))}, token))
	if err != nil || contracts.AdmitObservationResponse(trace.Msg) != nil || trace.Msg == nil || len(trace.Msg.Rows) == 0 {
		t.Fatal("published Controller identity did not address its actual native trace", err)
	}
	second, err := reader.GetMetrics(ctx, nha056SessionRequest(&obsv2.GetMetricsRequest{Name: home + "/metrics"}, token))
	if err != nil || handle != second.Msg.Runs.GetInstalled().Controller {
		t.Fatal("Controller handle changed without replacement", err)
	}
	identity := controller.Identity()
	changedIdentity := identity
	changedIdentity[31] ^= 1
	changedName, err := apitypes.RuntimeControllerNameForIdentity(controller.Home(), changedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	changed := changedName.String()
	stale, err := reader.QueryRunTrace(ctx, nha056SessionRequest(&obsv2.QueryRunTraceRequest{Parent: nha203ObservationRun(t, changed, uint64(actual.ID()))}, token))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || stale != nil {
		t.Fatal("another Controller identity disclosed a trace", err)
	}
	absent, err := reader.QueryRunTrace(ctx, nha056SessionRequest(&obsv2.QueryRunTraceRequest{Parent: nha203ObservationRun(t, handle, 999999)}, token))
	if connect.CodeOf(err) != connect.CodeNotFound || absent != nil {
		t.Fatal("run outside native retention was presented as durable history", err)
	}
	if !bytes.Equal(identity[:16], first.Msg.ProcessEpoch.Value) {
		t.Fatal("Controller identity escaped its real process context")
	}
}
