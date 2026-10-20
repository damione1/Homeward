package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// nha186CommitACKProxy only accepts local PostgreSQL test traffic. It forwards
// complete protocol frames and withholds the successful COMMIT response for
// one transaction which updated the bootstrap pointer. The backend's
// CommandComplete and ReadyForQuery are read before the response is dropped;
// an independent PostgreSQL read then proves the commit was durable.
type nha186CommitACKProxy struct {
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

func nha186StartCommitACKProxy(t *testing.T, backendPort int) *nha186CommitACKProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha186CommitACKProxy{
		listener: listener, backend: net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)),
		held: make(chan struct{}), release: make(chan struct{}), stop: make(chan struct{}),
		accepted: make(chan struct{}), conns: make(map[net.Conn]struct{}),
	}
	go p.accept()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *nha186CommitACKProxy) port(t *testing.T) int {
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

func (p *nha186CommitACKProxy) accept() {
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

func (p *nha186CommitACKProxy) forward(client, backend net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = client.Close()
		_ = backend.Close()
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, backend)
		p.mu.Unlock()
	}()
	var historyUpdate atomic.Bool
	var interceptCommit atomic.Bool
	frontDone := make(chan struct{})
	go func() {
		defer close(frontDone)
		// sslmode=disable sends one untagged StartupMessage. All following
		// frontend messages have a type byte and a big-endian length.
		frame, err := nha186ReadStartupFrame(client)
		if err != nil || nha186WriteAll(backend, frame) != nil {
			return
		}
		for {
			typ, payload, frame, err := nha186ReadTaggedFrame(client)
			if err != nil {
				return
			}
			if (typ == 'P' && nha186ParseContainsHistoryUpdate(payload)) ||
				(typ == 'Q' && nha186ContainsHistoryUpdate(payload)) {
				historyUpdate.Store(true)
			}
			if typ == 'Q' && bytes.EqualFold(bytes.TrimSpace(bytes.TrimSuffix(payload, []byte{0})), []byte("commit")) &&
				historyUpdate.Load() && p.armed.Load() && p.claimed.CompareAndSwap(false, true) {
				interceptCommit.Store(true)
			}
			if nha186WriteAll(backend, frame) != nil {
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
		typ, payload, frame, err := nha186ReadTaggedFrame(backend)
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
		if nha186WriteAll(client, frame) != nil {
			return
		}
	}
}

func nha186ParseContainsHistoryUpdate(payload []byte) bool {
	endName := bytes.IndexByte(payload, 0)
	if endName < 0 {
		return false
	}
	return nha186ContainsHistoryUpdate(payload[endName+1:])
}

func nha186ContainsHistoryUpdate(payload []byte) bool {
	return bytes.Contains(bytes.ToLower(payload), []byte("update public.execution_history_head"))
}

func nha186ReadStartupFrame(r io.Reader) ([]byte, error) {
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

func nha186ReadTaggedFrame(r io.Reader) (byte, []byte, []byte, error) {
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

func nha186WriteAll(w io.Writer, frame []byte) error {
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

func (p *nha186CommitACKProxy) allowDrop() { p.relOnce.Do(func() { close(p.release) }) }

func (p *nha186CommitACKProxy) close(t *testing.T) {
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

func TestNHA186LostCommitACKResolvedWithoutDuplicate(t *testing.T) {
	store, settings := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	proxy := nha186StartCommitACKProxy(t, int(settings.port))
	settings.port = uint16(proxy.port(t))
	writer, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	batch := nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, time.Now().UTC(), 2))
	proxy.armed.Store(true)
	finished := make(chan error, 1)
	go func() { _, err := writer.CommitExecutionHistoryBatch(ctx, batch); finished <- err }()
	defer proxy.allowDrop()
	select {
	case <-proxy.held:
	case err := <-finished:
		t.Fatalf("writer returned before COMMIT ACK trap: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("COMMIT was not trapped")
	}
	// This independent, unproxied connection sees durable data while the
	// writer has received neither CommandComplete nor ReadyForQuery.
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.execution_history_records WHERE epoch=$1`, epoch.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("independent durable witness: count=%d err=%v", count, err)
	}
	proxy.allowDrop()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrHistoryCommitUnknown) {
			t.Fatalf("lost ACK classification: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not join after ACK drop")
	}
	if got, err := store.ResolveExecutionHistoryBatch(ctx, batch); err != nil || got != HistoryResolutionCommitted {
		t.Fatalf("fresh connection resolution: %v %v", got, err)
	}
	if got, err := store.CommitExecutionHistoryBatch(ctx, batch); err != nil || got != HistoryAlreadyCommitted {
		t.Fatalf("retry: %v %v", got, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.execution_history_records WHERE epoch=$1`, epoch.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate after ACK loss: %d %v", count, err)
	}
}

func TestNHA186ResolverWaitsForOriginalTransaction(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	batch := nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, time.Now().UTC(), 2))
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT singleton FROM public.execution_history_head WHERE singleton=true FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	// Reproduce a live original writer that has acquired the actual barrier
	// and inserted the receipt, but has not yet committed or rolled back.
	if _, err := tx.Exec(ctx, `INSERT INTO public.execution_history_batches(epoch,batch_number,digest,retention_time) VALUES($1,1,$2,now())`, epoch.String(), batch.digest[:]); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	got, err := store.ResolveExecutionHistoryBatch(short, batch)
	cancel()
	if got == HistoryResolutionAbsent || !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("uncommitted absence passed live writer: resolution=%v err=%v", got, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ResolveExecutionHistoryBatch(ctx, batch); err != nil || got != HistoryResolutionAbsent {
		t.Fatalf("absence after rollback: %v %v", got, err)
	}
	if got, err := store.CommitExecutionHistoryBatch(ctx, batch); err != nil || got != HistoryCommitted {
		t.Fatalf("retry after proven rollback: %v %v", got, err)
	}
}

func TestNHA186KeysetSnapshotAndPurgeKeepOtherClasses(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	commandID, _ := domain.ParseCommandID("nha186-retention-ledger-sentinel")
	actorID, _ := domain.ParseActorID("nha186-retention-actor")
	fingerprint := []byte("unchanged ledger evidence")
	if _, created, outcome, err := store.AcceptCommandIntent(ctx, commandID, actorID, "nha186-ledger-epoch", fingerprint); err != nil || !created || outcome != TransactionCommitted {
		t.Fatalf("ledger sentinel: %v %v %v", created, outcome, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES('nha186-device'); INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('Host:nha186','nha186-device','switch.nha186')`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	var records []HistoryRecord
	for i := uint64(1); i <= 80; i++ {
		records = append(records, nha186Record(t, epoch, i, at.Add(time.Duration(i)*time.Microsecond), 2))
	}
	batch := nha186Batch(t, epoch, 1, records...)
	if _, err := store.CommitExecutionHistoryBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	first, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16})
	if err != nil || len(first.Records) != 16 || first.Next == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	// A later commit must stay outside this cursor's captured high bound.
	if _, err := store.CommitExecutionHistoryBatch(ctx, nha186Batch(t, epoch, 2, nha186Record(t, epoch, 81, at.Add(81*time.Microsecond), 2))); err != nil {
		t.Fatal(err)
	}
	seen := len(first.Records)
	cursor := first.Next
	for cursor != nil {
		page, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if page.CommitHigh != first.CommitHigh {
			t.Fatal("cursor changed commit bound")
		}
		if page.Checkpoint != first.Checkpoint {
			t.Fatalf("cursor spliced later checkpoint: got=%d captured=%d", page.Checkpoint, first.Checkpoint)
		}
		for _, r := range page.Records {
			if r.Record.ID().Ordinal() > 80 {
				t.Fatal("later commit entered earlier snapshot")
			}
		}
		seen += len(page.Records)
		cursor = page.Next
	}
	if seen != 80 {
		t.Fatalf("keyset lost or repeated rows: %d", seen)
	}
	cutoff := at.Add(40 * time.Microsecond)
	if _, err := store.PurgeExecutionHistory(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16, Cursor: first.Next}); !errors.Is(err, ErrHistoryRetentionAdvanced) {
		t.Fatalf("cursor silently survived purge: %v", err)
	}
	page, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 1})
	if err != nil || len(page.Records) != 1 || page.Records[0].Record.ID().Ordinal() != 40 {
		t.Fatalf("exact retention equality: %+v %v", page, err)
	}
	intent, found, err := store.CommandIntent(ctx, commandID)
	if err != nil || !found || !bytes.Equal(intent.Fingerprint(), fingerprint) || intent.State() != IntentAccepted {
		t.Fatalf("purge changed ledger: %v %v", found, err)
	}
	var alias string
	if err := store.pool.QueryRow(ctx, `SELECT manifest_alias FROM public.registry_entities WHERE id='Host:nha186'`).Scan(&alias); err != nil || alias != "switch.nha186" {
		t.Fatalf("purge changed registry: %q %v", alias, err)
	}
	// Corrupt an enum in this disposable DB to exercise SQL admission rather
	// than merely a public Go constructor; an empty page cannot hide it.
	if _, err := store.pool.Exec(ctx, `ALTER TABLE public.execution_history_records DROP CONSTRAINT execution_history_records_decision_check; UPDATE public.execution_history_records SET decision=255 WHERE ordinal=40`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 1}); !errors.Is(err, ErrHistoryIntegrity) {
		t.Fatalf("unknown SQL issue not rejected: %v", err)
	}
}

func TestNHA186CrashChild(t *testing.T) {
	portText := os.Getenv("HOMEWARD_NHA186_CRASH_TEST_PORT")
	if portText == "" {
		return
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := ParseSettings(ConnectionInput{Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(string) (string, bool) { return testpg.Password(), true })
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // SIGKILL must prevent this clean close.
	epoch := nha186Epoch(t, "1123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(context.Background(), epoch); err != nil {
		t.Fatal(err)
	}
	batch := nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, time.Now().UTC(), 2))
	if _, err := store.CommitExecutionHistoryBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	fmt.Println("NHA186_CHECKPOINT_COMMITTED")
	for {
		time.Sleep(time.Hour)
	}
}

func TestNHA186ProcessCrashKeepsCheckpointAndUncertainEpochEdge(t *testing.T) {
	store, settings := nha051Store(t)
	childCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestNHA186CrashChild$")
	child.Env = append(os.Environ(), "HOMEWARD_NHA186_CRASH_TEST_PORT="+strconv.Itoa(int(settings.port)))
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	child.Stderr = &diagnostics
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	defer func() {
		if !joined {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	scanner := bufio.NewScanner(output)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "NHA186_CHECKPOINT_COMMITTED" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("child never committed: %v %s", scanner.Err(), diagnostics.String())
	}
	old := nha186Epoch(t, "1123456789abcdef0123456789abcdef")
	before, err := store.ReadExecutionHistoryPage(context.Background(), HistoryPageQuery{Epoch: old, Limit: 16})
	if err != nil || len(before.Records) != 1 || before.Checkpoint != 1 {
		t.Fatalf("durable witness before SIGKILL: %+v %v", before, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	joined = true
	if err == nil {
		t.Fatal("child exited cleanly instead of crashing")
	}
	next := nha186Epoch(t, "2123456789abcdef0123456789abcdef")
	opening, err := store.OpenExecutionHistoryEpoch(context.Background(), next)
	if err != nil || !opening.InterruptedPredecessor || opening.PreviousEpoch != old || opening.PreviousCheckpoint != 1 {
		t.Fatalf("crash frontier: %+v %v", opening, err)
	}
	after, err := store.ReadExecutionHistoryPage(context.Background(), HistoryPageQuery{Epoch: old, Limit: 16})
	if err != nil || len(after.Records) != 1 || after.Records[0].Record.Digest() != before.Records[0].Record.Digest() {
		t.Fatalf("crash rewrote prior history: %+v %v", after, err)
	}
	if after.EpochState != HistoryEpochInterrupted {
		t.Fatalf("old epoch hides its interruption: %v", after.EpochState)
	}
	page, err := store.ReadExecutionHistoryPage(context.Background(), HistoryPageQuery{Epoch: next, Limit: 16})
	if err != nil || len(page.Records) != 0 || page.Checkpoint != 0 || !page.InterruptedPredecessor || page.PreviousEpoch != old {
		t.Fatalf("restart invented events or lost uncertainty: %+v %v", page, err)
	}
}

type nha186ReadBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *nha186ReadBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT retention_cutoff,commit_sequence") {
		b.entered <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*nha186ReadBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestNHA186TwoReadSlotsIncludeAcquisitionInDeadline(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	barrier := &nha186ReadBarrier{entered: make(chan struct{}, 4), release: make(chan struct{})}
	config := store.pool.Config()
	config.ConnConfig.Tracer = barrier
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reader := &Store{pool: pool, historyReadSlots: make(chan struct{}, 2)}
	var once sync.Once
	defer once.Do(func() { close(barrier.release) })
	finished := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := reader.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 1})
			finished <- err
		}()
	}
	for range 2 {
		select {
		case <-barrier.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("reader did not reach held query")
		}
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = reader.ReadExecutionHistoryPage(short, HistoryPageQuery{Epoch: epoch, Limit: 1})
	cancel()
	if !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("third read escaped bounded acquisition: %v", err)
	}
	select {
	case <-barrier.entered:
		t.Fatal("third query bypassed the two read reservations")
	default:
	}
	once.Do(func() { close(barrier.release) })
	for range 2 {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("read did not join")
		}
	}
}

func TestNHA186PurgeMixedLossClassesAndSevenDayBoundary(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 7 * 24 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			store, _ := nha051Store(t)
			ctx := context.Background()
			epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
			if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
				t.Fatal(err)
			}
			cutoff := time.Now().UTC().Truncate(time.Microsecond).Add(-age)
			older, equal, newer := cutoff.Add(-time.Microsecond), cutoff, cutoff.Add(time.Microsecond)
			// These old audit and business rows belong to different retention
			// classes and must survive even when older than the trace fence.
			if _, err := store.pool.Exec(ctx, `INSERT INTO public.registry_devices(id) VALUES('nha186-helper-device');
			 INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('Host:nha186-helper','nha186-helper-device','input_boolean.nha186');
			 INSERT INTO public.helper_values(entity_id,kind,boolean_value,revision,updated_at) VALUES('Host:nha186-helper',1,true,7,'2001-01-01T00:00:00Z');
			 INSERT INTO public.auth_audit(occurred_at,action,outcome,route_path) VALUES('2001-01-01T00:00:00Z',1,1,'nha186-sentinel')`); err != nil {
				t.Fatal(err)
			}
			first, err := NewHistoryBatch(epoch, 1, []HistoryRecord{nha186Record(t, epoch, 1, older, 2)}, []HistoryLoss{{Reason: HistoryLossQueueFull, KnownCount: 7, From: older.Add(-time.Microsecond), Through: older}})
			if err != nil {
				t.Fatal(err)
			}
			second, err := NewHistoryBatch(epoch, 2, []HistoryRecord{nha186Record(t, epoch, 2, equal, 2), nha186Record(t, epoch, 3, newer, 2)}, []HistoryLoss{{Reason: HistoryLossContention, KnownCount: 11, From: older, Through: newer}})
			if err != nil {
				t.Fatal(err)
			}
			for _, batch := range []HistoryBatch{first, second} {
				if _, err := store.CommitExecutionHistoryBatch(ctx, batch); err != nil {
					t.Fatal(err)
				}
			}
			result, err := store.PurgeExecutionHistory(ctx, cutoff)
			if err != nil || result.DeletedRecords != 1 || result.DeletedLosses != 1 || result.DeletedBatches != 1 || result.MoreExpired {
				t.Fatalf("mixed purge: %+v %v", result, err)
			}
			again, err := store.PurgeExecutionHistory(ctx, cutoff)
			if err != nil || again.DeletedRecords+again.DeletedLosses+again.DeletedBatches != 0 {
				t.Fatalf("repeat purge: %+v %v", again, err)
			}
			var retained []int64
			if err := store.pool.QueryRow(ctx, `SELECT array_agg(ordinal ORDER BY ordinal) FROM public.execution_history_records WHERE epoch=$1`, epoch.String()).Scan(&retained); err != nil || len(retained) != 2 || retained[0] != 2 || retained[1] != 3 {
				t.Fatalf("cutoff -1/equal/+1us: %v %v", retained, err)
			}
			var loss int64
			if err := store.pool.QueryRow(ctx, `SELECT known_count FROM public.execution_history_loss WHERE epoch=$1`, epoch.String()).Scan(&loss); err != nil || loss != 11 {
				t.Fatalf("crossing loss was prorated or removed: %d %v", loss, err)
			}
			if age == time.Hour {
				page, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16})
				if err != nil || len(page.Losses) != 1 || page.Losses[0].KnownCount != 11 || !page.Losses[0].UncertainEnvelope {
					t.Fatalf("crossing loss quality: %+v %v", page, err)
				}
			}
			var value bool
			var revision, audit int64
			if err := store.pool.QueryRow(ctx, `SELECT boolean_value,revision FROM public.helper_values WHERE entity_id='Host:nha186-helper'`).Scan(&value, &revision); err != nil || !value || revision != 7 {
				t.Fatalf("purge changed business value: %v %d %v", value, revision, err)
			}
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_audit WHERE route_path='nha186-sentinel'`).Scan(&audit); err != nil || audit != 1 {
				t.Fatalf("purge changed audit: %d %v", audit, err)
			}
		})
	}
}

func TestNHA186PurgeReturnsBoundedContinuation(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-8 * 24 * time.Hour)
	for batchNumber, start := uint64(1), uint64(1); start <= 513; batchNumber++ {
		var records []HistoryRecord
		for len(records) < 128 && start <= 513 {
			records = append(records, nha186Record(t, epoch, start, at, 2))
			start++
		}
		if _, err := store.CommitExecutionHistoryBatch(ctx, nha186Batch(t, epoch, batchNumber, records...)); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	first, err := store.PurgeExecutionHistory(ctx, cutoff)
	if err != nil || first.DeletedRecords != 512 || first.DeletedBatches != 0 || !first.MoreExpired {
		t.Fatalf("first bounded purge: %+v %v", first, err)
	}
	second, err := store.PurgeExecutionHistory(ctx, cutoff)
	if err != nil || second.DeletedRecords != 1 || second.DeletedBatches != 5 || second.MoreExpired || second.DeletedRecords+second.DeletedLosses+second.DeletedBatches > 512 {
		t.Fatalf("purge continuation: %+v %v", second, err)
	}
}

func TestNHA186SQLDiscriminantsRejectNarrowingAliases(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitExecutionHistoryBatch(ctx, nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, time.Now().UTC(), 2))); err != nil {
		t.Fatal(err)
	}
	// Deliberately relax only CHECK constraints in this disposable table.
	// Wire/SQL admission must survive a corrupt discriminant even if its
	// modulo-256 alias and the original digest otherwise look valid.
	nha186RelaxRecordChecks(t, store)
	for _, column := range []string{"kind", "provenance_kind"} {
		for _, value := range []int{257, -255} {
			if _, err := store.pool.Exec(ctx, "UPDATE public.execution_history_records SET "+pgx.Identifier{column}.Sanitize()+"=$1", value); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 1}); !errors.Is(err, ErrHistoryIntegrity) {
				t.Fatalf("SQL %s=%d narrowed into admitted enum: %v", column, value, err)
			}
		}
		if _, err := store.pool.Exec(ctx, "UPDATE public.execution_history_records SET "+pgx.Identifier{column}.Sanitize()+"=1"); err != nil {
			t.Fatal(err)
		}
	}
}

func nha186RelaxRecordChecks(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.pool.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='public.execution_history_records'::regclass AND contype='c'`)
	if err != nil {
		t.Fatal(err)
	}
	var constraints []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		constraints = append(constraints, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	for _, name := range constraints {
		if _, err := store.pool.Exec(ctx, "ALTER TABLE public.execution_history_records DROP CONSTRAINT "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNHA186SQLPresenceRejectsZeroAndMissingSceneMember(t *testing.T) {
	store, _ := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	var records []HistoryRecord
	for index, kind := range []HistoryRecordKind{HistoryDecision, HistorySceneMember, HistorySceneInvocation} {
		id, _ := NewHistoryTraceID(epoch, uint64(index+1))
		in := HistoryRecordInput{ID: id, EmissionTime: time.Now().UTC(), Kind: kind, Provenance: HistoryUnboundFixture}
		switch kind {
		case HistoryDecision:
			in.Decision = 2
		case HistorySceneMember:
			in.SceneOutcome, in.SceneMember = 1, 0
		case HistorySceneInvocation:
			in.SceneAggregate = 1
		}
		record, err := NewHistoryRecord(in)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if _, err := store.CommitExecutionHistoryBatch(ctx, nha186Batch(t, epoch, 1, records...)); err != nil {
		t.Fatal(err)
	}
	read := func() (HistoryPage, error) {
		return store.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16})
	}
	page, err := read()
	if err != nil || len(page.Records) != 3 || page.Records[1].Record.Input().Kind != HistorySceneMember || page.Records[1].Record.Input().SceneMember != 0 {
		t.Fatalf("member0 and absent optional metadata must round-trip: %+v %v", page, err)
	}
	update := func(t *testing.T, column string, ordinal int64, value any) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, "UPDATE public.execution_history_records SET "+pgx.Identifier{column}.Sanitize()+"=$1 WHERE epoch=$2 AND ordinal=$3", value, epoch.String(), ordinal); err != nil {
			t.Fatal(err)
		}
	}
	// Both corruptions are constructible with migration20's CHECKs active.
	// Neither may reuse the admitted digest for member0 or an absent member.
	t.Run("missing_scene_member_with_checks", func(t *testing.T) {
		update(t, "scene_member", 2, nil)
		defer update(t, "scene_member", 2, 0)
		if _, err := read(); !errors.Is(err, ErrHistoryIntegrity) {
			t.Fatalf("NULL became scene member0: %v", err)
		}
	})
	t.Run("unexpected_member_with_checks", func(t *testing.T) {
		update(t, "scene_member", 1, 0)
		defer update(t, "scene_member", 1, nil)
		if _, err := read(); !errors.Is(err, ErrHistoryIntegrity) {
			t.Fatalf("member0 became absence on a decision: %v", err)
		}
	})
	// Positive SQL optionals have no zero variant. Relax only the disposable
	// table's CHECKs to exercise those boundary refusals with unchanged digests.
	nha186RelaxRecordChecks(t, store)
	for _, column := range []string{
		"native_ordinal", "node_visit", "command_ordinal", "controller_generation", "run_id", "parent_run_id", "config_revision",
		"decision", "node_stage", "command_outcome", "scene_outcome", "scene_aggregate", "script_mode", "action_kind", "failure_kind", "wait_exit",
		"source_line", "source_column",
	} {
		t.Run(column, func(t *testing.T) {
			ordinal := int64(1)
			if column == "decision" {
				ordinal = 3 // the invocation's decision is absent
			}
			update(t, column, ordinal, 0)
			defer update(t, column, ordinal, nil)
			if _, err := read(); !errors.Is(err, ErrHistoryIntegrity) {
				t.Fatalf("present SQL %s=0 became absence: %v", column, err)
			}
		})
	}
	if page, err := read(); err != nil || len(page.Records) != 3 {
		t.Fatalf("restored absent metadata/member0 did not round-trip: %+v %v", page, err)
	}
}
