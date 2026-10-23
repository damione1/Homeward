package frontendmock

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	obsv1 "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
)

func TestSyntheticDiagnosticsOverTLS(t *testing.T) {
	epoch := [16]byte{1}
	server := httptest.NewTLSServer(Handler(epoch))
	defer server.Close()
	client := observabilityv1connect.NewObservabilityServiceClient(server.Client(), server.URL)
	response, err := client.GetHealth(context.Background(), connect.NewRequest(&obsv1.GetHealthRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	h := response.Msg.Health
	if response.Header().Get("Homeward-Development-Mode") != "mock" || h == nil || len(h.ProcessEpoch.Value) != 16 || h.ProcessEpoch.Value[0] != 1 {
		t.Fatal("missing isolated mock identity or mode")
	}
	if h.Server.Readiness != obsv1.ServerReadiness_SERVER_READINESS_CORE_UNAVAILABLE || h.Server.Generation.GetNotLoaded() == nil || h.Durable.Availability != obsv1.DurableAvailability_DURABLE_AVAILABILITY_NOT_CONFIGURED || h.Recorder.GetNotInstalled() == nil || len(h.Pilots) != 5 {
		t.Fatal("fixture claims an installed production runtime")
	}
	for _, phase := range h.Pilots {
		if phase.Instances != 0 {
			t.Fatal("mock must not connect to providers")
		}
	}
}
