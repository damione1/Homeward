package testpg

import (
	"os"
	"path/filepath"
	"testing"
)

// ShortPrivateDataDir provides a private fixture path short enough for
// Darwin's AF_UNIX sockaddr. GOTMPDIR keeps it under Homeward's shared build
// cache in local gates; the user cache is the portable fallback.
func ShortPrivateDataDir(t *testing.T) string {
	t.Helper()
	parent := os.Getenv("GOTMPDIR")
	if parent == "" {
		var err error
		parent, err = os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp(parent, "hw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	if len(filepath.Join(dir, "admin.sock")) > 103 {
		t.Fatal("test data directory exceeds Darwin AF_UNIX path limit")
	}
	return dir
}
