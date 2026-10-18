package worker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	"github.com/damione1/Homeward/gen/home/integrations/v1/integrationsv1connect"
	"github.com/damione1/Homeward/internal/integrations"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/net/netutil"
	"google.golang.org/protobuf/proto"
)

const (
	maxFrame          = 1 << 20
	maxBindings       = 1024
	maxPending        = 32
	maxCorrelations   = 1024
	maxRPCs           = 32
	startupTimeout    = 5 * time.Second
	rpcTimeout        = 5 * time.Second
	heartbeatInterval = 2 * time.Second
	leaseTimeout      = 6 * time.Second
	stopTimeout       = 5 * time.Second
)

type delivery struct {
	frame     *wire.CommandsResponse
	result    chan error
	delivered bool
	command   integrations.AdmittedCommand
	ctx       context.Context
	expires   time.Time
}
type correlation struct {
	entity  string
	expires time.Time
}

type pilot struct {
	config        Config
	mu            sync.Mutex
	discoverMu    sync.Mutex
	host          integrations.SessionHost
	ctx           context.Context
	active        bool
	handshaken    bool
	hostMinor     uint32
	ready         bool
	token         string
	capabilities  map[wire.Capability]bool
	frameLimit    uint32
	lastHeartbeat time.Time
	bindings      map[string]integrations.Binding
	sources       map[integrations.SourceKey][]byte
	entityHandles map[string][]byte
	correlations  map[string]correlation
	pending       map[string]*delivery
	queue         chan *delivery
	streaming     bool
	streamEver    bool
	streamReady   chan struct{}
	transportLost chan struct{}
	transportOnce sync.Once
	handshakeDone chan struct{}
	process       *exec.Cmd
	waitDone      chan struct{}
	groupDone     chan struct{}
	exitErr       error
	listener      *trackedListener
	server        *http.Server
	serveDone     chan struct{}
	directory     string
	rpcSlots      chan struct{}
	rpcWG         sync.WaitGroup
	rpcDone       chan struct{}
	rpcOnce       sync.Once
}

func newPilot(config Config) *pilot {
	return &pilot{config: config, bindings: make(map[string]integrations.Binding), sources: make(map[integrations.SourceKey][]byte), entityHandles: make(map[string][]byte),
		correlations: make(map[string]correlation), pending: make(map[string]*delivery),
		queue: make(chan *delivery, maxPending), handshakeDone: make(chan struct{}),
		streamReady: make(chan struct{}), transportLost: make(chan struct{}),
		rpcSlots: make(chan struct{}, maxRPCs), rpcDone: make(chan struct{})}
}

func (p *pilot) CommandDriver() integrations.CommandDriver { return p }

func (p *pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if ctx == nil || host == nil || tasks == nil {
		return ErrInvalidConfig
	}
	directory, err := os.MkdirTemp("/tmp", "hw-host-")
	if err != nil {
		return err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		_ = os.Remove(directory)
		return err
	}
	socket := directory + "/host.sock"
	if len(socket) >= 100 {
		_ = os.Remove(directory)
		return ErrInvalidConfig
	}
	rawListener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Remove(directory)
		return err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = rawListener.Close()
		_ = os.RemoveAll(directory)
		return err
	}
	listener := newTrackedListener(rawListener)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(directory)
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	read, write, err := os.Pipe()
	if err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(directory)
		return err
	}
	cmd := exec.Command(p.config.executable, p.config.args...)
	cmd.Dir = directory
	cmd.Env = []string{"HOMEWARD_HOST_SOCKET=" + socket, "HOMEWARD_HOST_TOKEN_FD=3", "PYTHONDONTWRITEBYTECODE=1"}
	cmd.ExtraFiles = []*os.File{read}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := configureProcess(cmd); err != nil {
		_ = read.Close()
		_ = write.Close()
		_ = listener.Close()
		_ = os.RemoveAll(directory)
		return err
	}
	path, handler := integrationsv1connect.NewHostServiceHandler(p, connect.WithReadMaxBytes(maxFrame), connect.WithSendMaxBytes(maxFrame))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := &http.Server{ReadHeaderTimeout: rpcTimeout, IdleTimeout: leaseTimeout,
		MaxHeaderBytes: 8192,
		Handler: h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor != 2 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				http.Error(w, "gRPC required", http.StatusUnsupportedMediaType)
				return
			}
			mux.ServeHTTP(w, r)
		}), &http2.Server{MaxConcurrentStreams: maxRPCs})}
	p.mu.Lock()
	p.host, p.ctx, p.active, p.token = host, ctx, true, token
	p.listener, p.server, p.directory, p.serveDone = listener, server, directory, make(chan struct{})
	p.mu.Unlock()
	go func() { defer close(p.serveDone); _ = server.Serve(netutil.LimitListener(listener, 4)) }()
	if err := tasks.Go(func(context.Context) error { <-p.rpcDone; return nil }); err != nil {
		_ = p.Stop(context.Background())
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		_ = p.Stop(context.Background())
		return err
	}
	_ = read.Close()
	_, writeErr := io.WriteString(write, token)
	_ = write.Close()
	p.mu.Lock()
	p.process, p.waitDone, p.groupDone = cmd, make(chan struct{}), make(chan struct{})
	p.mu.Unlock()
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exitErr = err
		close(p.waitDone)
		p.mu.Unlock()
	}()
	go func() {
		<-p.waitDone // Zero polling while the direct child is running.
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for processGroupAlive(cmd.Process.Pid) {
			<-tick.C
		}
		close(p.groupDone)
	}()
	if err := tasks.Go(func(context.Context) error { <-p.groupDone; return nil }); err != nil {
		return err
	}
	if err := tasks.Go(func(taskCtx context.Context) error {
		<-p.waitDone // Join the actual child even after task cancellation.
		if taskCtx.Err() != nil {
			return nil
		}
		return ErrWorkerExit
	}); err != nil {
		return err
	}
	if writeErr != nil {
		return writeErr
	}
	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	select {
	case <-p.handshakeDone:
	case <-p.waitDone:
		return ErrWorkerExit
	case <-startCtx.Done():
		return ErrUnavailable
	}
	p.mu.Lock()
	commands := p.capabilities[wire.Capability_CAPABILITY_COMMAND]
	p.mu.Unlock()
	if commands {
		select {
		case <-p.streamReady:
		case <-p.waitDone:
			return ErrWorkerExit
		case <-startCtx.Done():
			return ErrUnavailable
		}
	}
	if commands {
		if err := tasks.Go(func(taskCtx context.Context) error {
			select {
			case <-taskCtx.Done():
				return nil
			case <-p.transportLost:
				return ErrUnavailable
			}
		}); err != nil {
			return err
		}
	}
	if err := tasks.Go(func(taskCtx context.Context) error {
		timer := time.NewTicker(heartbeatInterval)
		defer timer.Stop()
		for {
			select {
			case <-taskCtx.Done():
				return nil
			case <-timer.C:
				p.mu.Lock()
				expired := p.active && p.leaseExpiredLocked()
				p.mu.Unlock()
				if expired {
					return ErrHeartbeat
				}
			}
		}
	}); err != nil {
		return err
	}
	return nil
}

func (p *pilot) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfig
	}
	p.mu.Lock()
	p.active = false
	p.bindings = make(map[string]integrations.Binding)
	p.sources = make(map[integrations.SourceKey][]byte)
	p.entityHandles = make(map[string][]byte)
	p.correlations = make(map[string]correlation)
	p.pending = make(map[string]*delivery)
	server, listener, cmd, waitDone, groupDone, serveDone, directory := p.server, p.listener, p.process, p.waitDone, p.groupDone, p.serveDone, p.directory
	p.rpcOnce.Do(func() { go func() { p.rpcWG.Wait(); close(p.rpcDone) }() })
	p.mu.Unlock()
	if listener != nil {
		listener.CloseAll()
	}
	if server != nil {
		_ = server.Close()
	}
	deadline := time.Now().Add(stopTimeout)
	if cmd != nil {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
		grace := time.NewTimer(time.Second)
		select {
		case <-waitDone:
			grace.Stop()
		case <-grace.C:
		}
		// Kill remaining group members even if the direct child exited on TERM.
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	channels := []<-chan struct{}{waitDone, groupDone, serveDone, p.rpcDone}
	if listener != nil {
		channels = append(channels, listener.Done())
	}
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrJoinTimeout
		}
		timer := time.NewTimer(remaining)
		select {
		case <-channel:
			timer.Stop()
		case <-timer.C:
			return ErrJoinTimeout
		}
	}
	if directory != "" {
		_ = os.RemoveAll(directory)
	}
	return nil
}

func (p *pilot) admitted(auth string, capability wire.Capability) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active || !p.handshaken {
		return connect.NewError(connect.CodeFailedPrecondition, ErrUnavailable)
	}
	if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+p.token)) != 1 {
		return connect.NewError(connect.CodeUnauthenticated, ErrUnavailable)
	}
	if p.leaseExpiredLocked() {
		return rpcUnavailable()
	}
	if !p.capabilities[capability] {
		return connect.NewError(connect.CodePermissionDenied, ErrUnavailable)
	}
	return nil
}

// The ticker revokes the attempt, but no admission may use the gap between
// the exact lease deadline and that asynchronous Supervisor notification.
func (p *pilot) leaseExpiredLocked() bool {
	return p.handshaken && !time.Now().Before(p.lastHeartbeat.Add(leaseTimeout))
}

func (p *pilot) frame(message proto.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frameLocked(message)
}

func (p *pilot) frameLocked(message proto.Message) error {
	limit, active := p.frameLimit, p.active
	if !active || limit == 0 {
		return rpcUnavailable()
	}
	if message == nil || proto.Size(message)+5 > int(limit) {
		return connect.NewError(connect.CodeResourceExhausted, ErrCapacity)
	}
	return nil
}

func (p *pilot) admittedFrame(auth string, capability wire.Capability, message proto.Message) error {
	if err := p.admitted(auth, capability); err != nil {
		return err
	}
	return p.frame(message)
}

func (p *pilot) reserveRPC() (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return nil, rpcUnavailable()
	}
	select {
	case p.rpcSlots <- struct{}{}:
		p.rpcWG.Add(1)
		return func() { <-p.rpcSlots; p.rpcWG.Done() }, nil
	default:
		return nil, connect.NewError(connect.CodeResourceExhausted, ErrCapacity)
	}
}

func (p *pilot) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, rpcTimeout)
}

func (p *pilot) workerAlive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active && p.handshaken
}

var _ integrations.Pilot = (*pilot)(nil)
var _ integrations.SupervisedCommandPilot = (*pilot)(nil)
var _ integrations.CommandDriver = (*pilot)(nil)
