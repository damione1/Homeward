package worker

import (
	"testing"
	"time"
)

// Version negotiation is a worker-only transport contract. The shared SDK
// scenarios exercise Host behavior without inventing a built-in handshake.
func TestNHA095WorkerVersionMatrix(t *testing.T) {
	t.Run("host-and-driver-1.0-exact-digest", func(t *testing.T) {
		good := startWorkerRig(t, "normal")
		waitMarker(t, good.marker, "handshake_ok", 6*time.Second)
	})
	t.Run("host-and-driver-1.1-exact-digest", func(t *testing.T) {
		good := startWorkerRig(t, "new")
		waitMarker(t, good.marker, "handshake_ok", 6*time.Second)
	})
	for _, mode := range []string{"incompatible", "incompatible_host_major", "incompatible_host_minor", "incompatible_driver_major", "incompatible_digest", "incompatible_version_pair", "incompatible_new_digest"} {
		t.Run(mode, func(t *testing.T) {
			bad := startWorkerRig(t, mode)
			waitMarker(t, bad.marker, "handshake_rejected:FAILED_PRECONDITION", 6*time.Second)
		})
	}
}
