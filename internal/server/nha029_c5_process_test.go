package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Child helpers never own PostgreSQL. All addresses/paths and the nonce come
// from the parent which owns the actual exec.Cmd, cluster, proxy and pipes.
// The fixed Logic1 IR below is only an execution/projection fixture, not a
// persisted definition recovered from a SQL configuration manifest (NHA-115).
const nha029C5ParentBody = nha042ServerCommand + nha029OwnerParent

type nha029C5Kind uint8

const (
	nha029C5Invalid nha029C5Kind = iota
	nha029C5Admitted
	nha029C5Ready
	nha029C5Effect
	nha029C5Waits
	nha029C5Activated
	nha029C5Status
	nha029C5Stopped
	nha029C5Projection
	nha029C5Failure
	nha029C5Ack
	nha029C5Fire
	nha029C5ReleaseWait
	nha029C5AllowEffect
	nha029C5Activate
	nha029C5Reconcile
	nha029C5ReadStatus
	nha029C5Stop
	nha029C5ActivateProjection
	nha029C5ReadRecoveredSQL
)

type nha029C5Message struct {
	kind     nha029C5Kind
	revision uint64
	manifest [32]byte
	digest   [32]byte
	count    uint64
	value    string
}
type nha029C5Wire struct {
	mu     sync.Mutex
	writer io.Writer
	nonce  [32]byte
	next   uint64
}

func (w *nha029C5Wire) send(message nha029C5Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if message.kind <= nha029C5Invalid || message.kind > nha029C5ReadRecoveredSQL || len(message.value) > 1024 || strings.ContainsRune(message.value, '\x00') {
		return errors.New("invalid C5 message")
	}
	w.next++
	// A fixed private envelope, then one bounded string. No source, key,
	// principal, DSN, action argument or arbitrary JSON crosses these pipes.
	frame := make([]byte, 4+1+8+32+8+32+32+8+2+len(message.value))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(frame)-4))
	frame[4] = byte(message.kind)
	binary.BigEndian.PutUint64(frame[5:13], w.next)
	copy(frame[13:45], w.nonce[:])
	binary.BigEndian.PutUint64(frame[45:53], message.revision)
	copy(frame[53:85], message.manifest[:])
	copy(frame[85:117], message.digest[:])
	binary.BigEndian.PutUint64(frame[117:125], message.count)
	binary.BigEndian.PutUint16(frame[125:127], uint16(len(message.value)))
	copy(frame[127:], message.value)
	return nha029OwnerWriteAll(w.writer, frame)
}
func nha029C5Read(reader io.Reader, nonce [32]byte, next *uint64) (nha029C5Message, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nha029C5Message{}, err
	}
	size := int(binary.BigEndian.Uint32(header[:]))
	if size < 123 || size > 4096 {
		return nha029C5Message{}, errors.New("invalid C5 IPC length")
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nha029C5Message{}, err
	}
	var gotNonce [32]byte
	copy(gotNonce[:], frame[9:41])
	sequence := binary.BigEndian.Uint64(frame[1:9])
	kind := nha029C5Kind(frame[0])
	length := int(binary.BigEndian.Uint16(frame[121:123]))
	if gotNonce != nonce || sequence != *next+1 || kind <= nha029C5Invalid || kind > nha029C5ReadRecoveredSQL || length > 1024 || len(frame) != 123+length {
		return nha029C5Message{}, errors.New("invalid C5 IPC identity or phase")
	}
	*next = sequence
	message := nha029C5Message{kind: kind, revision: binary.BigEndian.Uint64(frame[41:49]), count: binary.BigEndian.Uint64(frame[113:121]), value: string(frame[123:])}
	copy(message.manifest[:], frame[49:81])
	copy(message.digest[:], frame[81:113])
	if strings.ContainsRune(message.value, '\x00') {
		return nha029C5Message{}, errors.New("invalid C5 IPC text")
	}
	return message, nil
}

// The owned child's stdout/stderr are diagnostics only. Copies are bounded;
// the process protocol remains on its two separate nonce-checked pipes.
type nha029C5ChildLog struct {
	mu    sync.Mutex
	data  [64 << 10]byte
	next  int
	size  int
	total uint64
}

func (b *nha029C5ChildLog) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, value := range raw {
		b.data[b.next] = value
		b.next = (b.next + 1) % len(b.data)
		if b.size < len(b.data) {
			b.size++
		}
	}
	b.total += uint64(len(raw))
	return len(raw), nil
}
func (b *nha029C5ChildLog) snapshot() (string, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw := make([]byte, b.size)
	start := (b.next - b.size + len(b.data)) % len(b.data)
	copied := copy(raw, b.data[start:])
	copy(raw[copied:], b.data[:b.size-copied])
	return string(raw), b.total
}

type nha029C5Process struct {
	fixture       *nha029C5Fixture
	command       *exec.Cmd
	diagnostics   *nha029C5ChildLog
	control       *os.File
	reports       *os.File
	wire          *nha029C5Wire
	messages      chan nha029C5Message
	readerDone    chan struct{}
	readerStop    chan struct{}
	done          chan struct{}
	err           error
	readErr       error
	stopOnce      sync.Once
	journalBefore int
}
type nha029C5Fixture struct {
	ctx                                 context.Context
	workspace, path, dataDir, bootstrap string
	backendPort                         int
	proxy                               *nha029C5SQLProxy
	store                               *storage.Store
	observer                            *pgx.Conn
	raw                                 []byte
	effects                             map[string]int
}

func nha029C5NewFixture(t *testing.T) *nha029C5Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	cluster := testpg.Start(t)
	f := &nha029C5Fixture{ctx: ctx, workspace: t.TempDir(), dataDir: shortPrivateDataDir(t), backendPort: cluster.Port, effects: make(map[string]int)}
	f.path = filepath.Join(f.workspace, "configuration.yaml")
	f.proxy = nha029C5StartSQLProxy(t, cluster.Port)
	f.raw = []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n# operational source A\n", f.dataDir, f.proxy.port()))
	if err := os.WriteFile(f.path, f.raw, 0600); err != nil {
		t.Fatal(err)
	}
	directRaw := strings.Replace(string(f.raw), "  port: "+strconv.Itoa(f.proxy.port())+"\n", "  port: "+strconv.Itoa(cluster.Port)+"\n", 1)
	parsed, err := config.ParseWithEnv([]byte(directRaw), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parsed.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.store.Close)
	f.observer = nha030ServerObserver(t, cluster.Port)
	return f
}
func (f *nha029C5Fixture) edit(t *testing.T, comment string) {
	t.Helper()
	if strings.ContainsAny(comment, "\r\n") {
		t.Fatal("invalid C5 comment")
	}
	raw := append(append([]byte(nil), f.raw...), []byte("# "+comment+"\n")...)
	if err := os.WriteFile(f.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *nha029C5Fixture) active(t *testing.T) storage.ConfigGenerationSnapshot {
	t.Helper()
	active, present, err := f.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatalf("C5 SQL authority unavailable: present=%t err=%v", present, err)
	}
	return active
}
func (f *nha029C5Fixture) start(t *testing.T, configured bool) *nha029C5Process {
	t.Helper()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outputR, outputW, err := os.Pipe()
	if err != nil {
		_ = inputR.Close()
		_ = inputW.Close()
		t.Fatal(err)
	}
	p := &nha029C5Process{fixture: f, control: inputW, reports: outputR, messages: make(chan nha029C5Message, 16),
		readerDone: make(chan struct{}), readerStop: make(chan struct{}), done: make(chan struct{}), journalBefore: len(f.effects)}
	p.wire = &nha029C5Wire{writer: inputW, nonce: nonce}
	mode := "ir_fixture"
	if configured {
		mode = "configured_sql"
	}
	p.command = exec.Command(os.Args[0], "-test.run=^TestNHA029C5ProcessHelper$", "-test.count=1", "-test.timeout=120s", "-test.v")
	p.command.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.workspace,
		"NHA029_C5_CHILD=" + hex.EncodeToString(nonce[:]), "NHA029_C5_SOURCE=" + f.path,
		"NHA029_C5_BOOTSTRAP=" + f.bootstrap, "NHA029_C5_BACKEND=" + strconv.Itoa(f.backendPort), "NHA029_C5_MODE=" + mode}
	p.command.ExtraFiles = []*os.File{inputR, outputW}
	// The protocol is on owned extra pipes, never stdout. Test output is kept
	// bounded and is not a transport for source/credential data.
	p.diagnostics = &nha029C5ChildLog{}
	p.command.Stdout, p.command.Stderr = p.diagnostics, p.diagnostics
	if err := p.command.Start(); err != nil {
		_ = inputR.Close()
		_ = inputW.Close()
		_ = outputR.Close()
		_ = outputW.Close()
		t.Fatal(err)
	}
	_ = inputR.Close()
	_ = outputW.Close()
	go func() { p.err = p.command.Wait(); close(p.done) }()
	go func() {
		defer close(p.readerDone)
		defer close(p.messages)
		var next uint64
		for {
			message, err := nha029C5Read(outputR, nonce, &next)
			if err != nil {
				p.readErr = err
				return
			}
			select {
			case p.messages <- message:
			case <-p.readerStop:
				return
			}
		}
	}()
	t.Cleanup(func() { p.dispose(t) })
	return p
}
func (p *nha029C5Process) send(t *testing.T, message nha029C5Message) {
	t.Helper()
	if err := p.wire.send(message); err != nil {
		t.Fatalf("C5 parent control failed: %v", err)
	}
}
func (p *nha029C5Process) next(t *testing.T) nha029C5Message {
	t.Helper()
	select {
	case message, open := <-p.messages:
		if !open {
			<-p.readerDone
			waitState := "pending after diagnostic deadline"
			exitCode := -1
			waitError := "not joined"
			select {
			case <-p.done:
				waitState = "terminal"
				exitCode = p.command.ProcessState.ExitCode()
				waitError = fmt.Sprint(p.err)
			case <-time.After(5 * time.Second):
			}
			output, total := p.diagnostics.snapshot()
			t.Fatalf("C5 child report ended: %v; pid=%d wait=%q exit_code=%d wait_error=%q child_output_total=%d retained=%d child_output=%q",
				p.readErr, p.command.Process.Pid, waitState, exitCode, waitError, total, len(output), output)
		}
		if message.kind == nha029C5Failure {
			t.Fatalf("C5 child refused phase %s", message.value)
		}
		return message
	case <-p.fixture.ctx.Done():
		t.Fatal("C5 child phase deadline")
	}
	return nha029C5Message{}
}
func (p *nha029C5Process) recordEffect(t *testing.T, message nha029C5Message, allow bool) {
	t.Helper()
	if message.kind != nha029C5Effect || message.value == "" || len(message.value) > 255 {
		t.Fatal("invalid C5 effect identity")
	}
	var state, origin string
	var actor *string
	if err := p.fixture.observer.QueryRow(p.fixture.ctx, `SELECT state,origin_kind::text,actor_id FROM public.command_intents WHERE command_id=$1`, message.value).Scan(&state, &origin, &actor); err != nil || state != "dispatching" || origin != "internal_automation" || actor != nil {
		t.Fatalf("C5 effect preceded internal durable ledger: state=%s origin=%s err=%v", state, origin, err)
	}
	p.fixture.effects[message.value]++
	if p.fixture.effects[message.value] != 1 {
		t.Fatal("C5 replayed the same simulated command effect")
	}
	if allow {
		p.send(t, nha029C5Message{kind: nha029C5AllowEffect, value: message.value})
	}
}
func (p *nha029C5Process) await(t *testing.T, kind nha029C5Kind) nha029C5Message {
	t.Helper()
	for {
		message := p.next(t)
		if message.kind == nha029C5Effect {
			p.recordEffect(t, message, true)
			continue
		}
		if message.kind == nha029C5Ack {
			continue
		}
		if message.kind != kind {
			t.Fatalf("C5 phase mismatch: got=%d wanted=%d", message.kind, kind)
		}
		return message
	}
}
func (p *nha029C5Process) ready(t *testing.T, expected *storage.ConfigGenerationSnapshot) nha029C5Message {
	t.Helper()
	admitted := p.await(t, nha029C5Admitted)
	if admitted.revision == 0 || admitted.manifest == ([32]byte{}) || admitted.digest == ([32]byte{}) {
		t.Fatal("C5 Run did not admit exact SQL source")
	}
	if expected != nil && (admitted.revision != uint64(expected.Revision().Number()) || admitted.manifest != expected.ManifestSHA256()) {
		t.Fatal("C5 restart admitted a different SQL generation")
	}
	ready := p.await(t, nha029C5Ready)
	if ready.revision != admitted.revision || ready.manifest != admitted.manifest || ready.digest != admitted.digest {
		t.Fatal("C5 readiness preceded coherent SQL admission")
	}
	if len(p.fixture.effects) != p.journalBefore {
		t.Fatal("C5 startup replayed a simulated effect")
	}
	return ready
}
func (p *nha029C5Process) fireWaits(t *testing.T) {
	t.Helper()
	p.send(t, nha029C5Message{kind: nha029C5Fire})
	p.await(t, nha029C5Waits)
}
func (p *nha029C5Process) status(t *testing.T, available bool) nha029C5Message {
	t.Helper()
	p.send(t, nha029C5Message{kind: nha029C5ReadStatus})
	message := p.await(t, nha029C5Status)
	if (message.count == 0) != available {
		t.Fatalf("C5 admission availability: status=%d wanted=%t", message.count, available)
	}
	return message
}
func (p *nha029C5Process) kill(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Kill(); err != nil {
		t.Fatalf("kill owned C5 child: %v", err)
	}
	select {
	case <-p.done:
	case <-p.fixture.ctx.Done():
		t.Fatal("owned C5 child did not join after SIGKILL")
	}
	status, ok := p.command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("C5 child did not terminate at the owned crash")
	}
}
func (p *nha029C5Process) stop(t *testing.T) {
	t.Helper()
	p.send(t, nha029C5Message{kind: nha029C5Stop})
	p.await(t, nha029C5Stopped)
	select {
	case <-p.done:
	case <-p.fixture.ctx.Done():
		t.Fatal("C5 graceful Run did not join")
	}
	if p.err != nil {
		t.Fatalf("C5 child Run failed: %v", p.err)
	}
}
func (p *nha029C5Process) dispose(t *testing.T) {
	t.Helper()
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
		default:
			_ = p.command.Process.Kill()
		}
		_ = p.control.Close()
		_ = p.reports.Close()
		close(p.readerStop)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		select {
		case <-p.done:
		case <-ctx.Done():
			t.Error("C5 child process not joined")
		}
		select {
		case <-p.readerDone:
		case <-ctx.Done():
			t.Error("C5 child report reader not joined")
		}
	})
}
func nha029C5Eventually(ctx context.Context, predicate func() (bool, error)) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := predicate()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func nha029C5Barrier(t *testing.T, f *nha029C5Fixture, ticket *nha029C5SQLTicket) nha029C5SQLBarrier {
	t.Helper()
	select {
	case barrier := <-ticket.held:
		if barrier.pid == 0 {
			t.Fatal("C5 barrier has no owned writer backend")
		}
		return barrier
	case <-f.ctx.Done():
		t.Fatal("real PostgreSQL barrier not reached")
	}
	return nha029C5SQLBarrier{}
}
func (f *nha029C5Fixture) waitBackendGone(t *testing.T, pid uint32) {
	t.Helper()
	if err := nha029C5Eventually(f.ctx, func() (bool, error) {
		var absent bool
		err := f.observer.QueryRow(f.ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&absent)
		return absent, err
	}); err != nil {
		t.Fatalf("owned writer backend not joined: %v", err)
	}
}
func (f *nha029C5Fixture) counts(t *testing.T) [3]int64 {
	t.Helper()
	var counts [3]int64
	if err := f.observer.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_activation_attempts),(SELECT count(*) FROM public.command_intents)`).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func (f *nha029C5Fixture) runtimeBackend(t *testing.T) uint32 {
	t.Helper()
	var count int
	var pid uint32
	if err := f.observer.QueryRow(f.ctx, `SELECT count(*),COALESCE(min(pid),0) FROM pg_locks WHERE locktype='advisory' AND classid=481316 AND objid=24 AND objsubid=2 AND granted`).Scan(&count, &pid); err != nil {
		t.Fatal(err)
	}
	if count != 1 || pid == 0 {
		t.Fatalf("C5 requires the actual single durable runtime session: count=%d pid=%d", count, pid)
	}
	return pid
}

// The effect notification blocks this fake transport until the owning parent
// confirms the real dispatching row. Effects are counted outside the child and
// survive its SIGKILL; startup never gets a new empty journal as its oracle.
type nha029C5Driver struct {
	wire     *nha029C5Wire
	observer *pgx.Conn
	mu       sync.Mutex
	pending  map[string]chan struct{}
}

func (d *nha029C5Driver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrInvalidInput
	}
	id, _, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	value, err := id.Value()
	if err != nil {
		return err
	}
	var state string
	d.mu.Lock()
	if len(d.pending) >= 64 {
		d.mu.Unlock()
		return integrations.ErrInvalidInput
	}
	err = d.observer.QueryRow(ctx, `SELECT state FROM public.command_intents WHERE command_id=$1`, value).Scan(&state)
	if err != nil || state != "dispatching" {
		d.mu.Unlock()
		return errors.New("C5 fake effect preceded durable dispatch")
	}
	if _, duplicate := d.pending[value]; duplicate {
		d.mu.Unlock()
		return errors.New("C5 duplicate fake effect")
	}
	release := make(chan struct{})
	d.pending[value] = release
	d.mu.Unlock()
	if err := d.wire.send(nha029C5Message{kind: nha029C5Effect, value: value}); err != nil {
		return err
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (d *nha029C5Driver) allow(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	release, present := d.pending[id]
	if !present {
		return errors.New("C5 foreign effect ACK")
	}
	delete(d.pending, id)
	close(release)
	return nil
}
func (d *nha029C5Driver) releaseAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, release := range d.pending {
		close(release)
		delete(d.pending, id)
	}
}

type nha029C5Pilot struct {
	mu      sync.Mutex
	host    *integrations.Manager
	key     integrations.SourceKey
	driver  *nha029C5Driver
	session *integrations.Session
	binding integrations.Binding
	ready   chan struct{}
	once    sync.Once
}

func (p *nha029C5Pilot) Start(ctx context.Context, source integrations.SessionHost, _ integrations.Tasks) error {
	session, ok := source.(*integrations.Session)
	if !ok {
		return integrations.ErrInvalidInput
	}
	candidate, err := integrations.NewCandidate(p.key, []*registrywire.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "C5 simulated transport", Constraints: &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{SetOn: true}}}})
	if err != nil {
		return err
	}
	bindings, err := source.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		return errors.Join(integrations.ErrInvalidInput, err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	value := domain.NewSwitchValue(false)
	at := time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	if err := source.Publish(ctx, bindings[0], observation); err != nil {
		return err
	}
	if err := p.host.AttachCommandDriver(session, p.driver); err != nil {
		return err
	}
	p.mu.Lock()
	p.session, p.binding = session, bindings[0]
	p.mu.Unlock()
	p.once.Do(func() { close(p.ready) })
	return nil
}
func (*nha029C5Pilot) Stop(context.Context) error { return nil }
func (p *nha029C5Pilot) emit(ctx context.Context, name string) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	session, binding := p.session, p.binding
	p.mu.Unlock()
	event, err := integrations.NewDeviceEvent(name, time.Now(), nil)
	if err != nil {
		return err
	}
	return session.Emit(ctx, binding, event)
}

type nha029C5Child struct {
	ctx         context.Context
	startup     config.StartupInputs
	wire        *nha029C5Wire
	store       *storage.Store
	owner       *configactivation.Owner
	sequence    *sequencer.Sequencer
	pilot       *nha029C5Pilot
	driver      *nha029C5Driver
	composition *AutomationRunsComposition
	tasks       sync.WaitGroup
	admitted    configactivation.AdmittedGeneration
}

func nha029C5LocalOnly(c config.Config) bool {
	_, mqtt := c.MQTTSettings()
	_, cloud := c.NeviwebSettings()
	_, hilo := c.HiloSettings()
	return !mqtt && !cloud && !hilo && len(c.ExternalWorkers()) == 0 && len(c.EnergyBindings()) == 0 && !c.DerivedPowerProfile().Enabled()
}
func (c *nha029C5Child) factory(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
	// Run already admitted SQL. Read the same actual authority rather than
	// choosing operational A/B from the helper's process mode or workspace.
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, c.startup)
	if err != nil {
		return RuntimePlan{}, err
	}
	operational, err := admitted.OperationalConfig()
	if err != nil || !nha029C5LocalOnly(operational) {
		return RuntimePlan{}, errors.New("C5 source admits an external transport")
	}
	c.store, c.admitted = store, admitted
	if err := c.wire.send(nha029C5Message{kind: nha029C5Admitted, revision: uint64(admitted.Revision().Number()), manifest: admitted.ManifestSHA256(), digest: admitted.SourceSHA256()}); err != nil {
		return RuntimePlan{}, err
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return RuntimePlan{}, err
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		return RuntimePlan{}, err
	}
	instance, err := domain.ParseIntegrationInstanceID("nha029-c5-simulated")
	if err != nil {
		return RuntimePlan{}, err
	}
	key, err := integrations.NewSourceKey("nha029-c5-simulated-device", "relay")
	if err != nil {
		return RuntimePlan{}, err
	}
	bound, err := repository.ConfigureBinding(ctx, instance, key, nha042ServerAlias)
	if err != nil {
		return RuntimePlan{}, err
	}
	automations, scripts, err := nha042ServerPrograms(nha029C5ParentBody, nha029OwnerChild, false)
	if err != nil {
		return RuntimePlan{}, err
	}
	runs, err := automationruns.NewPlanWithScripts(automations, scripts)
	if err != nil {
		return RuntimePlan{}, err
	}
	commands, err := NewDurableCommandsComposition(DurableCommandsInput{Generation: nha094Generation{}, Clock: automationtriggers.RealClock{}})
	if err != nil {
		return RuntimePlan{}, err
	}
	c.composition, err = NewScriptRunsComposition(ScriptRunsInput{Plan: runs, Commands: commands, Clock: automationtriggers.RealClock{}, Zone: time.UTC, CommandDeadline: time.Minute})
	if err != nil {
		return RuntimePlan{}, err
	}
	templates, err := (reactivetemplate.Plan{}).WithScriptWaits(time.UTC)
	if err != nil {
		return RuntimePlan{}, err
	}
	reactive, err := NewReactiveTemplateComposition(templates, nha042NoPermanentOutput{}, reactivetemplate.RealClock{})
	if err != nil {
		return RuntimePlan{}, err
	}
	alias, _ := domain.ParseHAAlias(nha042ServerAlias)
	triggers, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: automations, Scripts: scripts, Resolver: nha042ServerResolver{entities: map[domain.HAAlias]domain.EntityID{alias: bound.Entity}}, Zone: "UTC"})
	if err != nil {
		return RuntimePlan{}, err
	}
	c.pilot = &nha029C5Pilot{key: key, driver: c.driver, ready: make(chan struct{})}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return c.pilot })
	if err != nil {
		return RuntimePlan{}, err
	}
	plan := RuntimePlan{Identity: repository, Pilots: []integrations.PilotSpec{spec}, DurableCommands: commands, AutomationRuns: c.composition, AutomationTriggers: &triggers, ReactiveTemplates: reactive,
		Configure: func(_ context.Context, host *integrations.Manager, _ *storage.Store) error {
			c.pilot.host = host
			return nil
		},
		ConfigureAuthorization: func(_ context.Context, composition AuthorizationComposition) error {
			actual, ok := composition.(runtimeAuthorizationComposition)
			if !ok {
				return ErrIdentityNotConfigured
			}
			c.sequence = actual.views.source
			return nil
		},
		ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
			if owner == nil {
				return configactivation.ErrInvalidStartup
			}
			c.owner = owner
			return nil
		}}
	if legacy, present := admitted.LegacyCandidate(); present {
		if err := addActiveHelpers(&plan, legacy, store); err != nil {
			return RuntimePlan{}, err
		}
	}
	return plan, nil
}
func (c *nha029C5Child) request() (configactivation.Request, error) {
	author, err := storage.NewConfigAuthor("nha029:c5-trusted-composition")
	if err != nil {
		return configactivation.Request{}, err
	}
	source, err := storage.NewConfigSource("nha029:c5-physical-workspace")
	if err != nil {
		return configactivation.Request{}, err
	}
	return configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
}

// Classify only closed error facts. Never serialize Error(), driver details,
// a connection string, SQL, or protected source text across the child pipe.
func nha029C5SQLDiagnostic(err error) string {
	var stored *storage.Error
	var kind storage.Kind
	if errors.As(err, &stored) {
		kind = stored.Kind()
	}
	var connect *pgconn.ConnectError
	var network net.Error
	var postgres *pgconn.PgError
	state := "none"
	if errors.As(err, &postgres) {
		state = postgres.Code
		if len(state) != 5 {
			state = "invalid"
		}
		for _, char := range state {
			if char < '0' || char > '9' && char < 'A' || char > 'Z' {
				state = "invalid"
				break
			}
		}
	}
	timeout := errors.As(err, &network) && network.Timeout()
	return fmt.Sprintf("type=%T storage=%d startup_conflict=%t connect=%t eof=%t unexpected_eof=%t closed=%t canceled=%t deadline=%t timeout=%t safe_retry=%t sqlstate=%s", err, kind,
		errors.Is(err, configactivation.ErrStartupConflict), errors.As(err, &connect), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), timeout, pgconn.SafeToRetry(err), state)
}

// Reopening the proxy permits new connections; it does not make recently
// closed idle resources in pgxpool usable. Establish fixture recovery through
// this Owner's real pool before its single Activate/Reconcile call. Only reads
// are issued here, and only a driver-proven not-sent Unavailable is tolerated.
// Each such failed BEGIN destroys its resource in pgxpool. The bound is the
// actual number of transports cut by this fault plus one successful read.
func nha029C5ReadRecoveredPool(ctx context.Context, store *storage.Store, cutConnections uint64) (storage.ConfigGenerationSnapshot, uint64, string, error) {
	if ctx == nil || store == nil || cutConnections == 0 || cutConnections > 32 {
		return storage.ConfigGenerationSnapshot{}, 0, "", errors.New("C5 invalid SQL recovery observation")
	}
	var refused uint64
	var firstRefusal string
	for probe := uint64(0); probe <= cutConnections; probe++ {
		active, present, err := store.ReadActiveConfig(ctx)
		if err == nil {
			if !present {
				return active, refused, firstRefusal, errors.New("C5 recovered SQL has no complete active authority")
			}
			return active, refused, firstRefusal, nil
		}
		var unavailable *storage.Error
		if ctx.Err() != nil || !errors.As(err, &unavailable) || unavailable.Kind() != storage.Unavailable || !pgconn.SafeToRetry(err) {
			return active, refused, firstRefusal, fmt.Errorf("C5 SQL recovery read refused: %s", nha029C5SQLDiagnostic(err))
		}
		refused++
		if firstRefusal == "" {
			firstRefusal = nha029C5SQLDiagnostic(err)
		}
	}
	return storage.ConfigGenerationSnapshot{}, refused, firstRefusal, errors.New("C5 SQL recovery exhausted the actual cut transports")
}

func (c *nha029C5Child) result(result configactivation.Result, err error) {
	failed := uint64(0)
	if err != nil {
		failed = 1
	}
	diagnostic := result.Attempt().String()
	if err != nil {
		diagnostic += " " + nha029C5SQLDiagnostic(err)
	}
	_ = c.wire.send(nha029C5Message{kind: nha029C5Activated, revision: uint64(result.Revision().Number()), count: uint64(result.Outcome()) | (failed << 32), value: diagnostic})
}
func (c *nha029C5Child) command(message nha029C5Message) error {
	switch message.kind {
	case nha029C5Fire:
		if c.pilot == nil {
			return errors.New("C5 no fixture pilot")
		}
		c.tasks.Add(1)
		go func() {
			defer c.tasks.Done()
			err := c.pilot.emit(c.ctx, "nha042_start")
			if err == nil {
				err = nha029C5Eventually(c.ctx, func() (bool, error) {
					return nha042ServerNode(c.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting) && nha042ServerNode(c.composition, "script.nha042_child", ir.ActionWait, automationruns.NodeWaiting), nil
				})
			}
			if err != nil {
				_ = c.wire.send(nha029C5Message{kind: nha029C5Failure, value: "live_waits"})
				return
			}
			_ = c.wire.send(nha029C5Message{kind: nha029C5Waits})
		}()
	case nha029C5ReleaseWait:
		if c.pilot == nil {
			return errors.New("C5 no wait pilot")
		}
		if err := c.pilot.emit(c.ctx, "nha042_release"); err != nil {
			return err
		}
		return c.wire.send(nha029C5Message{kind: nha029C5Ack})
	case nha029C5AllowEffect:
		if err := c.driver.allow(message.value); err != nil {
			return err
		}
	case nha029C5ReadRecoveredSQL:
		active, refused, firstRefusal, err := nha029C5ReadRecoveredPool(c.ctx, c.store, message.count)
		if err != nil {
			return err
		}
		return c.wire.send(nha029C5Message{kind: nha029C5ReadRecoveredSQL, revision: uint64(active.Revision().Number()), manifest: active.ManifestSHA256(), count: refused, value: firstRefusal})
	case nha029C5ReadStatus:
		if c.owner == nil {
			return errors.New("C5 status without execution owner")
		}
		stamp, err := c.owner.CurrentConfig(c.ctx)
		if err != nil {
			if !errors.Is(err, configactivation.ErrActivationUnavailable) {
				return err
			}
			if _, captureErr := c.sequence.Capture(); captureErr == nil {
				return errors.New("C5 external capture reopened while owner closed")
			}
			return c.wire.send(nha029C5Message{kind: nha029C5Status, count: 1})
		}
		revision, manifest, err := stamp.Components()
		if err != nil {
			return err
		}
		return c.wire.send(nha029C5Message{kind: nha029C5Status, revision: revision, manifest: manifest})
	case nha029C5Activate, nha029C5Reconcile, nha029C5ActivateProjection:
		if c.owner == nil {
			return errors.New("C5 activation without actual owner")
		}
		c.tasks.Add(1)
		go func() {
			defer c.tasks.Done()
			if message.kind == nha029C5Reconcile {
				result, err := c.owner.Reconcile(c.ctx)
				c.result(result, err)
				return
			}
			request, err := c.request()
			if err == nil && message.kind == nha029C5ActivateProjection {
				request, err = c.projectionRequest()
			}
			if err != nil {
				_ = c.wire.send(nha029C5Message{kind: nha029C5Failure, value: "request"})
				return
			}
			result, err := c.owner.Activate(c.ctx, request)
			c.result(result, err)
		}()
	default:
		return errors.New("C5 unsupported child phase")
	}
	return nil
}

func TestNHA029C5ProcessHelper(t *testing.T) {
	nonceText := os.Getenv("NHA029_C5_CHILD")
	if nonceText == "" {
		return
	}
	decoded, err := hex.DecodeString(nonceText)
	if err != nil || len(decoded) != 32 {
		t.Fatal("invalid owned child nonce")
	}
	var nonce [32]byte
	copy(nonce[:], decoded)
	if nonce == ([32]byte{}) {
		t.Fatal("zero owned child nonce")
	}
	path := os.Getenv("NHA029_C5_SOURCE")
	bootstrap := os.Getenv("NHA029_C5_BOOTSTRAP")
	mode := os.Getenv("NHA029_C5_MODE")
	port, err := strconv.Atoi(os.Getenv("NHA029_C5_BACKEND"))
	if err != nil || port < 1 || port > 65535 || !filepath.IsAbs(path) || filepath.Base(path) != "configuration.yaml" || (mode != "ir_fixture" && mode != "configured_sql") {
		t.Fatal("invalid owned child configuration")
	}
	if bootstrap != "" && (!filepath.IsAbs(bootstrap) || filepath.Dir(bootstrap) != filepath.Dir(path)) {
		t.Fatal("invalid explicit child bootstrap")
	}
	input := os.NewFile(3, "nha029-c5-control")
	output := os.NewFile(4, "nha029-c5-reports")
	if input == nil || output == nil {
		t.Fatal("owned child pipes absent")
	}
	defer input.Close()
	defer output.Close()
	wire := &nha029C5Wire{writer: output, nonce: nonce}
	choice := config.DefaultBootstrapChoice()
	if bootstrap != "" {
		choice, err = config.ExistingBootstrap(bootstrap)
		if err != nil {
			t.Fatal(err)
		}
	}
	startup, err := config.LoadStartupPaths(path, choice, nha029TestEnv)
	if err != nil {
		_ = wire.send(nha029C5Message{kind: nha029C5Failure, value: "startup_source"})
		t.Fatal("owned child source refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	child := &nha029C5Child{ctx: ctx, startup: startup, wire: wire}
	child.driver = &nha029C5Driver{wire: wire, observer: nha030ServerObserver(t, port), pending: make(map[string]chan struct{})}
	defer child.driver.releaseAll()
	done := make(chan error, 1)
	ready := make(chan struct{})
	if mode == "configured_sql" {
		// This is the default configured Run path with its existing read-only
		// consumer seam; no factory/IR chooses a replacement program at restart.
		dependencies := runDependencies{observe: func(observeCtx context.Context, sequence *sequencer.Sequencer) error {
			child.sequence = sequence
			var revision int64
			var manifest, source []byte
			err := child.driver.observer.QueryRow(observeCtx, `SELECT g.revision,g.manifest_hash,v.source_hash FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision JOIN public.config_generation_entries e ON e.generation_revision=g.revision JOIN public.config_documents d ON d.id=e.document_id JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision WHERE a.singleton AND d.kind='operational_yaml'`).Scan(&revision, &manifest, &source)
			if err != nil || len(manifest) != 32 || len(source) != 32 {
				return errors.New("C5 configured Run had no admitted SQL source")
			}
			message := nha029C5Message{kind: nha029C5Admitted, revision: uint64(revision)}
			copy(message.manifest[:], manifest)
			copy(message.digest[:], source)
			return wire.send(message)
		}}
		go func() {
			done <- runWithDependencies(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, nil, dependencies)
		}()
	} else {
		go func() {
			done <- RunWithStartupFactory(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, child.factory)
		}()
	}
	select {
	case <-ready:
	case err := <-done:
		_ = wire.send(nha029C5Message{kind: nha029C5Failure, value: "Run_before_ready"})
		t.Fatalf("owned Run refused: %v", err)
	case <-ctx.Done():
		t.Fatal("owned Run readiness deadline")
	}
	var readyMessage nha029C5Message
	if mode == "ir_fixture" {
		select {
		case <-child.pilot.ready:
		case <-ctx.Done():
			t.Fatal("owned fake pilot did not start")
		}
		readyMessage = nha029C5Message{kind: nha029C5Ready, revision: uint64(child.admitted.Revision().Number()), manifest: child.admitted.ManifestSHA256(), digest: child.admitted.SourceSHA256()}
	} else {
		var revision int64
		var manifest, source []byte
		if err := child.driver.observer.QueryRow(ctx, `SELECT g.revision,g.manifest_hash,v.source_hash FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision JOIN public.config_generation_entries e ON e.generation_revision=g.revision JOIN public.config_documents d ON d.id=e.document_id JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision WHERE a.singleton AND d.kind='operational_yaml'`).Scan(&revision, &manifest, &source); err != nil {
			t.Fatal(err)
		}
		readyMessage = nha029C5Message{kind: nha029C5Ready, revision: uint64(revision)}
		copy(readyMessage.manifest[:], manifest)
		copy(readyMessage.digest[:], source)
	}
	if err := wire.send(readyMessage); err != nil {
		t.Fatal("owned ready report failed")
	}
	var next uint64
	for {
		message, err := nha029C5Read(input, nonce, &next)
		if err != nil {
			cancel()
			t.Fatal("owned control channel ended")
		}
		if message.kind == nha029C5Stop {
			break
		}
		if err := child.command(message); err != nil {
			_ = wire.send(nha029C5Message{kind: nha029C5Failure, value: "control_phase"})
			cancel()
			t.Fatal("owned child control refused")
		}
	}
	child.driver.releaseAll()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("owned Run close failed: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("owned Run did not join")
	}
	child.tasks.Wait()
	if child.owner != nil {
		select {
		case <-child.owner.Done():
		default:
			t.Fatal("owned Run ended before Owner.Done")
		}
		nha034WaitRunStoreClosed(t, child.store)
	}
	if err := wire.send(nha029C5Message{kind: nha029C5Stopped}); err != nil {
		t.Fatal("owned final report failed")
	}
}

func TestNHA029C5CrashBeforeCommit(t *testing.T) {
	f := nha029C5NewFixture(t)
	child := f.start(t, false)
	child.ready(t, nil)
	a := f.active(t)
	child.fireWaits(t)
	if len(f.effects) != 1 {
		t.Fatal("C5 precondition: real terminal simulated effect absent")
	}
	counts := f.counts(t)
	f.edit(t, "B before COMMIT")
	ticket := f.proxy.arm(t, nha029C5CutBeforeCommit, nha029C5SQLNone)
	defer ticket.finish(false)
	child.send(t, nha029C5Message{kind: nha029C5Activate})
	barrier := nha029C5Barrier(t, f, ticket)
	if barrier.committed || barrier.attempt == ([16]byte{}) {
		t.Fatal("before-COMMIT barrier lacks real attempt Bind")
	}
	if active := f.active(t); active.Revision() != a.Revision() || active.ManifestSHA256() != a.ManifestSHA256() {
		t.Fatal("uncommitted B escaped SQL transaction")
	}
	attempt, err := storage.NewActivationAttemptID(barrier.attempt)
	if err != nil {
		t.Fatal(err)
	}
	type attemptResult struct {
		found bool
		err   error
	}
	read := make(chan attemptResult, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, found, err := f.store.ReadActivationAttempt(f.ctx, attempt)
		read <- attemptResult{found, err}
	}()
	t.Cleanup(func() {
		select {
		case <-readerDone:
		case <-time.After(10 * time.Second):
			t.Error("C5 fresh attempt reader not joined")
		}
	})
	if err := nha029C5Eventually(f.ctx, func() (bool, error) {
		var blocked bool
		err := f.observer.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=481316 AND objid=151 AND NOT granted AND pid<>$1)`, barrier.pid).Scan(&blocked)
		return blocked, err
	}); err != nil {
		t.Fatalf("absence reader did not join actual SQL gate151: %v", err)
	}
	child.status(t, false)
	child.kill(t)
	ticket.finish(false)
	f.waitBackendGone(t, barrier.pid)
	select {
	case result := <-read:
		if result.err != nil || result.found {
			t.Fatalf("killed uncommitted attempt not absent: found=%t err=%v", result.found, result.err)
		}
	case <-f.ctx.Done():
		t.Fatal("fresh attempt reader not joined")
	}
	if f.counts(t) != counts {
		t.Fatal("before-COMMIT crash left partial versions/audit/command")
	}
	if err := os.WriteFile(f.path, []byte("invalid: yaml: source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := f.start(t, true)
	restarted.ready(t, &a)
	restarted.stop(t)
	if len(f.effects) != 1 {
		t.Fatal("before-COMMIT restart replayed an action")
	}
}
func TestNHA029C5CrashAfterCommitBeforePublication(t *testing.T) {
	f := nha029C5NewFixture(t)
	child := f.start(t, false)
	child.ready(t, nil)
	a := f.active(t)
	child.fireWaits(t)
	before := f.counts(t)
	f.edit(t, "B durable before publication")
	ticket := f.proxy.arm(t, nha029C5CutAfterCommit, nha029C5SQLNone)
	defer ticket.finish(false)
	child.send(t, nha029C5Message{kind: nha029C5Activate})
	barrier := nha029C5Barrier(t, f, ticket)
	b := f.active(t)
	if !barrier.committed || b.Revision().Number() <= a.Revision().Number() {
		t.Fatal("held ACK was not a real durable B COMMIT")
	}
	attempt, err := storage.NewActivationAttemptID(barrier.attempt)
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := f.store.ReadActivationAttempt(f.ctx, attempt)
	if err != nil || !found || receipt.Target() != b.Revision() {
		t.Fatalf("B audit not atomic with durable pointer: %v", err)
	}
	child.status(t, false)
	child.kill(t)
	ticket.finish(false)
	f.waitBackendGone(t, barrier.pid)
	committed := f.counts(t)
	if committed[0] != before[0]+1 || committed[1] != before[1]+1 || committed[2] != before[2] {
		t.Fatal("post-COMMIT crash mutated an unexpected cohort")
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	restarted := f.start(t, true)
	restarted.ready(t, &b)
	restarted.stop(t)
	if f.counts(t) != committed || len(f.effects) != 1 {
		t.Fatal("post-COMMIT restart rewrote config or replayed an effect")
	}
}
func TestNHA029C5LostCommitACKQuarantinesUntilSQL(t *testing.T) {
	f := nha029C5NewFixture(t)
	child := f.start(t, false)
	child.ready(t, nil)
	child.fireWaits(t)
	f.edit(t, "B ACK unknown")
	ticket := f.proxy.arm(t, nha029C5CutAfterCommit, nha029C5SQLNone)
	defer ticket.finish(false)
	child.send(t, nha029C5Message{kind: nha029C5Activate})
	barrier := nha029C5Barrier(t, f, ticket)
	if !barrier.committed {
		t.Fatal("unknown ACK precondition: real B COMMIT absent")
	}
	b := f.active(t)
	counts := f.counts(t)
	runtimePID := f.runtimeBackend(t)
	cutConnections := f.proxy.setUnavailable(t, true, runtimePID)
	if cutConnections == 0 {
		t.Fatal("C5 fault did not cut any real configuration-pool transport")
	}
	ticket.finish(false)
	result := child.await(t, nha029C5Activated)
	if configactivation.Outcome(uint32(result.count)) != configactivation.CommitUnknown || result.count>>32 != 1 {
		t.Fatal("lost ACK was reported as success or proved absence")
	}
	child.status(t, false)
	child.send(t, nha029C5Message{kind: nha029C5Reconcile})
	refused := child.await(t, nha029C5Activated)
	if refused.count>>32 != 1 {
		t.Fatal("unavailable SQL reopened uncertain activation")
	}
	child.status(t, false)
	if f.runtimeBackend(t) != runtimePID {
		t.Fatal("C5 configuration fault retired an unrelated durable runtime session")
	}
	f.proxy.setUnavailable(t, false, 0)
	acceptedBeforeRecovery := f.proxy.acceptedConnections.Load()
	t.Logf("C5 transport reopened before pool recovery: %s", f.proxy.diagnostic())
	child.send(t, nha029C5Message{kind: nha029C5ReadRecoveredSQL, count: cutConnections})
	recovered := child.await(t, nha029C5ReadRecoveredSQL)
	t.Logf("C5 Owner pool readiness: refused=%d first={%s} proxy={%s}", recovered.count, recovered.value, f.proxy.diagnostic())
	if recovered.revision != uint64(b.Revision().Number()) || recovered.manifest != b.ManifestSHA256() || f.proxy.acceptedConnections.Load() <= acceptedBeforeRecovery {
		t.Fatal("C5 recovery did not read complete B through a new real Owner-pool transport")
	}
	// SQL readiness alone must not publish memory, reopen capture, write an
	// audit, or replay an effect. Only the following single Reconcile may do so.
	child.status(t, false)
	if f.counts(t) != counts || len(f.effects) != 1 || f.runtimeBackend(t) != runtimePID {
		t.Fatal("C5 read-only pool recovery changed SQL/effects or the singleton024 session")
	}
	child.send(t, nha029C5Message{kind: nha029C5Reconcile})
	published := child.await(t, nha029C5Activated)
	if published.count>>32 != 0 || configactivation.Outcome(uint32(published.count)) != configactivation.Published || published.revision != uint64(b.Revision().Number()) {
		t.Fatalf("fresh SQL reconciliation did not actually publish B: outcome=%d failed=%d revision=%d wanted=%d diagnostic=%q proxy={%s}", uint32(published.count), published.count>>32, published.revision, b.Revision().Number(), published.value, f.proxy.diagnostic())
	}
	current := child.status(t, true)
	if current.revision != uint64(b.Revision().Number()) || current.manifest != b.ManifestSHA256() {
		t.Fatal("resumed owner differs from SQL B")
	}
	if f.counts(t) != counts || len(f.effects) != 1 {
		t.Fatal("reconciliation performed another write or effect")
	}
	if f.runtimeBackend(t) != runtimePID {
		t.Fatal("C5 reconciliation replaced the durable runtime session")
	}
	child.stop(t)
	var runtimeLocks int
	if err := f.observer.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=481316 AND objid=24 AND objsubid=2 AND granted`).Scan(&runtimeLocks); err != nil || runtimeLocks != 0 {
		t.Fatalf("C5 Stop did not release the actual durable runtime lock: count=%d err=%v", runtimeLocks, err)
	}
	f.waitBackendGone(t, runtimePID)
}
