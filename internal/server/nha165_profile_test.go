package server

// NHA165 is deliberately a test-only composition: the measured core is a
// separate non-race process running the real Run/SQL/owner/TLS/native authority.
// The supervisor and all ten generated clients live outside that RSS boundary.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	reg "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/processbudget"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nha165Entities = 850
const nha165Clients = 10
const nha165RSSLimit = uint64(256 << 20)
const nha165LatencyLimit = int64(50 * time.Millisecond)
const nha165FirstSequence = uint64(nha165Entities + 1)

type nha165Profile struct {
	Name             string `json:"name"`
	SustainedSeconds int    `json:"sustained_seconds"`
	BurstSeconds     int    `json:"burst_seconds"`
	SustainedRate    int    `json:"sustained_rate"`
	BurstRate        int    `json:"burst_rate"`
}

func nha165SelectedProfile() (nha165Profile, error) {
	switch os.Getenv("HOMEWARD_NHA165_PROFILE") {
	case "acceptance":
		return nha165Profile{"acceptance", 3600, 60, 100, 1000}, nil
	case "preflight":
		return nha165Profile{"preflight_NOT_ONE_HOUR", 10, 2, 100, 1000}, nil
	case "rotation":
		return nha165Profile{"rotation_NOT_ONE_HOUR", 250, 2, 100, 1000}, nil
	default:
		return nha165Profile{}, errors.New("explicit HOMEWARD_NHA165_PROFILE=acceptance, preflight or rotation required")
	}
}
func (p nha165Profile) count() int {
	return p.SustainedSeconds*p.SustainedRate + p.BurstSeconds*p.BurstRate
}
func (p nha165Profile) seconds() int { return p.SustainedSeconds + p.BurstSeconds }
func nha165Mono() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(err)
	}
	return ts.Nano()
}
func nha165WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".new"
	if err := os.WriteFile(temp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
func nha165ReadJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func nha165WaitFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func nha165Proof(t *testing.T) string {
	t.Helper()
	p := os.Getenv("HOMEWARD_NHA165_PROOF")
	if !filepath.IsAbs(p) {
		t.Fatal("absolute proof directory required")
	}
	return p
}

type nha165Record struct {
	Client       int    `json:"client,omitempty"`
	Kind         string `json:"kind"`
	Sequence     uint64 `json:"sequence,omitempty"`
	Session      string `json:"session,omitempty"`
	At           int64  `json:"monotonic_ns"`
	Scheduled    int64  `json:"scheduled_ns,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Command      string `json:"command,omitempty"`
	RSS          uint64 `json:"max_rss_bytes,omitempty"`
	Heap         uint64 `json:"heap_alloc_bytes,omitempty"`
	GoSys        uint64 `json:"go_sys_bytes,omitempty"`
	HeapSys      uint64 `json:"heap_sys_bytes,omitempty"`
	HeapIdle     uint64 `json:"heap_idle_bytes,omitempty"`
	HeapReleased uint64 `json:"heap_released_bytes,omitempty"`
	GoManaged    uint64 `json:"go_managed_bytes,omitempty"`
	TotalAlloc   uint64 `json:"total_alloc_bytes,omitempty"`
	GC           uint32 `json:"num_gc,omitempty"`
	Goroutines   int    `json:"goroutines,omitempty"`
	UserCPU      int64  `json:"user_cpu_us,omitempty"`
	SystemCPU    int64  `json:"system_cpu_us,omitempty"`
	Queues       any    `json:"queues,omitempty"`
	Lags         any    `json:"lags,omitempty"`
}
type nha165Log struct {
	mu  sync.Mutex
	f   *os.File
	b   *bufio.Writer
	err error
}

func nha165NewLog(t *testing.T, path string) *nha165Log {
	t.Helper()
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	l := &nha165Log{f: f, b: bufio.NewWriterSize(f, 64<<10)}
	t.Cleanup(func() {
		if e := l.close(); e != nil {
			t.Error(e)
		}
	})
	return l
}
func (l *nha165Log) put(v nha165Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return
	}
	b, e := json.Marshal(v)
	if e == nil {
		_, e = l.b.Write(append(b, '\n'))
	}
	l.err = e
}
func (l *nha165Log) flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = l.b.Flush()
	}
	return l.err
}
func (l *nha165Log) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return l.err
	}
	if l.err == nil {
		l.err = l.b.Flush()
	}
	e := l.f.Close()
	l.f = nil
	return errors.Join(l.err, e)
}
func nha165RSS(r *syscall.Rusage) uint64 {
	n := uint64(r.Maxrss)
	if runtime.GOOS == "linux" {
		n *= 1024
	}
	return n
}
func nha165Resource(l *nha165Log, s *sequencer.Sequencer) {
	var r syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_SELF, &r); e != nil {
		panic(e)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	q, g, e := s.ObservationMetrics()
	if e != nil {
		panic(e)
	}
	l.put(nha165Record{Kind: "resource", At: nha165Mono(), RSS: nha165RSS(&r), Heap: m.HeapAlloc, GoSys: m.Sys, HeapSys: m.HeapSys, HeapIdle: m.HeapIdle, HeapReleased: m.HeapReleased, GoManaged: m.Sys - m.HeapReleased, TotalAlloc: m.TotalAlloc, GC: m.NumGC, Goroutines: runtime.NumGoroutine(), UserCPU: r.Utime.Sec*1000000 + int64(r.Utime.Usec), SystemCPU: r.Stime.Sec*1000000 + int64(r.Stime.Usec), Queues: q, Lags: g})
}

type nha165Ready struct {
	Base     string `json:"base"`
	Data     string `json:"data"`
	Token    string `json:"token"`
	Alias    string `json:"alias"`
	Epoch    string `json:"epoch"`
	Revision uint64 `json:"revision"`
	PID      int    `json:"pid"`
}
type nha165CoreSummary struct {
	GoMemoryLimit int64 `json:"go_memory_limit_bytes"`
	Profile       nha165Profile
	Count         int
	Edges         uint64
	Dispatches    uint64
	Start         int64
	End           int64
	PID           int
	MaxRSS        uint64
	SourceSession string
}
type nha165ClientSummary struct {
	Rotations        [nha165Clients]int `json:"stream_rotations"`
	Refreshes        [nha165Clients]int `json:"native_session_refreshes"`
	CommandRotations int                `json:"command_session_rotations"`
	Counts           [nha165Clients]int
	Sessions         [nha165Clients]string
	SnapshotEntities [nha165Clients]int
	Commands         int
}
type nha165Driver struct {
	log   *nha165Log
	count atomic.Uint64
}

func (d *nha165Driver) Dispatch(_ context.Context, c integrations.AdmittedCommand) error {
	at := nha165Mono()
	if !c.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	wire, e := c.Request()
	if e != nil {
		return e
	}
	d.log.put(nha165Record{Kind: "driver", At: at, Command: wire.CommandId})
	d.count.Add(1)
	return nil
}

// The finite test route borrows Run's native TLS authorization and the volatile
// service attached to the SAME Run-owned Host. It copies the existing NHA050
// fixture seam, never constructs an independent Host or an authorization bypass.
func TestNHA165CoreHelper(t *testing.T) {
	if os.Getenv("HOMEWARD_NHA165_HELPER") != "core" {
		t.Skip("explicit isolated profile helper only")
	}
	processbudget.ConfigureServer() // exact permanent homed process-owner startup policy
	p, e := nha165SelectedProfile()
	if e != nil {
		t.Fatal(e)
	}
	proof := nha165Proof(t)
	t.Cleanup(func() { _ = os.Remove(filepath.Join(proof, "ready.private.json")) })
	port, e := strconv.Atoi(os.Getenv("HOMEWARD_NHA165_PG_PORT"))
	if e != nil {
		t.Fatal(e)
	}
	data := os.Getenv("HOMEWARD_NHA165_DATA")
	if !filepath.IsAbs(data) {
		t.Fatal("private data directory required")
	}
	settings := nha049Config(t, port, data)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	log := nha165NewLog(t, filepath.Join(proof, "core.jsonl"))
	driver := &nha165Driver{log: log}
	instance, _ := domain.ParseIntegrationInstanceID("nha165-only-fake")
	var aliases struct {
		Entities []struct {
			ID string `json:"entity_id"`
		} `json:"entities"`
	}
	if e := json.Unmarshal(scope.ManifestV1(), &aliases); e != nil || len(aliases.Entities) != nha165Entities {
		t.Fatal("frozen850manifest", e)
	}
	keys := make([]integrations.SourceKey, nha165Entities)
	for i := range keys {
		keys[i], e = integrations.NewSourceKey(fmt.Sprintf("nha165-device-%03d", i), "relay")
		if e != nil {
			t.Fatal(e)
		}
	}
	var host *integrations.Manager
	var sequence *sequencer.Sequencer
	var owner *configactivation.Owner
	var volatile *integrations.VolatileCommandService
	factory := func(c context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, e := registry.LoadManifest(scope.BaselineV1())
		if e != nil {
			return RuntimePlan{}, e
		}
		repo, e := registry.NewRepository(store, manifest)
		if e != nil {
			return RuntimePlan{}, e
		}
		for i, key := range keys {
			if _, e := repo.ConfigureBinding(c, instance, key, aliases.Entities[i].ID); e != nil {
				return RuntimePlan{}, e
			}
		}
		public, e := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if e != nil {
			return RuntimePlan{}, e
		}
		e = public.Register(RouteKindMedia, RouteMethodPost, "/nha165/volatile", RouteAccessProtected, RouteFlowUnary, RouteTransportTLSRequired, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", 401)
				return
			}
			b, e := io.ReadAll(io.LimitReader(r.Body, 4097))
			request := new(actions.CommandRequest)
			if e != nil || len(b) > 4096 || proto.Unmarshal(b, request) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			start := nha165Mono()
			response, e := volatile.Dispatch(r.Context(), actor, request)
			log.put(nha165Record{Kind: "command", At: start, Command: request.CommandId})
			if e != nil || response == nil {
				http.Error(w, "command refused", 409)
				return
			}
			b, e = proto.Marshal(response)
			if e != nil {
				http.Error(w, "encode", 500)
				return
			}
			w.Header().Set("Content-Type", "application/protobuf")
			_, _ = w.Write(b)
		}))
		if e != nil {
			return RuntimePlan{}, e
		}
		permission, e := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
		if e != nil {
			return RuntimePlan{}, e
		}
		if e = public.BindPermission(RouteKindMedia, RouteMethodPost, "/nha165/volatile", permission); e != nil {
			return RuntimePlan{}, e
		}
		return RuntimePlan{Identity: repo, Public: public, Configure: func(_ context.Context, h *integrations.Manager, _ *storage.Store) error { host = h; return nil }, ConfigureAuthorization: func(_ context.Context, a AuthorizationComposition) error {
			sequence = a.(runtimeAuthorizationComposition).views.source
			return nil
		}, ConfigureActivation: func(_ context.Context, o *configactivation.Owner) error {
			owner = o
			var e error
			volatile, e = integrations.NewVolatileCommandService(host, o, integrations.DefaultVolatileCommandLimits())
			return e
		}}, nil
	}
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() {
		done <- RunWithStartupFactory(ctx, settings.startup, listen.Listen, func(a net.Addr) { ready <- a }, factory)
	}()
	var addr net.Addr
	select {
	case addr = <-ready:
	case e := <-done:
		t.Fatal("core startup", e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var joined bool
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case e := <-done:
				if e != nil {
					t.Error(e)
				}
			case <-time.After(15 * time.Second):
				t.Error("core owner did not join")
			}
		}
	})
	if host == nil || owner == nil || sequence == nil || volatile == nil {
		t.Fatal("actual owners missing")
	}
	session, e := host.OpenSession(instance)
	if e != nil {
		t.Fatal(e)
	}
	candidates := make([]integrations.Candidate, nha165Entities)
	for i, key := range keys {
		candidates[i], e = integrations.NewCandidate(key, []*reg.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA165 fake relay", Constraints: &reg.CapabilityDescription_Switch{Switch: &reg.SwitchConstraints{SetOn: true}}}})
		if e != nil {
			t.Fatal(e)
		}
	}
	bindings := make([]integrations.Binding, 0, nha165Entities)
	for i := 0; i < nha165Entities; i += 250 {
		end := min(i+250, nha165Entities)
		b, e := session.Discover(ctx, candidates[i:end])
		if e != nil {
			t.Fatal(e)
		}
		bindings = append(bindings, b...)
	}
	if e := host.AttachCommandDriver(session, driver); e != nil {
		t.Fatal(e)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	publish := func(index int, on bool, seq uint64) (int64, error) {
		v, at := domain.NewSwitchValue(on), time.Now().UTC()
		o, e := integrations.NewStateObservation("relay", quality, &v, &at, at, &seq)
		if e != nil {
			return 0, e
		}
		start := nha165Mono()
		return start, session.Publish(ctx, bindings[index], o)
	}
	for i := range bindings {
		if _, e := publish(i, false, uint64(i+1)); e != nil {
			t.Fatal(e)
		}
	}
	// Native bootstrap and policy CAS happen once, before the measured hour.
	client := nha181Client(nha165LoadRoots(t, data))
	defer client.CloseIdleConnections()
	opHTTP := &http.Client{Transport: &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(c, "unix", filepath.Join(data, operatorSocketName))
	}}}
	defer opHTTP.CloseIdleConnections()
	password := "NHA165 synthetic device-free native password"
	admin, e := authv1connect.NewOperatorAccountServiceClient(opHTTP, "http://operator").BootstrapAdmin(ctx, connect.NewRequest(&auth.BootstrapAdminRequest{LoginName: "NHA165 Admin", DisplayName: "NHA165", Password: password}))
	if e != nil {
		t.Fatal(e)
	}
	base := "https://" + addr.String()
	login, e := authv1connect.NewAuthServiceClient(client, base).StartLogin(ctx, connect.NewRequest(&auth.StartLoginRequest{LoginName: "NHA165 Admin", Password: password}))
	if e != nil || login.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("native login", e)
	}
	token := login.Msg.Decision.GetAuthenticated().AccessToken
	policy := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	current, e := policy.GetPolicy(ctx, nha141BearerRequest(&auth.GetPolicyRequest{AccountId: admin.Msg.Account.Id}, token))
	if e != nil {
		t.Fatal(e)
	}
	source := "schema: homeward.authorization/1\naccount_id: " + admin.Msg.Account.Id.Value + "\ngrants:\n"
	for _, s := range []string{"state_registry, operation: read", "actions, operation: read", "actions, operation: write"} {
		source += "  - scope: {family: " + s + "}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"
	}
	_, e = policy.ReplacePolicy(ctx, nha141BearerRequest(&auth.ReplacePolicyRequest{AccountId: admin.Msg.Account.Id, YamlSource: []byte(source), Precondition: &auth.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, token))
	if e != nil {
		t.Fatal(e)
	}
	epoch, e := sequence.CurrentAdmissionEpoch()
	if e != nil {
		t.Fatal(e)
	}
	stamp, e := owner.CurrentConfig(ctx)
	if e != nil {
		t.Fatal(e)
	}
	revision, _, e := stamp.Components()
	if e != nil {
		t.Fatal(e)
	}
	r := nha165Ready{Base: base, Data: data, Token: token, Alias: aliases.Entities[0].ID, Epoch: epoch, Revision: revision, PID: os.Getpid()}
	if e := nha165WriteJSON(filepath.Join(proof, "ready.private.json"), r); e != nil {
		t.Fatal(e)
	}
	if e := nha165WaitFile(ctx, filepath.Join(proof, "clients-ready.json")); e != nil {
		t.Fatal(e)
	}
	// SubscribeEdge returns cursor separately; the dedicated edge helper below
	// performs exact accounting without conflating state WatchView with events.
	nha165RunIngress(t, ctx, p, proof, log, sequence, session, bindings, publish, driver)
	if e := nha165WaitFile(ctx, filepath.Join(proof, "clients-done.json")); e != nil {
		t.Fatal(e)
	}
	if e := log.close(); e != nil {
		t.Fatal(e)
	}
	cancel()
	select {
	case e := <-done:
		joined = true
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run drain")
	}
	var final nha165CoreSummary
	if e := nha165ReadJSON(filepath.Join(proof, "main-done.json"), &final); e != nil {
		t.Fatal(e)
	}
	var usage syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); e != nil {
		t.Fatal(e)
	}
	final.MaxRSS = nha165RSS(&usage)
	final.Dispatches = driver.count.Load()
	if e := nha165WriteJSON(filepath.Join(proof, "core-final.json"), final); e != nil {
		t.Fatal(e)
	}
	_ = os.Remove(filepath.Join(proof, "ready.private.json"))
}

func nha165LoadRoots(t *testing.T, data string) *x509.CertPool {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if e != nil {
		t.Fatal(e)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		t.Fatal("fixture CA")
	}
	return p
}

func nha165RunIngress(t *testing.T, ctx context.Context, p nha165Profile, proof string, log *nha165Log, sequence *sequencer.Sequencer, session *integrations.Session, bindings []integrations.Binding, publish func(int, bool, uint64) (int64, error), driver *nha165Driver) {
	t.Helper()
	edge, _, e := sequence.SubscribeEdge()
	if e != nil {
		t.Fatal(e)
	}
	defer edge.Close()
	edgeCtx, edgeCancel := context.WithCancel(ctx)
	defer edgeCancel()
	edgeDone := make(chan error, 1)
	var edgeCount atomic.Uint64
	var sourceSession atomic.Value
	go func() {
		var expected uint64 = 1
		for {
			d, e := edge.Next(edgeCtx)
			if e != nil {
				if errors.Is(e, context.Canceled) {
					edgeDone <- nil
				} else {
					edgeDone <- e
				}
				return
			}
			if loss, ok := d.Loss(); ok {
				edgeDone <- fmt.Errorf("edge loss must be explicit and fail acceptance: %+v", loss)
				return
			}
			entry, ok := d.Entry()
			if !ok {
				edgeDone <- errors.New("edge delivery kind")
				return
			}
			if entry.Kind() != sequencer.KindEvent {
				continue
			}
			_, event, ok := entry.Event()
			if !ok {
				edgeDone <- errors.New("edge shape")
				return
			}
			code, _, seq, e := event.Components()
			if e != nil || code != "nha165_edge" || seq == nil || *seq != expected {
				edgeDone <- fmt.Errorf("edge exact accounting expected=%d got=%v err=%v", expected, seq, e)
				return
			}
			_, stamp, _, present := entry.Origin()
			name, _, e := stamp.Components()
			if !present || e != nil || name == "" {
				edgeDone <- errors.New("edge source session missing")
				return
			}
			if old := sourceSession.Load(); old != nil && old.(string) != name {
				edgeDone <- errors.New("edge source session changed")
				return
			}
			sourceSession.Store(name)
			expected++
			edgeCount.Add(1)
		}
	}()
	resourceCtx, resourceCancel := context.WithCancel(ctx)
	resourceDone := make(chan error, 1)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-resourceCtx.Done():
				resourceDone <- log.flush()
				return
			case <-tick.C:
				nha165Resource(log, sequence)
				if e := log.flush(); e != nil {
					resourceDone <- e
					return
				}
			}
		}
	}()
	defer func() {
		resourceCancel()
		if e := <-resourceDone; e != nil {
			t.Error(e)
		}
	}()
	nha165Resource(log, sequence)
	start := nha165Mono()
	if e := nha165WriteJSON(filepath.Join(proof, "main-start.json"), map[string]int64{"monotonic_ns": start}); e != nil {
		t.Fatal(e)
	}
	index := 0
	edgeExpected := uint64(0)
	for _, phase := range []struct {
		name          string
		seconds, rate int
	}{{"sustained", p.SustainedSeconds, p.SustainedRate}, {"burst", p.BurstSeconds, p.BurstRate}} {
		phaseStart := nha165Mono()
		for i := 0; i < phase.seconds*phase.rate; i++ {
			deadline := phaseStart + int64(i+1)*int64(time.Second)/int64(phase.rate)
			if remaining := deadline - nha165Mono(); remaining > 0 {
				timer := time.NewTimer(time.Duration(remaining))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					t.Fatal(ctx.Err())
				}
			}
			seq := nha165FirstSequence + uint64(index)
			at, e := publish(index%nha165Entities, (index/nha165Entities)%2 == 0, seq)
			if e != nil {
				t.Fatalf("scheduled observation %d: %v", index, e)
			}
			log.put(nha165Record{Kind: "observation", Sequence: seq, At: at, Scheduled: deadline, Phase: phase.name})
			index++
			if i%phase.rate == 0 {
				edgeExpected++
				event, e := integrations.NewDeviceEvent("nha165_edge", time.Now().UTC(), &edgeExpected)
				if e != nil {
					t.Fatal(e)
				}
				if e = session.Emit(ctx, bindings[0], event); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	end := nha165Mono()
	if end-start < int64(p.seconds())*int64(time.Second) {
		t.Fatal("wall duration less than declared open-loop phases")
	}
	// Edges must be drained by a real subscription, not inferred from state frames.
	drain, dc := context.WithTimeout(ctx, 10*time.Second)
	defer dc()
	for edgeCount.Load() != edgeExpected {
		select {
		case e := <-edgeDone:
			t.Fatal("edge consumer", e)
		case <-drain.Done():
			t.Fatalf("edge drain %d/%d: %v", edgeCount.Load(), edgeExpected, drain.Err())
		case <-time.After(time.Millisecond):
		}
	}
	edgeCancel()
	if e := <-edgeDone; e != nil {
		t.Fatal(e)
	}
	nha165Resource(log, sequence)
	var usage syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); e != nil {
		t.Fatal(e)
	}
	var src string
	if v := sourceSession.Load(); v != nil {
		src = v.(string)
	}
	summary := nha165CoreSummary{GoMemoryLimit: debug.SetMemoryLimit(-1), Profile: p, Count: index, Edges: edgeExpected, Dispatches: driver.count.Load(), Start: start, End: end, PID: os.Getpid(), MaxRSS: nha165RSS(&usage), SourceSession: src}
	if e := nha165WriteJSON(filepath.Join(proof, "main-done.json"), summary); e != nil {
		t.Fatal(e)
	}
}

func TestNHA165ClientsHelper(t *testing.T) {
	if os.Getenv("HOMEWARD_NHA165_HELPER") != "clients" {
		t.Skip("explicit isolated profile helper only")
	}
	p, e := nha165SelectedProfile()
	if e != nil {
		t.Fatal(e)
	}
	proof := nha165Proof(t)
	var ready nha165Ready
	if e := nha165ReadJSON(filepath.Join(proof, "ready.private.json"), &ready); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	fast := nha165NewLog(t, filepath.Join(proof, "fast-client.jsonl"))
	var summary nha165ClientSummary
	var readyCount atomic.Int32
	errorsCh := make(chan error, nha165Clients+1)
	// Native StartLogin has a bounded KDF admission and reserves a per-login
	// attempt before KDF. A committed successful CreateSession clears those
	// attempts. Acquire one session at a time BEFORE the measured workload;
	// the ten readers and command sampler then refresh independent sessions.
	provisioning := nha181Client(nha165LoadRoots(t, ready.Data))
	defer provisioning.CloseIdleConnections()
	var sessions [nha165Clients + 1]*auth.SessionTokens
	for i := range sessions {
		sessions[i], e = nha165Login(ctx, provisioning, ready.Base)
		if e != nil {
			t.Fatalf("native session provisioning%d: %v", i, e)
		}
	}
	provisioning.CloseIdleConnections()
	var snapshots sync.Mutex
	var wg sync.WaitGroup
	for id := 0; id < nha165Clients; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			e := nha165ReadClient(ctx, t, id, p, proof, ready, sessions[id], &snapshots, fast, &summary, &readyCount)
			if e != nil {
				errorsCh <- fmt.Errorf("client%d: %w", id, e)
				cancel()
			}
		}(id)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e := nha165CommandsClient(ctx, t, p, proof, ready, sessions[nha165Clients], &summary)
		if e != nil {
			errorsCh <- e
			cancel()
		}
	}()
	wg.Wait()
	close(errorsCh)
	for e := range errorsCh {
		t.Error(e)
	}
	if t.Failed() {
		return
	}
	if e := fast.close(); e != nil {
		t.Fatal(e)
	}
	if e := nha165WriteJSON(filepath.Join(proof, "clients-done.json"), summary); e != nil {
		t.Fatal(e)
	}
}
func nha165Login(ctx context.Context, client *http.Client, base string) (*auth.SessionTokens, error) {
	r, e := authv1connect.NewAuthServiceClient(client, base).StartLogin(ctx, connect.NewRequest(&auth.StartLoginRequest{LoginName: "NHA165 Admin", Password: "NHA165 synthetic device-free native password"}))
	if e != nil {
		return nil, e
	}
	if r.Msg.Decision.GetAuthenticated() == nil {
		return nil, errors.New("native login not authenticated")
	}
	return r.Msg.Decision.GetAuthenticated(), nil
}

var nha165RotateStream = errors.New("owned four-minute stream rotation")
var nha165StreamFinished = errors.New("owned stream reader finished")

// A local timer owns rotation; a Connect timeout must not race the server's
// propagated deadline. WithCancelCause exports no new RPC deadline. Joining
// AfterFunc before classification prevents an unjoined late timer from changing
// an already observed failure into a planned rotation.
type nha165StreamRotation struct {
	ctx    context.Context
	parent context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	fired  chan struct{}
	stop   sync.Once
}

func nha165NewStreamRotation(parent context.Context, duration time.Duration) *nha165StreamRotation {
	ctx, cancel := context.WithCancelCause(parent)
	r := &nha165StreamRotation{ctx: ctx, parent: parent, cancel: cancel, fired: make(chan struct{})}
	r.timer = time.AfterFunc(duration, func() {
		cancel(nha165RotateStream)
		close(r.fired)
	})
	return r
}
func (r *nha165StreamRotation) stopTimer() {
	r.stop.Do(func() {
		if !r.timer.Stop() {
			<-r.fired
		}
	})
}
func (r *nha165StreamRotation) close() {
	r.stopTimer()
	r.cancel(nha165StreamFinished)
}
func (r *nha165StreamRotation) planned(err error) bool {
	r.stopTimer()
	// A genuine remote DeadlineExceeded/EOF/resync is never a rotation,
	// even if its delivery happens to coincide with the owned timer.
	canceled := errors.Is(err, context.Canceled) || connect.CodeOf(err) == connect.CodeCanceled
	planned := r.parent.Err() == nil && errors.Is(context.Cause(r.ctx), nha165RotateStream) && canceled && !errors.Is(err, context.DeadlineExceeded) && connect.CodeOf(err) != connect.CodeDeadlineExceeded
	r.cancel(nha165StreamFinished)
	return planned
}

func nha165ReadClient(ctx context.Context, t *testing.T, id int, p nha165Profile, proof string, ready nha165Ready, tokens *auth.SessionTokens, snapshots *sync.Mutex, fast *nha165Log, summary *nha165ClientSummary, readyCount *atomic.Int32) error {
	client := nha181Client(nha165LoadRoots(t, ready.Data))
	client.Timeout = 0
	defer client.CloseIdleConnections()
	api := statev1connect.NewStateServiceClient(client, ready.Base)
	// OpenView admits only MaxPreparations=2 simultaneous captures. Serialize
	// bootstrap snapshot+all850pages locally; release before WatchView blocks.
	// All ten independent native streams remain concurrent during measurement.
	var cursor *state.ViewCursor
	bootstrapErr := func() error {
		snapshots.Lock()
		defer snapshots.Unlock()
		open, e := api.OpenView(ctx, nha141BearerRequest(&state.OpenViewRequest{PageSize: 100, Filter: &state.ViewFilter{}}, tokens.AccessToken))
		if e != nil {
			return e
		}
		page := open.Msg
		cursor = proto.Clone(page.Cursor).(*state.ViewCursor)
		seen := make(map[string]bool, nha165Entities)
		for {
			for _, entity := range page.Entities {
				if entity.EntityId == "" || seen[entity.EntityId] {
					return errors.New("snapshot duplicate/empty entity")
				}
				seen[entity.EntityId] = true
			}
			if page.NextPageToken == "" {
				break
			}
			next, e := api.ReadPage(ctx, nha141BearerRequest(&state.ReadPageRequest{SnapshotToken: page.SnapshotToken, PageToken: page.NextPageToken}, tokens.AccessToken))
			if e != nil {
				return e
			}
			page = next.Msg.Page
		}
		if len(seen) != nha165Entities {
			return fmt.Errorf("snapshot %d entities", len(seen))
		}
		summary.SnapshotEntities[id] = len(seen)
		return nil
	}()
	if bootstrapErr != nil {
		return bootstrapErr
	}

	if readyCount.Add(1) == nha165Clients {
		if e := nha165WriteJSON(filepath.Join(proof, "clients-ready.json"), map[string]int{"active_readers": nha165Clients}); e != nil {
			return e
		}
	}
	// Access leases last 15m. Rotate at 4m through native RefreshSession and
	// reopen from the LAST delivered public cursor: no snapshot reset or ignored
	// frames; all sequences across every rotation remain strictly contiguous.
	expected := nha165FirstSequence
	var source string
	received := 0
	pendingRotation := false
	for received < p.count() {
		rotation := nha165NewStreamRotation(ctx, 4*time.Minute)
		stream, e := api.WatchView(rotation.ctx, nha141BearerRequest(&state.WatchViewRequest{Cursor: cursor, Filter: &state.ViewFilter{}}, tokens.AccessToken))
		if e == nil {
			for stream.Receive() {
				at := nha165Mono()
				frame := stream.Msg()
				if frame.GetResyncRequired() != nil {
					rotation.close()
					stream.Close()
					return fmt.Errorf("unexpected resync: %v", frame.GetResyncRequired().Reason)
				}
				if frame.Cursor == nil {
					rotation.close()
					stream.Close()
					return errors.New("frame cursor missing")
				}
				cursor = proto.Clone(frame.Cursor).(*state.ViewCursor)
				for _, value := range frame.GetDelta().GetStates() {
					if value.SourceSeq == nil || *value.SourceSeq < nha165FirstSequence {
						continue
					}
					seq := *value.SourceSeq
					if seq != expected {
						rotation.close()
						stream.Close()
						return fmt.Errorf("silent loss or duplicate: expected=%d got=%d", expected, seq)
					}
					if source == "" {
						source = value.SourceSession
					}
					if source == "" || source != value.SourceSession {
						rotation.close()
						stream.Close()
						return errors.New("source session changed")
					}
					if id == 0 {
						fast.put(nha165Record{Kind: "received", At: at, Sequence: seq, Session: source})
					}
					if pendingRotation {
						summary.Rotations[id]++
						pendingRotation = false
					}
					expected++
					received++
					if received%100 == 0 {
						fast.put(nha165Record{Kind: "client_progress", Client: id, At: at, Sequence: seq, Session: source})
					}
					if received > p.count() {
						rotation.close()
						stream.Close()
						return errors.New("unexpected extra observation")
					}
				}
				if received == p.count() {
					break
				}
			}
			e = stream.Err()
		}
		planned := rotation.planned(e)
		if stream != nil {
			stream.Close()
		}
		if received == p.count() {
			break
		}
		if !planned {
			return fmt.Errorf("stream stopped before all frames %d/%d: %w", received, p.count(), e)
		}
		refresh, e := authv1connect.NewAuthServiceClient(client, ready.Base).RefreshSession(ctx, connect.NewRequest(&auth.RefreshSessionRequest{RefreshToken: tokens.RefreshToken}))
		if e != nil {
			return e
		}
		tokens = refresh.Msg.Tokens
		if tokens == nil {
			return errors.New("native refresh missing tokens")
		}
		summary.Refreshes[id]++
		pendingRotation = true
	}
	summary.Counts[id] = received
	summary.Sessions[id] = source
	return nil
}
func nha165CommandsClient(ctx context.Context, t *testing.T, p nha165Profile, proof string, ready nha165Ready, tokens *auth.SessionTokens, summary *nha165ClientSummary) error {
	if e := nha165WaitFile(ctx, filepath.Join(proof, "main-start.json")); e != nil {
		return e
	}
	var start map[string]int64
	if e := nha165ReadJSON(filepath.Join(proof, "main-start.json"), &start); e != nil {
		return e
	}
	client := nha181Client(nha165LoadRoots(t, ready.Data))
	client.Timeout = 10 * time.Second
	defer client.CloseIdleConnections()
	for i := 0; i < p.seconds(); i++ {
		deadline := start["monotonic_ns"] + int64(i)*int64(time.Second)
		if d := deadline - nha165Mono(); d > 0 {
			timer := time.NewTimer(time.Duration(d))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if i > 0 && i%240 == 0 {
			refresh, e := authv1connect.NewAuthServiceClient(client, ready.Base).RefreshSession(ctx, connect.NewRequest(&auth.RefreshSessionRequest{RefreshToken: tokens.RefreshToken}))
			if e != nil {
				return e
			}
			tokens = refresh.Msg.Tokens
			if tokens == nil {
				return errors.New("command session refresh")
			}
			summary.CommandRotations++
		}
		command := &actions.CommandRequest{CommandId: fmt.Sprintf("nha165-volatile-%05d", i), AdmissionEpoch: ready.Epoch, ConfigRevision: ready.Revision, Target: &reg.EntityRef{Reference: &reg.EntityRef_HaAlias{HaAlias: ready.Alias}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: i%2 == 0}}}, ExpiresAt: timestamppb.New(time.Now().Add(5 * time.Second))}
		b, e := proto.Marshal(command)
		if e != nil {
			return e
		}
		request, e := http.NewRequestWithContext(ctx, http.MethodPost, ready.Base+"/nha165/volatile", bytes.NewReader(b))
		if e != nil {
			return e
		}
		request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		request.Header.Set("Content-Type", "application/protobuf")
		response, e := client.Do(request)
		if e != nil {
			return e
		}
		b, e = io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if e != nil {
			return e
		}
		var result actions.CommandResponse
		if response.StatusCode != 200 || proto.Unmarshal(b, &result) != nil || result.CommandId != command.CommandId || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || result.Error != nil {
			return fmt.Errorf("simulated volatile command refused/status=%d/%s", response.StatusCode, result.Status)
		}
		summary.Commands++
	}
	return nil
}

type nha165Percentiles struct {
	Count int   `json:"count"`
	P50   int64 `json:"p50_ns"`
	P95   int64 `json:"p95_ns"`
	P99   int64 `json:"p99_ns"`
	Max   int64 `json:"max_ns"`
}

func nha165Percentile(values []int64) nha165Percentiles {
	if len(values) == 0 {
		return nha165Percentiles{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	rank := func(p float64) int64 { return values[int(math.Ceil(float64(len(values))*p))-1] }
	return nha165Percentiles{len(values), rank(.5), rank(.95), rank(.99), values[len(values)-1]}
}
func nha165ReadRecords(path string, consume func(nha165Record) error) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var r nha165Record
		if e := json.Unmarshal(scanner.Bytes(), &r); e != nil {
			return e
		}
		if e := consume(r); e != nil {
			return e
		}
	}
	return scanner.Err()
}

type nha165Report struct {
	Status              string                       `json:"status"`
	Profile             nha165Profile                `json:"profile"`
	CompleteHour        bool                         `json:"complete_hour"`
	Observation         map[string]nha165Percentiles `json:"host_publish_to_fast_authorized_decoded_frame"`
	Command             map[string]nha165Percentiles `json:"volatile_service_dispatch_to_fake_driver"`
	Scheduling          map[string]nha165Percentiles `json:"open_loop_scheduling_lateness"`
	LateOverOnePeriod   int                          `json:"scheduled_observations_late_over_one_period"`
	SourceSession       string                       `json:"source_session"`
	Core                nha165CoreSummary            `json:"core"`
	Clients             nha165ClientSummary          `json:"clients"`
	MaxRSS              uint64                       `json:"core_max_rss_bytes"`
	CorroboratingMaxRSS uint64                       `json:"external_core_max_rss_bytes"`
	Auxiliary           string                       `json:"auxiliary_accounting"`
	Method              string                       `json:"method"`
	RealObservation     string                       `json:"NHA166"`
	ManualActions       string                       `json:"NHA167"`
	Failure             []string                     `json:"failures"`
	BinarySHA           string                       `json:"test_binary_sha256"`
	Versions            map[string]string            `json:"versions"`
}

func nha165Merge(proof string, p nha165Profile, externalRSS uint64) (nha165Report, error) {
	r := nha165Report{Status: "FAILED", Profile: p, CompleteHour: p.Name == "acceptance", Observation: map[string]nha165Percentiles{}, Command: map[string]nha165Percentiles{}, Scheduling: map[string]nha165Percentiles{}, CorroboratingMaxRSS: externalRSS, Auxiliary: "PostgreSQL, supervisor and ten clients are distinct OS processes, outside core RSS. External PID/birth and RSS samples are in process-resources.jsonl (Linux proc stat starttime/status VmRSS, Darwin ps lstart/rss; birth checked around each sample); core CPU, TotalAlloc, GC, goroutines and bounded queue/lag samples are in core.jsonl.", Method: "CLOCK_MONOTONIC common OS domain, ns. Host timestamp immediately before Session.Publish; client0 (chosen before run) timestamps immediately after Receive/decode. Join exact source_session/source_seq, require every scheduled observation from all10 readers, no resync/omitted samples. Volatile start immediately before SAME Run-Host service Dispatch with native principal and current Owner generation; end at fake driver entry, exact command ID. Public durable API is separate and is exercised by delivered114/147. Exact nearest-rank ceil(p*N), no averaged batch percentiles. Darwin Getrusage Maxrss bytes; Linux KiB*1024; no baseline subtraction, forced GC or GOGC change. Resource sampling1Hz plus phase boundaries.", RealObservation: "NOT_PERFORMED: NHA166 real contiguous24h observation is a separate gate", ManualActions: "NOT_PERFORMED: NHA167 actions require Damien; all165 outputs are in-memory fake drivers on loopback", Versions: map[string]string{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "clock": "CLOCK_MONOTONIC", "logical_cpus": strconv.Itoa(runtime.NumCPU())}}
	if e := nha165ReadJSON(filepath.Join(proof, "core-final.json"), &r.Core); e != nil {
		return r, e
	}
	if e := nha165ReadJSON(filepath.Join(proof, "clients-done.json"), &r.Clients); e != nil {
		return r, e
	}
	r.SourceSession = r.Core.SourceSession
	r.MaxRSS = r.Core.MaxRSS
	r.Versions["go_soft_memory_limit_bytes"] = strconv.FormatInt(r.Core.GoMemoryLimit, 10)
	r.Method += " Permanent homed process policy (shared exact processbudget.ConfigureServer) caps Go-managed soft memory at160MiB and preserves stricter operator settings. This controls Sys-HeapReleased, not totalRSS; binary/external memory remains included in measured RSS. GOGC remains default and no GC is forced."
	r.Method += " Stream rotation is owned by a4minute local timer/WithCancelCause, never a4minute RPC deadline. Only its exact cancellation cause permits native RefreshSession and replay from the last delivered public cursor; remote DeadlineExceeded, parent cancellation, resync and sequence gaps fail. Every run longer than4minutes requires native refresh and validated cursor resume for each of10clients, plus command-session refresh."
	if p.Name == "rotation_NOT_ONE_HOUR" {
		r.Method += " Dedicated252second rotation regression:250seconds at100observations/s plus2seconds at1000/s,27000observations per client and252commands/edges; NOT the3660second acceptance and NOT AC2/hour qualification."
	}
	if r.MaxRSS == 0 || externalRSS == 0 {
		r.Failure = append(r.Failure, "missing positive core RSS evidence")
	}
	obs := make(map[uint64]nha165Record, p.count())
	starts := make(map[string]int64, p.seconds())
	ends := make(map[string]int64, p.seconds())
	latency := map[string][]int64{"sustained": {}, "burst": {}, "whole_run": {}}
	scheduling := map[string][]int64{"sustained": {}, "burst": {}, "whole_run": {}}
	if e := nha165ReadRecords(filepath.Join(proof, "core.jsonl"), func(v nha165Record) error {
		switch v.Kind {
		case "observation":
			if _, ok := obs[v.Sequence]; ok {
				return errors.New("duplicate core sequence")
			}
			obs[v.Sequence] = v
			late := v.At - v.Scheduled
			if late < 0 {
				return errors.New("early open-loop publication")
			}
			scheduling[v.Phase] = append(scheduling[v.Phase], late)
			scheduling["whole_run"] = append(scheduling["whole_run"], late)
			rate := p.SustainedRate
			if v.Phase == "burst" {
				rate = p.BurstRate
			}
			if late > int64(time.Second)/int64(rate) {
				r.LateOverOnePeriod++
			}
		case "command":
			if _, ok := starts[v.Command]; ok {
				return errors.New("duplicate command admission sample")
			}
			starts[v.Command] = v.At
		case "driver":
			if _, ok := ends[v.Command]; ok {
				return errors.New("duplicate driver dispatch")
			}
			ends[v.Command] = v.At
		case "resource":
			r.MaxRSS = max(r.MaxRSS, v.RSS)
		}
		return nil
	}); e != nil {
		return r, e
	}
	received := make(map[uint64]bool, p.count())
	if e := nha165ReadRecords(filepath.Join(proof, "fast-client.jsonl"), func(v nha165Record) error {
		if v.Kind == "client_progress" {
			return nil
		}
		if v.Kind != "received" {
			return errors.New("unexpected client record")
		}
		if v.Session != r.SourceSession {
			return errors.New("client/Host source session mismatch")
		}
		o, ok := obs[v.Sequence]
		if !ok || received[v.Sequence] {
			return errors.New("missing/duplicate latency join")
		}
		if v.At < o.At {
			return errors.New("CLOCK_MONOTONIC domains disagree")
		}
		received[v.Sequence] = true
		d := v.At - o.At
		latency[o.Phase] = append(latency[o.Phase], d)
		latency["whole_run"] = append(latency["whole_run"], d)
		return nil
	}); e != nil {
		return r, e
	}
	if len(obs) != p.count() || len(received) != p.count() || r.Core.Count != p.count() {
		r.Failure = append(r.Failure, "scheduled/admitted/received observation count mismatch")
	}
	for i := 0; i < nha165Clients; i++ {
		if r.Clients.Counts[i] != p.count() || r.Clients.SnapshotEntities[i] != nha165Entities || r.Clients.Sessions[i] != r.SourceSession {
			r.Failure = append(r.Failure, fmt.Sprintf("client%d incomplete", i))
		}
		if p.seconds() > 240 && (r.Clients.Refreshes[i] < 1 || r.Clients.Rotations[i] < 1 || r.Clients.Refreshes[i] != r.Clients.Rotations[i]) {
			r.Failure = append(r.Failure, fmt.Sprintf("client%d native refresh/cursor resume not demonstrated", i))
		}
	}
	if p.seconds() > 240 && r.Clients.CommandRotations < 1 {
		r.Failure = append(r.Failure, "command native session refresh/resume not demonstrated")
	}
	commandSamples := map[string][]int64{"sustained": {}, "burst": {}, "whole_run": {}}
	for id, start := range starts {
		end, ok := ends[id]
		if !ok || end < start {
			r.Failure = append(r.Failure, "missing/inverted command sample "+id)
			continue
		}
		phase := "sustained"
		if start >= r.Core.Start+int64(p.SustainedSeconds)*int64(time.Second) {
			phase = "burst"
		}
		d := end - start
		commandSamples[phase] = append(commandSamples[phase], d)
		commandSamples["whole_run"] = append(commandSamples["whole_run"], d)
	}
	if len(starts) != p.seconds() || len(ends) != p.seconds() || r.Clients.Commands != p.seconds() || r.Core.Dispatches != uint64(p.seconds()) {
		r.Failure = append(r.Failure, "volatile admissions/driver/client result counts mismatch")
	}
	if r.Core.Edges != uint64(p.seconds()) {
		r.Failure = append(r.Failure, "device edge accounting mismatch")
	}
	for name, v := range latency {
		r.Observation[name] = nha165Percentile(v)
		if len(v) == 0 || r.Observation[name].P95 > nha165LatencyLimit {
			r.Failure = append(r.Failure, "observation p95 failed: "+name)
		}
	}
	for name, v := range commandSamples {
		r.Command[name] = nha165Percentile(v)
		if len(v) == 0 || r.Command[name].P95 > nha165LatencyLimit {
			r.Failure = append(r.Failure, "volatile p95 failed: "+name)
		}
	}
	for name, v := range scheduling {
		r.Scheduling[name] = nha165Percentile(v)
	}
	if r.Scheduling["whole_run"].Max >= int64(time.Second) {
		r.Failure = append(r.Failure, "open-loop injection missed a fullsecond; declared rates not demonstrated")
	}
	if r.MaxRSS > nha165RSSLimit || externalRSS > nha165RSSLimit {
		r.Failure = append(r.Failure, "core RSS exceeded256MiB")
	}
	if r.Core.End-r.Core.Start < int64(p.seconds())*int64(time.Second) {
		r.Failure = append(r.Failure, "declared elapsed workload duration not reached")
	}
	if len(r.Failure) == 0 {
		if r.CompleteHour {
			r.Status = "SOFTWARE_PROFILE_PASSED_COVERAGE_AND_FULL_GATE_REQUIRED"
		} else if p.Name == "rotation_NOT_ONE_HOUR" {
			r.Status = "ROTATION_PASSED_NOT_ONE_HOUR_NOT_AC2"
		} else {
			r.Status = "PREFLIGHT_PASSED_NOT_ONE_HOUR_NOT_AC2"
		}
	}
	return r, nil
}

// The supervisor is excluded from measured RSS and owns/joins only its two
// direct helper children. The outer qualified controller owns the whole cohort.
func TestNHA165SoftwareProfile(t *testing.T) {
	if os.Getenv("HOMEWARD_NHA165_PROFILE") == "" {
		t.Skip("run make qualify-mvp-software for explicit hour profile")
	}
	if os.Getenv("HOMEWARD_NHA165_HELPER") != "" {
		t.Fatal("supervisor may not run as child")
	}
	p, e := nha165SelectedProfile()
	if e != nil {
		t.Fatal(e)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "-race" && setting.Value == "true" {
				t.Fatal("RSS/latency profile requires non-race binary; make check is separate")
			}
		}
	}
	proof := nha165Proof(t)
	if e := os.Mkdir(proof, 0700); e != nil {
		t.Fatal("fresh proof directory required", e)
	}
	// Never alter GC settings to fit the acceptance RSS budget.
	// Qualification uses the delivered product startup memory policy; operator
	// GOGC/GOMEMLIMIT overrides may not select a more favorable test setting.
	if os.Getenv("GOGC") != "" || os.Getenv("GOMEMLIMIT") != "" {
		t.Fatal("profile uses product startup budget; GOGC/GOMEMLIMIT overrides forbidden for qualification")
	}
	if os.Getenv("HOMEWARD_NHA165_DIAGNOSTICS") != "" {
		t.Fatal("preparation diagnostic experiment must not leak into acceptance/preflight")
	}
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(b)
	binarySHA := hex.EncodeToString(hash[:])
	b = nil
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	nha181CertificateFixtureFor(t, data, time.Now().Add(-time.Hour), time.Now().Add(3*time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 74*time.Minute)
	defer cancel()
	type child struct {
		command  *exec.Cmd
		done     chan error
		log      *os.File
		finished bool
	}
	launch := func(role, test string) *child {
		t.Helper()
		log, e := os.OpenFile(filepath.Join(proof, role+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.CommandContext(ctx, binary, "-test.run=^"+test+"$", "-test.count=1", "-test.v", "-test.timeout=73m")
		cmd.Env = append(os.Environ(), "HOMEWARD_NHA165_HELPER="+role, "HOMEWARD_NHA165_PG_PORT="+strconv.Itoa(cluster.Port), "HOMEWARD_NHA165_DATA="+data)
		cmd.Stdout = log
		cmd.Stderr = log
		if e := cmd.Start(); e != nil {
			log.Close()
			t.Fatal(e)
		}
		c := &child{command: cmd, done: make(chan error, 1), log: log}
		go func() { c.done <- cmd.Wait() }()
		t.Cleanup(func() {
			if !c.finished {
				_ = cmd.Process.Kill()
				select {
				case <-c.done:
				case <-time.After(10 * time.Second):
					t.Error("helper did not join", role)
				}
			}
			log.Close()
		})
		return c
	}
	core := launch("core", "TestNHA165CoreHelper")
	readyCtx, rc := context.WithTimeout(ctx, 60*time.Second)
	defer rc()
	if e := nha165WaitFile(readyCtx, filepath.Join(proof, "ready.private.json")); e != nil {
		t.Fatal("core readiness", e)
	}
	var ready nha165Ready
	if e := nha165ReadJSON(filepath.Join(proof, "ready.private.json"), &ready); e != nil {
		t.Fatal(e)
	}
	if ready.PID != core.command.Process.Pid {
		t.Fatal("core PID identity mismatch")
	}
	clients := launch("clients", "TestNHA165ClientsHelper")
	resources := nha165NewLog(t, filepath.Join(proof, "process-resources.jsonl"))
	born, maxRSS, e := nha165ProcessSample(ready.PID)
	if e != nil {
		t.Fatal("core birth identity/RSS", e)
	}
	resources.put(nha165Record{Kind: "external_core_rss", At: nha165Mono(), RSS: maxRSS})
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	coreDone, clientsDone := false, false
	for !coreDone || !clientsDone {
		select {
		case e := <-core.done:
			core.finished = true
			coreDone = true
			core.done = nil
			if e != nil {
				t.Fatalf("core helper failed: %v; see core.log", e)
			}
		case e := <-clients.done:
			clients.finished = true
			clientsDone = true
			clients.done = nil
			if e != nil {
				t.Fatalf("client helper failed: %v; see clients.log", e)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
			if coreDone {
				continue
			}
			identity, rss, e := nha165ProcessSample(ready.PID)
			if e != nil {
				select {
				case doneErr := <-core.done:
					core.finished = true
					coreDone = true
					core.done = nil
					if doneErr != nil {
						t.Fatal("core helper", doneErr)
					}
					continue
				default:
					t.Fatal("core PID identity/RSS", e)
				}
			}
			if identity != born {
				t.Fatal("core PID birth identity changed")
			}
			maxRSS = max(maxRSS, rss)
			resources.put(nha165Record{Kind: "external_core_rss", At: nha165Mono(), RSS: rss})
		}
	}
	if e := resources.close(); e != nil {
		t.Fatal(e)
	}
	report, e := nha165Merge(proof, p, maxRSS)
	if e != nil {
		t.Fatal("measurement merge", e)
	}
	report.BinarySHA = binarySHA
	if e := nha165WriteJSON(filepath.Join(proof, "report.json"), report); e != nil {
		t.Fatal(e)
	}
	if len(report.Failure) != 0 {
		t.Fatalf("profile failed: %v; see report.json", report.Failure)
	}
	t.Logf("%s: observations=%d clients=%d coreRSS=%d observationp95=%dns volatilep95=%dns proof=%s", report.Status, p.count(), nha165Clients, report.MaxRSS, report.Observation["whole_run"].P95, report.Command["whole_run"].P95, proof)
}

func TestNHA165MeasurementRejectsOmissionAndUsesExactRank(t *testing.T) {
	samples := []int64{100, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	got := nha165Percentile(samples)
	if got.P95 != 19 || got.P99 != 100 || got.Max != 100 || got.Count != 20 {
		t.Fatalf("nearest ranks: %+v", got)
	}
	before := nha165Mono()
	time.Sleep(time.Millisecond)
	if nha165Mono() <= before {
		t.Fatal("CLOCK_MONOTONIC did not advance")
	}
	proof := t.TempDir()
	p := nha165Profile{"preflight_NOT_ONE_HOUR", 1, 1, 1, 1}
	core := nha165CoreSummary{Profile: p, Count: 2, Start: 1, End: int64(2*time.Second) + 1, Edges: 2, Dispatches: 2, SourceSession: "fixture", MaxRSS: 1}
	if e := nha165WriteJSON(filepath.Join(proof, "core-final.json"), core); e != nil {
		t.Fatal(e)
	}
	var clients nha165ClientSummary
	for i := 0; i < nha165Clients; i++ {
		clients.Counts[i] = 2
		clients.Sessions[i] = "fixture"
		clients.SnapshotEntities[i] = 850
	}
	clients.Commands = 2
	if e := nha165WriteJSON(filepath.Join(proof, "clients-done.json"), clients); e != nil {
		t.Fatal(e)
	}
	log := nha165NewLog(t, filepath.Join(proof, "core.jsonl"))
	for i := 0; i < 2; i++ {
		at := int64(i)*int64(time.Second) + 2
		phase := []string{"sustained", "burst"}[i]
		id := fmt.Sprintf("fixture-%d", i)
		log.put(nha165Record{Kind: "observation", Sequence: nha165FirstSequence + uint64(i), At: at, Scheduled: at, Phase: phase})
		log.put(nha165Record{Kind: "command", At: at, Command: id})
		log.put(nha165Record{Kind: "driver", At: at + 1, Command: id})
	}
	if e := log.close(); e != nil {
		t.Fatal(e)
	}
	fast := nha165NewLog(t, filepath.Join(proof, "fast-client.jsonl"))
	first := nha165Record{Kind: "received", Sequence: nha165FirstSequence, Session: "fixture", At: 3}
	fast.put(first)
	fast.put(nha165Record{Kind: "received", Sequence: nha165FirstSequence + 1, Session: "fixture", At: int64(time.Second) + 3})
	if e := fast.close(); e != nil {
		t.Fatal(e)
	}
	valid, e := nha165Merge(proof, p, 1)
	if e != nil {
		t.Fatal(e)
	}
	if len(valid.Failure) != 0 || valid.Status != "PREFLIGHT_PASSED_NOT_ONE_HOUR_NOT_AC2" {
		t.Fatalf("complete fixture must pass before omission mutation: %+v", valid.Failure)
	}
	// Change exactly one input: omit the second (burst) received sample. A valid
	// baseline then fails rather than letting percentile selection hide the gap.
	encoded, e := json.Marshal(first)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(proof, "fast-client.jsonl"), append(encoded, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	missing, e := nha165Merge(proof, p, 1)
	if e != nil {
		t.Fatal(e)
	}
	if missing.Status != "FAILED" || len(missing.Failure) == 0 {
		t.Fatal("omitted received record passed")
	}
}
