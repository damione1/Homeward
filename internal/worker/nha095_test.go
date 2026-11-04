package worker

import (
	"testing"
	"time"
)

// Exact current version admission is a worker-only transport contract. The shared SDK
// scenarios exercise Host behavior without inventing a built-in handshake.
func TestNHA095WorkerCurrentContract(t *testing.T) {
	t.Run("current-host-and-driver-exact-digest", func(t *testing.T) {
		good := startWorkerRig(t, "normal")
		waitMarker(t, good.marker, "handshake_ok", 6*time.Second)
	})
	for _, mode := range []string{"incompatible", "incompatible_host_major", "incompatible_host_minor", "incompatible_driver_major", "incompatible_digest", "incompatible_version_pair"} {
		t.Run(mode, func(t *testing.T) {
			bad := startWorkerRig(t, mode)
			waitMarker(t, bad.marker, "handshake_rejected:FAILED_PRECONDITION", 6*time.Second)
		})
	}
}
