package server

import (
	"encoding/json"
	"net/http"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
)

// packageOperatorHealth runs only behind the existing same-UID Unix listener.
// It is not registered on the public transport and grants no RPC authority.
func packageOperatorHealth(next http.Handler, cells ...*observability.HealthCell) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || len(cells) != 1 || cells[0] == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		facts, err := cells[0].Snapshot()
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		server, serverErr := facts.Server()
		durable, durableErr := facts.Durable()
		if serverErr != nil || durableErr != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ready := server.Readiness == obs.ServerReadiness_SERVER_READINESS_READY
		degraded := durable.Availability != obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE &&
			durable.Availability != obs.DurableAvailability_DURABLE_AVAILABILITY_NOT_CONFIGURED
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Live     bool `json:"live"`
			Ready    bool `json:"ready"`
			Degraded bool `json:"degraded"`
		}{true, ready, degraded})
	})
}
