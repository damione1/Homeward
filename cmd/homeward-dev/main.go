// homeward-dev is a separate executable, never selected by homed or packaging.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/damione1/Homeward/internal/frontendmock"
)

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("homeward-dev", flag.ContinueOnError)
	mock := flags.Bool("mock", false, "explicitly enable isolated synthetic services")
	data := flags.String("data-dir", "", "new, frontend-owned instance directory")
	cert := flags.String("tls-cert", "", "development certificate PEM")
	key := flags.String("tls-key", "", "development private key PEM")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*mock || *data == "" || *cert == "" || *key == "" {
		return errors.New("requires --mock, a new --data-dir, --tls-cert and --tls-key")
	}
	// A new directory is mandatory: no opening/replacing a production installation.
	if err := os.Mkdir(*data, 0700); err != nil {
		return fmt.Errorf("create isolated instance: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		return err
	}
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	srv := &http.Server{Handler: frontendmock.Handler(epoch), ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(deadline); err != nil {
			_ = srv.Close()
		}
	}()
	if err := json.NewEncoder(output).Encode(map[string]any{"mock": true, "url": "https://" + listener.Addr().String(), "data_dir": *data}); err != nil {
		stop()
		return err
	}
	err = srv.ServeTLS(listener, "", "")
	stop()
	<-shutdown
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
