package configactivation

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// nha029CommitACKProxy only accepts local PostgreSQL test traffic. It forwards
// complete protocol frames and withholds the successful COMMIT response for
// one transaction which updated the bootstrap pointer. The backend's
// CommandComplete and ReadyForQuery are read before the response is dropped;
// an independent PostgreSQL read then proves the commit was durable.
type nha029CommitACKProxy struct {
	listener net.Listener
	backend  string
	armed    atomic.Bool
	claimed  atomic.Bool
	held     chan struct{}
	release  chan struct{}
	stop     chan struct{}
	accepted chan struct{}
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	once     sync.Once
	relOnce  sync.Once
}

func nha029StartCommitACKProxy(t *testing.T, backendPort int) *nha029CommitACKProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha029CommitACKProxy{
		listener: listener, backend: net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)),
		held: make(chan struct{}), release: make(chan struct{}), stop: make(chan struct{}),
		accepted: make(chan struct{}), conns: make(map[net.Conn]struct{}),
	}
	go p.accept()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *nha029CommitACKProxy) port(t *testing.T) int {
	t.Helper()
	_, textPort, err := net.SplitHostPort(p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(textPort)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (p *nha029CommitACKProxy) accept() {
	defer close(p.accepted)
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		backend, err := net.DialTimeout("tcp", p.backend, 5*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns[client], p.conns[backend] = struct{}{}, struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go p.forward(client, backend)
	}
}

func (p *nha029CommitACKProxy) forward(client, backend net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = client.Close()
		_ = backend.Close()
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, backend)
		p.mu.Unlock()
	}()
	var bootstrapUpdate atomic.Bool
	var interceptCommit atomic.Bool
	frontDone := make(chan struct{})
	go func() {
		defer close(frontDone)
		// sslmode=disable sends one untagged StartupMessage. All following
		// frontend messages have a type byte and a big-endian length.
		frame, err := nha029ReadStartupFrame(client)
		if err != nil || nha029WriteAll(backend, frame) != nil {
			return
		}
		for {
			typ, payload, frame, err := nha029ReadTaggedFrame(client)
			if err != nil {
				return
			}
			if (typ == 'P' && nha029ParseContainsBootstrapUpdate(payload)) ||
				(typ == 'Q' && nha029ContainsBootstrapUpdate(payload)) {
				bootstrapUpdate.Store(true)
			}
			if typ == 'Q' && bytes.EqualFold(bytes.TrimSpace(bytes.TrimSuffix(payload, []byte{0})), []byte("commit")) &&
				bootstrapUpdate.Load() && p.armed.Load() && p.claimed.CompareAndSwap(false, true) {
				interceptCommit.Store(true)
			}
			if nha029WriteAll(backend, frame) != nil {
				return
			}
		}
	}()
	defer func() {
		_ = client.Close()
		_ = backend.Close()
		<-frontDone
	}()
	committed := false
	for {
		typ, payload, frame, err := nha029ReadTaggedFrame(backend)
		if err != nil {
			return
		}
		if typ == 'C' && interceptCommit.Load() && bytes.Equal(payload, []byte("COMMIT\x00")) {
			committed = true
			continue // retain CommandComplete until ReadyForQuery confirms idle
		}
		if typ == 'Z' && committed {
			if len(payload) != 1 || payload[0] != 'I' {
				return
			}
			close(p.held)
			select {
			case <-p.release:
			case <-p.stop:
			}
			return // close client without sending COMMIT acknowledgement
		}
		if nha029WriteAll(client, frame) != nil {
			return
		}
	}
}

func nha029ParseContainsBootstrapUpdate(payload []byte) bool {
	endName := bytes.IndexByte(payload, 0)
	if endName < 0 {
		return false
	}
	return nha029ContainsBootstrapUpdate(payload[endName+1:])
}

func nha029ContainsBootstrapUpdate(payload []byte) bool {
	return bytes.Contains(bytes.ToLower(payload), []byte("update public.config_bootstrap_active"))
}

func nha029ReadStartupFrame(r io.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size < 8 || size > 16<<20 {
		return nil, fmt.Errorf("invalid PostgreSQL startup frame length %d", size)
	}
	frame := make([]byte, size)
	copy(frame, length[:])
	_, err := io.ReadFull(r, frame[4:])
	return frame, err
}

func nha029ReadTaggedFrame(r io.Reader) (byte, []byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size < 4 || size > 16<<20 {
		return 0, nil, nil, fmt.Errorf("invalid PostgreSQL tagged frame length %d", size)
	}
	frame := make([]byte, int(size)+1)
	copy(frame, header[:])
	_, err := io.ReadFull(r, frame[5:])
	return header[0], frame[5:], frame, err
}

func nha029WriteAll(w io.Writer, frame []byte) error {
	for len(frame) != 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func (p *nha029CommitACKProxy) allowDrop() { p.relOnce.Do(func() { close(p.release) }) }

func (p *nha029CommitACKProxy) close(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		close(p.stop)
		p.allowDrop()
		_ = p.listener.Close()
		<-p.accepted
		p.mu.Lock()
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		joined := make(chan struct{})
		go func() { p.wg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("PostgreSQL ACK proxy goroutines did not join")
		}
	})
}

func nha029ProxyStore(t *testing.T, ctx context.Context, port int) *storage.Store {
	t.Helper()
	settings, err := storage.ParseSettings(storage.ConnectionInput{
		Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test",
		PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable",
	}, nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestNHA029BootstrapOnlyLostCommitACKReconcilesBeforeStartup(t *testing.T) {
	backend := testpg.Start(t)
	proxy := nha029StartCommitACKProxy(t, backend.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := nha029ProxyStore(t, ctx, proxy.port(t))
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	raw := append(nha029Launch(proxy.port(t), filepath.Join(root, "private-data")), []byte("# exact operational source\nexternal_workers: []\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	firstInput, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	first, disposition, err := ReconcileStartup(ctx, store, firstInput)
	if err != nil || disposition != StartupInitialized || !first.Valid() {
		t.Fatalf("initial active configuration: disposition=%v err=%v", disposition, err)
	}
	before, found, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !found {
		t.Fatalf("initial bootstrap pointer: found=%t err=%v", found, err)
	}
	// A fresh Store forces a new physical writer connection and prevents a
	// prepared statement cached during the first activation from hiding the
	// pointer UPDATE from the protocol-level fixture.
	store.Close()
	store = nha029ProxyStore(t, ctx, proxy.port(t))
	observer := nha029ProxyStore(t, ctx, backend.Port)
	changed := append(before.Bytes(), []byte("recorder:\n  budget_mib: 16\n")...)
	if err := os.WriteFile(path+".bootstrap.yaml", changed, 0600); err != nil {
		t.Fatal(err)
	}
	secondInput, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029Lookup)
	if err != nil {
		t.Fatal(err)
	}
	proxy.armed.Store(true)
	type result struct {
		generation  AdmittedGeneration
		disposition StartupDisposition
		err         error
	}
	finished := make(chan result, 1)
	joined := false
	go func() {
		generation, state, runErr := ReconcileStartup(ctx, store, secondInput)
		finished <- result{generation, state, runErr}
	}()
	t.Cleanup(func() {
		cancel()
		proxy.allowDrop()
		if !joined {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("bootstrap ACK run did not join")
			}
		}
	})
	select {
	case <-proxy.held:
	case got := <-finished:
		joined = true
		t.Fatalf("bootstrap writer exited before COMMIT ACK trap: %v", got.err)
	case <-time.After(15 * time.Second):
		t.Fatal("bootstrap COMMIT response was not trapped")
	}
	// PostgreSQL completed COMMIT and emitted its successful response. This
	// direct observer is outside the proxy and sees the durable target before
	// the proxy severs the acknowledgement to the writer.
	committed, found, err := observer.ReadActiveBootstrap(ctx)
	if err != nil || !found || committed.Ref() == before.Ref() || committed.SHA256() != secondInputBootstrapHash(t, secondInput) {
		t.Fatalf("trapped COMMIT was not durably applied: found=%t err=%v", found, err)
	}
	proxy.allowDrop()
	got := <-finished
	joined = true
	if got.err != nil || got.disposition != StartupBootstrapUpdated || !got.generation.Valid() ||
		got.generation.Revision() != first.Revision() || got.generation.ManifestSHA256() != first.ManifestSHA256() {
		t.Fatalf("durable bootstrap COMMIT with lost ACK was not reconciled: disposition=%v err=%v", got.disposition, got.err)
	}
	active, found, err := observer.ReadActiveConfig(ctx)
	if err != nil || !found || active.Revision() != first.Revision() || active.ManifestSHA256() != first.ManifestSHA256() {
		t.Fatalf("bootstrap-only changed operational generation: found=%t err=%v", found, err)
	}
	var versions int
	if err := observer.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.config_versions WHERE document_id='bootstrap.launch'`).Scan(&versions)
	}); err != nil || versions != 2 {
		t.Fatalf("bootstrap was retried after lost ACK: versions=%d err=%v", versions, err)
	}
}

func secondInputBootstrapHash(t *testing.T, source config.StartupInputs) [32]byte {
	t.Helper()
	snapshot, err := source.BootstrapSource()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.SHA256()
}
