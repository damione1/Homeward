package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nha032CommitACKProxy only accepts local PostgreSQL test traffic. It forwards
// complete protocol frames and withholds the successful COMMIT response for
// one transaction which updated the analytical cursor. The backend's
// CommandComplete and ReadyForQuery are read before the response is dropped;
// an independent PostgreSQL read then proves the commit was durable.
type nha032CommitACKProxy struct {
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

func nha032StartCommitACKProxy(t *testing.T, backendPort int) *nha032CommitACKProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha032CommitACKProxy{
		listener: listener, backend: net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)),
		held: make(chan struct{}), release: make(chan struct{}), stop: make(chan struct{}),
		accepted: make(chan struct{}), conns: make(map[net.Conn]struct{}),
	}
	go p.accept()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *nha032CommitACKProxy) port(t *testing.T) int {
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

func (p *nha032CommitACKProxy) accept() {
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

func (p *nha032CommitACKProxy) forward(client, backend net.Conn) {
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
		frame, err := nha032ReadStartupFrame(client)
		if err != nil || nha032WriteAll(backend, frame) != nil {
			return
		}
		for {
			typ, payload, frame, err := nha032ReadTaggedFrame(client)
			if err != nil {
				return
			}
			if (typ == 'P' && nha032ParseContainsHistoryUpdate(payload)) ||
				(typ == 'Q' && nha032ContainsHistoryUpdate(payload)) {
				historyUpdate.Store(true)
			}
			if typ == 'Q' && bytes.EqualFold(bytes.TrimSpace(bytes.TrimSuffix(payload, []byte{0})), []byte("commit")) &&
				historyUpdate.Load() && p.armed.Load() && p.claimed.CompareAndSwap(false, true) {
				interceptCommit.Store(true)
			}
			if nha032WriteAll(backend, frame) != nil {
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
		typ, payload, frame, err := nha032ReadTaggedFrame(backend)
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
		if nha032WriteAll(client, frame) != nil {
			return
		}
	}
}

func nha032ParseContainsHistoryUpdate(payload []byte) bool {
	endName := bytes.IndexByte(payload, 0)
	if endName < 0 {
		return false
	}
	return nha032ContainsHistoryUpdate(payload[endName+1:])
}

func nha032ContainsHistoryUpdate(payload []byte) bool {
	return bytes.Contains(bytes.ToLower(payload), []byte("update public.recorder_maintenance_head"))
}

func nha032ReadStartupFrame(r io.Reader) ([]byte, error) {
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

func nha032ReadTaggedFrame(r io.Reader) (byte, []byte, []byte, error) {
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

func nha032WriteAll(w io.Writer, frame []byte) error {
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

func (p *nha032CommitACKProxy) allowDrop() { p.relOnce.Do(func() { close(p.release) }) }

func (p *nha032CommitACKProxy) close(t *testing.T) {
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
