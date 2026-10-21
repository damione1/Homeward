package server

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
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
	token := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, base), "NHA056 trace admin", password).AccessToken
	reader := observabilityv1connect.NewObservabilityServiceClient(client, base)
	first, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, token))
	if err != nil || contracts.AdmitObservabilityResponse(first.Msg) != nil {
		t.Fatal("actual controller metrics unavailable", err)
	}
	handle := first.Msg.Metrics.Runs.GetInstalled().GetController()
	if handle == nil || len(handle.Value) != 32 {
		t.Fatal("client did not receive the actual Controller identity")
	}
	trace, err := reader.ReadRunTrace(ctx, nha056SessionRequest(&obs.ReadRunTraceRequest{Controller: handle, RunId: uint64(actual.ID())}, token))
	if err != nil || contracts.AdmitObservabilityResponse(trace.Msg) != nil || trace.Msg.GetPage() == nil || len(trace.Msg.GetPage().Rows) == 0 {
		t.Fatal("published Controller identity did not address its actual native trace", err)
	}
	second, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, token))
	if err != nil || !proto.Equal(handle, second.Msg.Metrics.Runs.GetInstalled().Controller) {
		t.Fatal("Controller handle changed without replacement", err)
	}
	changed := proto.Clone(handle).(*obs.ControllerHandle)
	changed.Value[31] ^= 1
	stale, err := reader.ReadRunTrace(ctx, nha056SessionRequest(&obs.ReadRunTraceRequest{Controller: changed, RunId: uint64(actual.ID())}, token))
	if err != nil || stale.Msg.GetStaleCursor() == nil {
		t.Fatal("another Controller identity disclosed a trace", err)
	}
	absent, err := reader.ReadRunTrace(ctx, nha056SessionRequest(&obs.ReadRunTraceRequest{Controller: handle, RunId: 999999}, token))
	if err != nil || absent.Msg.GetNotRetained() == nil {
		t.Fatal("run outside native retention was presented as durable history", err)
	}
	if !bytes.Equal(handle.Value[:16], first.Msg.Metrics.ProcessEpoch.Value) {
		t.Fatal("Controller identity escaped its real process context")
	}
}
