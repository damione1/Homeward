// Package server provides the cancellable bootstrap listener, without public API or drivers.
package server

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/config"
	"net"
)

type ListenFunc func(context.Context, string, string) (net.Listener, error)

func Run(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
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
