package server

import (
	"testing"

	"github.com/damione1/Homeward/internal/testpg"
)

// AF_UNIX has a short sockaddr path on macOS. Keep integration fixtures in a
// private short directory, under the shared build cache when configured.
func shortPrivateDataDir(t *testing.T) string {
	t.Helper()
	return testpg.ShortPrivateDataDir(t)
}
