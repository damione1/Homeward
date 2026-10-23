// Package frontendmock is an opt-in development fixture. Production composition
// never imports it; domain-specific mock services are added by their NHF tickets.
package frontendmock

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"
	obsv1 "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type healthFixture struct {
	observabilityv1connect.UnimplementedObservabilityServiceHandler
	epoch [16]byte
}

// Handler mounts only synthetic, capability-free diagnostics, with an explicit
// mode marker on every response. It has no provider, storage or device handle.
func Handler(epoch [16]byte) http.Handler {
	mux := http.NewServeMux()
	path, handler := observabilityv1connect.NewObservabilityServiceHandler(&healthFixture{epoch: epoch})
	mux.Handle(path, handler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Homeward-Development-Mode", "mock")
		mux.ServeHTTP(w, r)
	})
}

func (h *healthFixture) GetHealth(context.Context, *connect.Request[obsv1.GetHealthRequest]) (*connect.Response[obsv1.GetHealthResponse], error) {
	pilots := make([]*obsv1.PilotPhaseCount, 0, 5)
	for _, phase := range []obsv1.PilotPhase{obsv1.PilotPhase_PILOT_PHASE_STARTING, obsv1.PilotPhase_PILOT_PHASE_RUNNING, obsv1.PilotPhase_PILOT_PHASE_RETRYING, obsv1.PilotPhase_PILOT_PHASE_DEGRADED, obsv1.PilotPhase_PILOT_PHASE_STOPPED} {
		pilots = append(pilots, &obsv1.PilotPhaseCount{Phase: phase})
	}
	return connect.NewResponse(&obsv1.GetHealthResponse{Health: &obsv1.HealthSnapshot{
		ProcessEpoch: &obsv1.ProcessObservationEpoch{Value: h.epoch[:]},
		Liveness:     obsv1.Liveness_LIVENESS_LIVE,
		Server: &obsv1.ServerHealth{Readiness: obsv1.ServerReadiness_SERVER_READINESS_CORE_UNAVAILABLE,
			Generation: &obsv1.GenerationPresence{Result: &obsv1.GenerationPresence_NotLoaded{NotLoaded: &emptypb.Empty{}}},
			Causes:     []obsv1.CoreCause{obsv1.CoreCause_CORE_CAUSE_GENERATION_NOT_LOADED}},
		Durable: &obsv1.DurableHealth{Availability: obsv1.DurableAvailability_DURABLE_AVAILABILITY_NOT_CONFIGURED,
			Evidence: &obsv1.DurableHealth_ObservedAt{ObservedAt: timestamppb.New(time.Unix(0, 0))}},
		Recorder: &obsv1.RecorderHealth{Result: &obsv1.RecorderHealth_NotInstalled{NotInstalled: &emptypb.Empty{}}},
		Pilots:   pilots,
	}}), nil
}
