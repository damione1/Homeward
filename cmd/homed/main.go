package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/damione1/Homeward/internal/server"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var listener net.ListenConfig
	code := runHomed(ctx, os.Args[1:], os.Stdout, os.Stderr, homedDependencies{read: os.ReadFile, run: server.Run, listen: listener.Listen})
	cancel()
	if code != 0 {
		os.Exit(code)
	}
}
