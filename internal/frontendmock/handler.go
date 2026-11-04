// Package frontendmock is an opt-in development fixture. Production composition
// never imports it; domain-specific mock services are added by their NHF tickets.
package frontendmock

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"
	obsv1 "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type healthFixture struct {
	apiv2connect.UnimplementedObservabilityServiceHandler
	epoch [16]byte
}

// Handler mounts only synthetic, capability-free diagnostics, with an explicit
// mode marker on every response. It has no provider, storage or device handle.
func Handler(epoch [16]byte) http.Handler {
	mux := http.NewServeMux()
	path, handler := apiv2connect.NewObservabilityServiceHandler(&healthFixture{epoch: epoch})
	mux.Handle(path, handler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Homeward-Development-Mode", "mock")
		mux.ServeHTTP(w, r)
	})
}

func (h *healthFixture) GetHealth(context.Context, *connect.Request[obsv1.GetHealthRequest]) (*connect.Response[obsv1.Health], error) {
	pilots := make([]*obsv1.ObservationPilotPhaseCount, 0, 5)
	for _, phase := range []obsv1.ObservationPilotPhase{obsv1.ObservationPilotPhase_OBSERVATION_PILOT_PHASE_STARTING, obsv1.ObservationPilotPhase_OBSERVATION_PILOT_PHASE_RUNNING, obsv1.ObservationPilotPhase_OBSERVATION_PILOT_PHASE_RETRYING, obsv1.ObservationPilotPhase_OBSERVATION_PILOT_PHASE_DEGRADED, obsv1.ObservationPilotPhase_OBSERVATION_PILOT_PHASE_STOPPED} {
		pilots = append(pilots, &obsv1.ObservationPilotPhaseCount{Phase: phase})
	}
	return connect.NewResponse(&obsv1.Health{Name: "homes/synthetic/health",
		ProcessEpoch: &obsv1.ObservationProcessObservationEpoch{Value: h.epoch[:]},
		Liveness:     obsv1.ObservationLiveness_OBSERVATION_LIVENESS_LIVE,
		Server: &obsv1.ObservationServerHealth{Readiness: obsv1.ObservationServerReadiness_OBSERVATION_SERVER_READINESS_CORE_UNAVAILABLE,
			Generation: &obsv1.ObservationGenerationPresence{Result: &obsv1.ObservationGenerationPresence_NotLoaded{NotLoaded: &emptypb.Empty{}}},
			Causes:     []obsv1.ObservationCoreCause{obsv1.ObservationCoreCause_OBSERVATION_CORE_CAUSE_GENERATION_NOT_LOADED}},
		Durable: &obsv1.ObservationDurableHealth{Availability: obsv1.ObservationDurableAvailability_OBSERVATION_DURABLE_AVAILABILITY_NOT_CONFIGURED,
			Evidence: &obsv1.ObservationDurableHealth_ObservedAt{ObservedAt: timestamppb.New(time.Unix(0, 0))}},
		Recorder: &obsv1.ObservationRecorderHealth{Result: &obsv1.ObservationRecorderHealth_NotInstalled{NotInstalled: &emptypb.Empty{}}},
		Pilots:   pilots,
	}), nil
}
