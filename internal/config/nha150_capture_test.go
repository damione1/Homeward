package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestNHA150CaptureRejectsNonregularAndSymlinkWithoutReadingCanary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configuration.yaml")
	canary := filepath.Join(dir, "private-canary.yaml")
	if err := os.WriteFile(canary, []byte("NHA150_PRIVATE_CANARY"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"symlink", "fifo", "directory", "empty", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			_ = os.Remove(path)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(canary, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "empty":
				err = os.WriteFile(path, nil, 0600)
			case "oversized":
				err = os.WriteFile(path, bytes.Repeat([]byte{'x'}, maxStartupSourceBytes+1), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			answer := make(chan bool, 1)
			go func() { raw, err := readStartupFile(path, false); answer <- err != nil && len(raw) == 0 }()
			select {
			case refused := <-answer:
				if !refused {
					t.Fatal("unsafe source was read")
				}
			case <-time.After(time.Second):
				t.Fatal("nonregular source blocked the capture")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNHA150CaptureConcurrentReplacementNeverReturnsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configuration.yaml")
	canary := filepath.Join(dir, "canary")
	base := []byte("external_workers: []\n")
	if err := os.WriteFile(path, base, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canary, []byte("NHA150_PRIVATE_CANARY"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	joined := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(stop) }); <-joined }
	defer finish()
	go func() {
		defer close(joined)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(path)
			_ = os.Symlink(canary, path)
			_ = os.Remove(path)
			_ = syscall.Mkfifo(path, 0600)
			_ = os.Remove(path)
			_ = os.WriteFile(path, base, 0600)
		}
	}()
	answer := make(chan bool, 1)
	go func() {
		for i := 0; i < 200; i++ {
			raw, err := readStartupFile(path, false)
			if err == nil && !bytes.Equal(raw, base) {
				answer <- false
				return
			}
		}
		answer <- true
	}()
	select {
	case safe := <-answer:
		if !safe {
			t.Fatal("capture followed a concurrently replaced symlink")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture blocked on a replacement FIFO")
	}
}
