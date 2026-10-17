package server

import (
	"context"
	"github.com/damione1/new-home-assistant/internal/config"
	"net"
	"testing"
	"time"
)

func TestNHA001Lifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() {
		done <- Run(ctx, config.Config{Listen: "127.0.0.1:0"}, lc.Listen, func(a net.Addr) { ready <- a })
	}()
	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("startup timeout")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timeout")
	}
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("port still open")
	}
}
func TestNHA001InvalidConfigurationNeverListens(t *testing.T) {
	for _, data := range []string{"listen: [", "listen: '127.0.0.1:0'\nunknown: true", "listen: '127.0.0.1:0'\n---\nlisten: '127.0.0.1:1'", "listen: 'localhost:8787'", "listen: '127.0.0.1:99999'", "listen: '127.0.0.1:http'", "listen: '127.0.0.1:-1'", ""} {
		if _, err := config.Parse([]byte(data)); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	called := false
	err := Run(context.Background(), config.Config{Listen: "invalid"}, func(context.Context, string, string) (net.Listener, error) { called = true; return nil, nil }, nil)
	if err == nil || called {
		t.Fatal("invalid config reached listener")
	}
}
