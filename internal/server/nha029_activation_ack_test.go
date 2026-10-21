package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nha029OwnerCommitACKProxy only accepts local PostgreSQL test traffic. It forwards
// complete protocol frames and withholds the successful COMMIT response for
// one transaction which updated the configuration pointer. The backend's
// CommandComplete and ReadyForQuery are read before the response is dropped;
// an independent PostgreSQL read then proves the commit was durable.
type nha029OwnerCommitACKProxy struct {
	listener net.Listener
	backend  string
	armed    atomic.Bool
	claimed  atomic.Bool
	held     chan struct{}
	release  chan struct{}
	stop     chan struct{}
	abort    bool
	accepted chan struct{}
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	once     sync.Once
	relOnce  sync.Once
}

func nha029OwnerStartCommitACKProxy(t *testing.T, backendPort int) *nha029OwnerCommitACKProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha029OwnerCommitACKProxy{
		listener: listener, backend: net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)),
		held: make(chan struct{}), release: make(chan struct{}), stop: make(chan struct{}),
		accepted: make(chan struct{}), conns: make(map[net.Conn]struct{}),
	}
	go p.accept()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *nha029OwnerCommitACKProxy) port(t *testing.T) int {
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

func (p *nha029OwnerCommitACKProxy) accept() {
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

func (p *nha029OwnerCommitACKProxy) forward(client, backend net.Conn) {
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
		targetStatements := make(map[string]bool)
		// sslmode=disable sends one untagged StartupMessage. All following
		// frontend messages have a type byte and a big-endian length.
		frame, err := nha029OwnerReadStartupFrame(client)
		if err != nil || nha029OwnerWriteAll(backend, frame) != nil {
			return
		}
		for {
			typ, payload, frame, err := nha029OwnerReadTaggedFrame(client)
			if err != nil {
				return
			}
			if typ == 'P' {
				end := bytes.IndexByte(payload, 0)
				if end < 0 {
					return
				}
				targetStatements[string(payload[:end])] = nha029OwnerParseContainsBootstrapUpdate(payload)
			}
			if typ == 'B' {
				portal := bytes.IndexByte(payload, 0)
				if portal < 0 {
					return
				}
				rest := payload[portal+1:]
				end := bytes.IndexByte(rest, 0)
				if end < 0 {
					return
				}
				if targetStatements[string(rest[:end])] {
					bootstrapUpdate.Store(true)
				}
			}
			if typ == 'Q' && nha029OwnerContainsBootstrapUpdate(payload) {
				bootstrapUpdate.Store(true)
			}
			commit := typ == 'Q' && bytes.EqualFold(bytes.TrimSpace(bytes.TrimSuffix(payload, []byte{0})), []byte("commit"))
			rollback := typ == 'Q' && bytes.EqualFold(bytes.TrimSpace(bytes.TrimSuffix(payload, []byte{0})), []byte("rollback"))
			if commit && bootstrapUpdate.Load() && p.armed.Load() && p.claimed.CompareAndSwap(false, true) {
				interceptCommit.Store(true)
				if p.abort {
					payload := []byte("rollback\x00")
					frame = make([]byte, 5+len(payload))
					frame[0] = 'Q'
					binary.BigEndian.PutUint32(frame[1:5], uint32(4+len(payload)))
					copy(frame[5:], payload)
				}
			}
			if commit || rollback {
				bootstrapUpdate.Store(false)
			}
			if nha029OwnerWriteAll(backend, frame) != nil {
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
		typ, payload, frame, err := nha029OwnerReadTaggedFrame(backend)
		if err != nil {
			return
		}
		if typ == 'C' && interceptCommit.Load() && (bytes.Equal(payload, []byte("COMMIT\x00")) || p.abort && bytes.Equal(payload, []byte("ROLLBACK\x00"))) {
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
		if nha029OwnerWriteAll(client, frame) != nil {
			return
		}
	}
}

func nha029OwnerParseContainsBootstrapUpdate(payload []byte) bool {
	endName := bytes.IndexByte(payload, 0)
	if endName < 0 {
		return false
	}
	return nha029OwnerContainsBootstrapUpdate(payload[endName+1:])
}

func nha029OwnerContainsBootstrapUpdate(payload []byte) bool {
	return bytes.Contains(bytes.ToLower(payload), []byte("update public.config_active"))
}

func nha029OwnerReadStartupFrame(r io.Reader) ([]byte, error) {
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

func nha029OwnerReadTaggedFrame(r io.Reader) (byte, []byte, []byte, error) {
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

func nha029OwnerWriteAll(w io.Writer, frame []byte) error {
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

func (p *nha029OwnerCommitACKProxy) allowDrop() { p.relOnce.Do(func() { close(p.release) }) }

func (p *nha029OwnerCommitACKProxy) close(t *testing.T) {
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

// C5 forwards PostgreSQL traffic unchanged. Unlike the older C4 abort fixture,
// it never substitutes ROLLBACK for COMMIT and never fabricates a SQL response.
type nha029C5SQLCut uint8

const (
	nha029C5CutInvalid nha029C5SQLCut = iota
	nha029C5CutBeforeCommit
	nha029C5CutAfterCommit
	nha029C5CutAfterStatement
)

type nha029C5SQLStatement uint8

const (
	nha029C5SQLNone nha029C5SQLStatement = iota
	nha029C5SQLProtectedSource
	nha029C5SQLPlainSource
	nha029C5SQLManifest
	nha029C5SQLRegistry
	nha029C5SQLPolicySource
	nha029C5SQLPolicyActive
	nha029C5SQLActive
	nha029C5SQLAttempt
	nha029C5SQLPolicyAudit
	nha029C5SQLDocumentRead
	nha029C5SQLCommit
	nha029C5SQLRollback
)

type nha029C5SQLBarrier struct {
	pid       uint32
	attempt   [16]byte
	statement nha029C5SQLStatement
	committed bool
}
type nha029C5SQLTicket struct {
	cut       nha029C5SQLCut
	statement nha029C5SQLStatement
	claimed   atomic.Bool
	held      chan nha029C5SQLBarrier
	decision  chan bool
	once      sync.Once
}

func (c *nha029C5SQLTicket) finish(deliver bool) { c.once.Do(func() { c.decision <- deliver }) }

type nha029C5SQLProxy struct {
	listener            net.Listener
	backend             string
	unavailable         atomic.Bool
	acceptedConnections atomic.Uint64
	refusedConnections  atomic.Uint64
	mu                  sync.Mutex
	conns               map[net.Conn]*nha029C5SQLConnection
	ticket              *nha029C5SQLTicket
	stop                chan struct{}
	accepted            chan struct{}
	wg                  sync.WaitGroup
	once                sync.Once
}

func nha029C5StartSQLProxy(t *testing.T, backendPort int) *nha029C5SQLProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha029C5SQLProxy{listener: listener, backend: net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)),
		conns: make(map[net.Conn]*nha029C5SQLConnection), stop: make(chan struct{}), accepted: make(chan struct{})}
	go p.accept()
	t.Cleanup(func() { p.close(t) })
	return p
}
func (p *nha029C5SQLProxy) port() int { return p.listener.Addr().(*net.TCPAddr).Port }
func (p *nha029C5SQLProxy) arm(t *testing.T, cut nha029C5SQLCut, statement nha029C5SQLStatement) *nha029C5SQLTicket {
	t.Helper()
	if cut < nha029C5CutBeforeCommit || cut > nha029C5CutAfterStatement ||
		(cut == nha029C5CutAfterStatement && (statement < nha029C5SQLProtectedSource || statement > nha029C5SQLDocumentRead)) {
		t.Fatal("invalid C5 SQL cut")
	}
	c := &nha029C5SQLTicket{cut: cut, statement: statement, held: make(chan nha029C5SQLBarrier, 1), decision: make(chan bool, 1)}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ticket != nil {
		t.Fatal("C5 SQL proxy already armed")
	}
	p.ticket = c
	return c
}
func (p *nha029C5SQLProxy) setUnavailable(t *testing.T, closed bool, runtimePID uint32) uint64 {
	t.Helper()
	p.unavailable.Store(closed)
	if !closed {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	retained := 0
	for conn, state := range p.conns {
		state.mu.Lock()
		pid := state.pid
		state.mu.Unlock()
		if runtimePID != 0 && pid == runtimePID {
			retained++
			continue
		}
		_ = conn.Close()
	}
	// Only the two ends of the already-held durable runtime session survive.
	// New connections, including every fresh configuration reader, are refused.
	if runtimePID == 0 || retained != 2 {
		t.Fatalf("C5 configuration fault did not preserve the exact runtime session: pid=%d ends=%d", runtimePID, retained)
	}
	// Each admitted transport has exactly two ends in conns. This is the
	// actual number cut by this fault, not a guessed retry/time allowance.
	return uint64((len(p.conns) - retained) / 2)
}

// Counts describe only this loopback fixture. No DSN, credentials, SQL or
// source bytes enter a failure report.
func (p *nha029C5SQLProxy) diagnostic() string {
	p.mu.Lock()
	pairs := len(p.conns) / 2
	p.mu.Unlock()
	return fmt.Sprintf("unavailable=%t accepted=%d refused=%d live_pairs=%d", p.unavailable.Load(), p.acceptedConnections.Load(), p.refusedConnections.Load(), pairs)
}

func (p *nha029C5SQLProxy) accept() {
	defer close(p.accepted)
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		if p.unavailable.Load() {
			p.refusedConnections.Add(1)
			_ = client.Close()
			continue
		}
		backend, err := net.DialTimeout("tcp", p.backend, 5*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		if len(p.conns) >= 64 || p.unavailable.Load() {
			p.refusedConnections.Add(1)
			p.mu.Unlock()
			_ = client.Close()
			_ = backend.Close()
			continue
		}
		state := &nha029C5SQLConnection{}
		p.conns[client], p.conns[backend] = state, state
		p.acceptedConnections.Add(1)
		p.wg.Add(1)
		p.mu.Unlock()
		go p.forwardC5(client, backend, state)
	}
}

type nha029C5SQLMeta struct {
	statement nha029C5SQLStatement
	attempt   [16]byte
}
type nha029C5SQLConnection struct {
	mu      sync.Mutex
	queue   []nha029C5SQLMeta
	pid     uint32
	attempt [16]byte
	active  bool
	commit  *nha029C5SQLTicket
}

func nha029C5SQLClassify(raw []byte) nha029C5SQLStatement {
	// SQL text contains placeholders, not source payloads. Keep only a bounded
	// classification rather than retaining SQL/source strings in the proxy.
	if len(raw) > 65536 {
		return nha029C5SQLNone
	}
	sql := strings.ToLower(strings.Join(strings.Fields(strings.TrimSuffix(string(raw), "\x00")), " "))
	switch {
	case sql == "commit":
		return nha029C5SQLCommit
	case sql == "rollback":
		return nha029C5SQLRollback
	case strings.HasPrefix(sql, "select d.id,d.last_revision,d.kind::text,") && strings.Contains(sql, "from public.config_documents d join public.config_versions v") && strings.HasSuffix(sql, "where d.id=$1"):
		return nha029C5SQLDocumentRead
	case strings.Contains(sql, "insert into public.config_activation_policy_changes"):
		return nha029C5SQLPolicyAudit
	case strings.Contains(sql, "insert into public.config_activation_attempts"):
		return nha029C5SQLAttempt
	case strings.Contains(sql, "update public.config_active "):
		return nha029C5SQLActive
	case strings.Contains(sql, "update public.authorization_policy_active ") || strings.Contains(sql, "insert into public.authorization_policy_active"):
		return nha029C5SQLPolicyActive
	case strings.Contains(sql, "update public.config_generations set sealed=true"):
		return nha029C5SQLManifest
	case strings.Contains(sql, "update public.registry_entities ") || strings.Contains(sql, "insert into public.registry_source_bindings"):
		return nha029C5SQLRegistry
	case strings.Contains(sql, "insert into public.config_versions") && strings.Contains(sql, "ciphertext"):
		return nha029C5SQLProtectedSource
	case strings.Contains(sql, "insert into public.config_versions"):
		return nha029C5SQLPlainSource
	default:
		return nha029C5SQLNone
	}
}
func nha029C5CString(payload []byte) (string, []byte, bool) {
	end := bytes.IndexByte(payload, 0)
	if end < 0 || end > 1024 {
		return "", nil, false
	}
	return string(payload[:end]), payload[end+1:], true
}
func nha029C5Bind(payload []byte) (string, string, [][]byte, bool) {
	portal, rest, ok := nha029C5CString(payload)
	if !ok {
		return "", "", nil, false
	}
	statement, rest, ok := nha029C5CString(rest)
	if !ok || len(rest) < 2 {
		return "", "", nil, false
	}
	formats := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	if formats > 64 || len(rest) < 2*formats+2 {
		return "", "", nil, false
	}
	rest = rest[2*formats:]
	count := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	if count > 64 {
		return "", "", nil, false
	}
	args := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if len(rest) < 4 {
			return "", "", nil, false
		}
		size := int64(int32(binary.BigEndian.Uint32(rest[:4])))
		rest = rest[4:]
		if size == -1 {
			args = append(args, nil)
			continue
		}
		if size < 0 || size > int64(len(rest)) {
			return "", "", nil, false
		}
		args = append(args, rest[:int(size)])
		rest = rest[int(size):]
	}
	if len(rest) < 2 {
		return "", "", nil, false
	}
	results := int(binary.BigEndian.Uint16(rest[:2]))
	if results > 64 || len(rest) != 2+2*results {
		return "", "", nil, false
	}
	return portal, statement, args, true
}
func (p *nha029C5SQLProxy) candidate(cut nha029C5SQLCut, statement nha029C5SQLStatement) *nha029C5SQLTicket {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.ticket
	if c == nil || c.cut != cut || (cut == nha029C5CutAfterStatement && c.statement != statement) || !c.claimed.CompareAndSwap(false, true) {
		return nil
	}
	return c
}
func (p *nha029C5SQLProxy) hold(c *nha029C5SQLTicket, barrier nha029C5SQLBarrier) bool {
	select {
	case c.held <- barrier:
	case <-p.stop:
		return false
	}
	select {
	case deliver := <-c.decision:
		return deliver
	case <-p.stop:
		return false
	}
}
func (p *nha029C5SQLProxy) forwardC5(client, backend net.Conn, state *nha029C5SQLConnection) {
	defer p.wg.Done()
	defer func() {
		_ = client.Close()
		_ = backend.Close()
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, backend)
		p.mu.Unlock()
	}()
	frontDone := make(chan struct{})
	go func() {
		defer close(frontDone)
		defer func() { _ = client.Close(); _ = backend.Close() }()
		frame, err := nha029OwnerReadStartupFrame(client)
		if err != nil || nha029OwnerWriteAll(backend, frame) != nil {
			return
		}
		statements := make(map[string]nha029C5SQLMeta)
		portals := make(map[string]nha029C5SQLMeta)
		for {
			typ, payload, frame, err := nha029OwnerReadTaggedFrame(client)
			if err != nil {
				return
			}
			var meta nha029C5SQLMeta
			execute := false
			switch typ {
			case 'P':
				name, sql, ok := nha029C5CString(payload)
				if !ok || len(statements) >= 512 {
					return
				}
				end := bytes.IndexByte(sql, 0)
				if end < 0 {
					return
				}
				statements[name] = nha029C5SQLMeta{statement: nha029C5SQLClassify(sql[:end])}
			case 'B':
				portal, statement, args, ok := nha029C5Bind(payload)
				if !ok || len(portals) >= 512 {
					return
				}
				meta = statements[statement]
				if meta.statement == nha029C5SQLPlainSource && len(args) > 6 && string(args[6]) == "homeward.authorization/1" {
					meta.statement = nha029C5SQLPolicySource
				}
				if meta.statement == nha029C5SQLAttempt && len(args) > 0 {
					if len(args[0]) == 16 {
						copy(meta.attempt[:], args[0])
					} else if len(args[0]) == 34 && string(args[0][:2]) == "\\x" {
						decoded, err := hex.DecodeString(string(args[0][2:]))
						if err != nil || len(decoded) != 16 {
							return
						}
						copy(meta.attempt[:], decoded)
					} else {
						return
					}
				}
				portals[portal] = meta
			case 'E':
				portal, _, ok := nha029C5CString(payload)
				if !ok {
					return
				}
				meta, execute = portals[portal], true
			case 'Q':
				meta, execute = nha029C5SQLMeta{statement: nha029C5SQLClassify(payload)}, true
			case 'C':
				if len(payload) < 2 {
					return
				}
				name, _, ok := nha029C5CString(payload[1:])
				if !ok {
					return
				}
				if payload[0] == 'S' {
					delete(statements, name)
				} else if payload[0] == 'P' {
					delete(portals, name)
				}
			}
			if execute {
				state.mu.Lock()
				if len(state.queue) >= 64 {
					state.mu.Unlock()
					return
				}
				if meta.statement == nha029C5SQLAttempt {
					state.attempt = meta.attempt
				}
				if meta.statement == nha029C5SQLActive {
					state.active = true
				}
				barrier := nha029C5SQLBarrier{pid: state.pid, attempt: state.attempt, statement: meta.statement}
				active := state.active
				state.mu.Unlock()
				if meta.statement == nha029C5SQLCommit && active {
					if c := p.candidate(nha029C5CutBeforeCommit, meta.statement); c != nil && !p.hold(c, barrier) {
						return
					}
					if c := p.candidate(nha029C5CutAfterCommit, meta.statement); c != nil {
						state.mu.Lock()
						state.commit = c
						state.mu.Unlock()
					}
				}
				state.mu.Lock()
				state.queue = append(state.queue, meta)
				state.mu.Unlock()
			}
			if nha029OwnerWriteAll(backend, frame) != nil {
				return
			}
		}
	}()
	defer func() { _ = client.Close(); _ = backend.Close(); <-frontDone }()
	var held [][]byte
	var ticket *nha029C5SQLTicket
	var barrier nha029C5SQLBarrier
	for {
		typ, payload, frame, err := nha029OwnerReadTaggedFrame(backend)
		if err != nil {
			return
		}
		if typ == 'K' && len(payload) == 8 {
			state.mu.Lock()
			state.pid = binary.BigEndian.Uint32(payload[:4])
			state.mu.Unlock()
		}
		if typ == 'C' {
			state.mu.Lock()
			var meta nha029C5SQLMeta
			if len(state.queue) > 0 {
				meta, state.queue = state.queue[0], state.queue[1:]
			}
			barrier = nha029C5SQLBarrier{pid: state.pid, attempt: state.attempt, statement: meta.statement}
			if meta.statement == nha029C5SQLCommit && bytes.Equal(payload, []byte("COMMIT\x00")) {
				ticket = state.commit
				barrier.committed = ticket != nil
			}
			state.mu.Unlock()
			if ticket == nil {
				ticket = p.candidate(nha029C5CutAfterStatement, meta.statement)
			}
			if ticket != nil {
				held = append(held, frame)
				continue
			}
		}
		if len(held) > 0 {
			if len(held) >= 8 {
				return
			}
			held = append(held, frame)
			if typ != 'Z' {
				continue
			}
			if len(payload) != 1 || (barrier.committed && payload[0] != 'I') || (!barrier.committed && payload[0] != 'T') {
				return
			}
			if !p.hold(ticket, barrier) {
				return
			}
			for _, saved := range held {
				if nha029OwnerWriteAll(client, saved) != nil {
					return
				}
			}
			held, ticket = nil, nil
			continue
		}
		if typ == 'Z' {
			state.mu.Lock()
			state.queue = nil
			if len(payload) == 1 && payload[0] == 'I' {
				state.active = false
				state.attempt = [16]byte{}
				state.commit = nil
			}
			state.mu.Unlock()
		}
		if nha029OwnerWriteAll(client, frame) != nil {
			return
		}
	}
}
func (p *nha029C5SQLProxy) close(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		close(p.stop)
		_ = p.listener.Close()
		<-p.accepted
		p.mu.Lock()
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		joined := make(chan struct{})
		go func() { p.wg.Wait(); close(joined) }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		select {
		case <-joined:
		case <-ctx.Done():
			t.Error("C5 owned PostgreSQL proxy did not join")
		}
	})
}
