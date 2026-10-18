package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var ErrPublicShutdownTimeout = errors.New("server: public transport shutdown timed out")

func closedPublicDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

// runPublicListener owns the one public TCP listener. The returned channel
// closes only after every admitted handler has returned, even when the
// configured shutdown deadline has expired. Callers retain Store until then.
func runPublicListener(
	ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr),
	public *compiledPublic, settings config.PublicSettings,
) (<-chan struct{}, error) {
	if ctx == nil || listen == nil || public == nil {
		return closedPublicDone(), ErrInvalidContext
	}
	servingTransport, err := preparePublicServingTransport(c, settings)
	if err != nil {
		return closedPublicDone(), err
	}
	return runPublicListenerPrepared(ctx, c, listen, ready, public, settings, servingTransport)
}

func runPublicListenerPrepared(
	ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr),
	public *compiledPublic, settings config.PublicSettings, transport publicServingTransport,
) (<-chan struct{}, error) {
	if ctx == nil || listen == nil || public == nil {
		return closedPublicDone(), ErrInvalidContext
	}
	if transport.mode != config.PublicTransportH2C && transport.mode != config.PublicTransportTLS ||
		transport.mode == config.PublicTransportTLS && transport.tlsConfig == nil {
		return closedPublicDone(), config.ErrInvalidPublicSettings
	}
	endpoint, err := c.ListenEndpoint()
	if err != nil {
		return closedPublicDone(), err
	}
	if err := ctx.Err(); err != nil {
		return closedPublicDone(), err
	}
	listener, err := listen(ctx, "tcp", endpoint.String())
	if err != nil {
		return closedPublicDone(), err
	}
	if listener == nil {
		return closedPublicDone(), ErrInvalidContext
	}
	admissions := newPublicAdmissions()
	sessions := newPublicAdmissions()
	connections := newPublicListenerConnections(listener)
	servingCtx, servingCancel := context.WithCancel(ctx)
	var routeHandler http.Handler = public.handler(settings, admissions)
	if transport.mode == config.PublicTransportH2C {
		routeHandler = h2c.NewHandler(routeHandler, &http2.Server{})
	}
	sessionHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject Upgrade before h2c can buffer its entire first body.
		if transport.mode == config.PublicTransportH2C && requestsH2CUpgrade(r) {
			http.Error(w, "h2c upgrade unsupported; use HTTP/2 prior knowledge", http.StatusUpgradeRequired)
			return
		}
		if !sessions.begin() {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		defer sessions.end()
		routeHandler.ServeHTTP(w, r)
	})
	server := &http.Server{
		// The outer cap also bounds h2c's first-request read before the
		// inner route handler is reached.
		Handler:           http.MaxBytesHandler(sessionHandler, settings.MaxRequestBytes()),
		BaseContext:       func(net.Listener) context.Context { return servingCtx },
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if transport.mode == config.PublicTransportTLS {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
		server.Protocols = protocols
		server.TLSConfig = transport.tlsConfig
	}
	serveDone := make(chan error, 1)
	go func() {
		if transport.mode == config.PublicTransportTLS {
			// ServeTLS wraps the tracked listener. Its Accept returns *tls.Conn
			// to net/http, so ALPN h2 is visible while raw sockets remain tracked.
			serveDone <- server.ServeTLS(connections, "", "")
		} else {
			serveDone <- server.Serve(connections)
		}
	}()
	var serveErr error
	serveExited := false
	select {
	case serveErr = <-serveDone:
		serveExited = true
	default:
		if ready != nil {
			ready(listener.Addr())
		}
		select {
		case <-ctx.Done():
		case serveErr = <-serveDone:
			serveExited = true
		}
	}
	// BaseContext cancellation reaches cooperative unary and streaming
	// handlers, including those using generated Connect handlers.
	servingCancel()
	routesDone := admissions.close()
	sessionsDone := sessions.close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), settings.ShutdownTimeout())
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(ErrPublicShutdownTimeout, shutdownErr, server.Close())
	}
	if !serveExited {
		serveErr = <-serveDone
	}
	if errors.Is(serveErr, http.ErrServerClosed) ||
		(ctx.Err() != nil && errors.Is(serveErr, net.ErrClosed)) {
		serveErr = nil
	}
	select {
	case <-routesDone:
	case <-shutdownCtx.Done():
		shutdownErr = errors.Join(shutdownErr, ErrPublicShutdownTimeout, shutdownCtx.Err(), server.Close())
	}
	// net/http Shutdown and Close do not own hijacked h2c connections.
	// Closing the accepted sockets releases idle ServeConn loops too.
	connections.closeConnections()
	select {
	case <-sessionsDone:
	case <-shutdownCtx.Done():
		shutdownErr = errors.Join(shutdownErr, ErrPublicShutdownTimeout, shutdownCtx.Err())
	}
	drained := joinPublicDrains(routesDone, sessionsDone)
	cancel()
	return drained, errors.Join(serveErr, shutdownErr)
}

func requestsH2CUpgrade(r *http.Request) bool {
	if r.ProtoMajor != 1 {
		return false
	}
	for _, value := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "h2c") {
				return true
			}
		}
	}
	return false
}

func joinPublicDrains(routes, sessions <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	routesClosed, sessionsClosed := false, false
	select {
	case <-routes:
		routesClosed = true
	default:
	}
	select {
	case <-sessions:
		sessionsClosed = true
	default:
	}
	if routesClosed && sessionsClosed {
		close(done)
	} else {
		go func() {
			<-routes
			<-sessions
			close(done)
		}()
	}
	return done
}

type publicListenerConnections struct {
	net.Listener
	mu      sync.Mutex
	closing bool
	active  map[*publicTrackedConn]struct{}
}

type publicTrackedConn struct {
	net.Conn
	owner *publicListenerConnections
	once  sync.Once
	err   error
}

func newPublicListenerConnections(listener net.Listener) *publicListenerConnections {
	return &publicListenerConnections{Listener: listener, active: make(map[*publicTrackedConn]struct{})}
}

func (l *publicListenerConnections) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &publicTrackedConn{Conn: conn, owner: l}
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.active[tracked] = struct{}{}
	l.mu.Unlock()
	return tracked, nil
}

func (l *publicListenerConnections) closeConnections() {
	l.mu.Lock()
	l.closing = true
	active := make([]*publicTrackedConn, 0, len(l.active))
	for conn := range l.active {
		active = append(active, conn)
	}
	l.mu.Unlock()
	for _, conn := range active {
		_ = conn.Close()
	}
}

func (c *publicTrackedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.active, c)
		c.owner.mu.Unlock()
	})
	return c.err
}
