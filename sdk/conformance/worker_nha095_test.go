package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type readBarrierConn struct {
	net.Conn
	once sync.Once
	read chan struct{}
}

func (c *readBarrierConn) Read(data []byte) (int, error) {
	c.once.Do(func() { close(c.read) })
	return c.Conn.Read(data)
}

func TestNHA095BlockedWorkerControlCancelsAndCloses(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	tracked := &readBarrierConn{Conn: client, read: make(chan struct{})}
	dir := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	adapter := &workerAdapter{conn: tracked, reader: bufio.NewReaderSize(tracked, maxControlFrame), dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := adapter.read(ctx); done <- err }()
	select {
	case <-tracked.read:
	case <-time.After(time.Second):
		t.Fatal("control read did not block")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- adapter.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked control returned %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked control ignored cancellation")
	}
	if err := <-closed; err != nil {
		t.Fatalf("control close: %v", err)
	}
}

func TestNHA095WorkerControlFrameIsBounded(t *testing.T) {
	valid, err := json.Marshal(workerControlEvent{Event: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	adapter := &workerAdapter{conn: client, reader: bufio.NewReaderSize(client, maxControlFrame)}
	written := make(chan error, 1)
	go func() {
		_, err := peer.Write(append(valid, '\n'))
		written <- err
	}()
	event, err := adapter.read(context.Background())
	if err != nil || event.Event != "ready" {
		t.Fatalf("valid control frame rejected: %+v, %v", event, err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write valid frame: %v", err)
	}
	_ = client.Close()
	_ = peer.Close()

	client, peer = net.Pipe()
	defer client.Close()
	defer peer.Close()
	adapter = &workerAdapter{conn: client, reader: bufio.NewReaderSize(client, maxControlFrame)}
	large, err := json.Marshal(workerControlEvent{Event: "ready", Reason: strings.Repeat("x", maxControlFrame)})
	if err != nil || len(large) <= maxControlFrame {
		t.Fatalf("large valid JSON fixture is invalid: len=%d error=%v", len(large), err)
	}
	written = make(chan error, 1)
	go func() {
		_, err := peer.Write(append(large, '\n'))
		written <- err
	}()
	_, err = adapter.read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded control frame admitted: %v", err)
	}
	_ = peer.Close() // release a writer still sending the rejected frame
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("oversized frame writer was not joined")
	}
}
