// Package server provides the cancellable bootstrap listener, without public API or drivers.
package server

import (
	"context"
	"errors"
	"net"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

type ListenFunc func(context.Context, string, string) (net.Listener, error)

var ErrInvalidContext = errors.New("server: missing context")

func Run(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	if ctx == nil {
		return ErrInvalidContext
	}
	settings, err := c.PostgresSettings()
	if err != nil {
		return err
	}
	if _, err := c.ListenEndpoint(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		return err
	}
	return runListener(ctx, c, listen, ready)
}

func runListener(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	endpoint, err := c.ListenEndpoint()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l, err := listen(ctx, "tcp", endpoint.String())
	if err != nil {
		return err
	}
	defer l.Close()
	// Close interrupts Accept; defer joins this goroutine even on unexpected listener failure.
	stop := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-stop:
		}
	}()
	defer func() { close(stop); <-joined }()
	if ready != nil {
		ready(l.Addr())
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		_ = conn.Close()
	}
}
