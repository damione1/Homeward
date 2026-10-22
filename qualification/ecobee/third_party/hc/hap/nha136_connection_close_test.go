package hap

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hcrypto "github.com/brutella/hc/crypto"
)

// Channels force the interleaving; the timer is only a deadlock watchdog.
func nha136Await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled I/O did not reach its barrier")
	}
}

type nha136CloseContext struct {
	Context
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	lookups atomic.Int32
}

func (c *nha136CloseContext) GetSessionForConnection(conn net.Conn) Session {
	s := c.Context.GetSessionForConnection(conn)
	c.lookups.Add(1)
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
	}
	return s
}

type nha136CloseConn struct {
	mu           sync.Mutex
	closed       bool
	block        bool
	shortWrite   bool
	writeErr     error
	reads        int
	writes       [][]byte
	done         chan struct{}
	readEntered  chan struct{}
	writeEntered chan struct{}
}

func nha136NewCloseConn(block bool) *nha136CloseConn {
	return &nha136CloseConn{block: block, done: make(chan struct{}), readEntered: make(chan struct{}), writeEntered: make(chan struct{})}
}
func (c *nha136CloseConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.reads++
	block := c.block
	c.mu.Unlock()
	if block {
		close(c.readEntered)
		<-c.done
		return 0, net.ErrClosed
	}
	return copy(b, "wire"), nil
}
func (c *nha136CloseConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.writes = append(c.writes, append([]byte(nil), b...))
	block := c.block
	c.mu.Unlock()
	if block {
		close(c.writeEntered)
		<-c.done
		return 0, net.ErrClosed
	}
	if c.shortWrite {
		return len(b) - 1, nil
	}
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}
func (c *nha136CloseConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}
func (c *nha136CloseConn) LocalAddr() net.Addr              { return &net.TCPAddr{Port: 136} }
func (c *nha136CloseConn) RemoteAddr() net.Addr             { return &net.TCPAddr{Port: 1136} }
func (c *nha136CloseConn) SetDeadline(time.Time) error      { return nil }
func (c *nha136CloseConn) SetReadDeadline(time.Time) error  { return nil }
func (c *nha136CloseConn) SetWriteDeadline(time.Time) error { return nil }

type nha136CloseCrypto struct {
	encryptErr error
	readerErr  error
	nilReader  bool
	encrypts   atomic.Int32
	decrypts   atomic.Int32
}

func (c *nha136CloseCrypto) Encrypt(r io.Reader) (io.Reader, error) {
	c.encrypts.Add(1)
	if c.encryptErr != nil {
		return nil, c.encryptErr
	}
	if c.nilReader {
		return nil, nil
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if c.readerErr != nil {
		return nha136FailedCipherReader{c.readerErr}, nil
	}
	return bytes.NewReader(append([]byte("sealed:"), b...)), nil
}
func (c *nha136CloseCrypto) Decrypt(r io.Reader) (io.Reader, error) { c.decrypts.Add(1); return r, nil }

type nha136FailedCipherReader struct{ err error }

func (r nha136FailedCipherReader) Read(b []byte) (int, error) {
	return copy(b, "partial-ciphertext"), r.err
}

func nha136InstallCloseCrypto(ctx Context, conn *Connection) *nha136CloseCrypto {
	c := &nha136CloseCrypto{}
	s := ctx.GetSessionForConnection(conn)
	s.SetCryptographer(c)
	_ = s.Decrypter()
	return c
}

type nha136IOResult struct {
	n   int
	err error
}

func TestNHA136CloseDuringSessionLookupFailsClosed(t *testing.T) {
	for _, method := range []string{"Write", "Read", "EncryptedWrite", "DecryptedRead"} {
		t.Run(method, func(t *testing.T) {
			raw := nha136NewCloseConn(false)
			ctx := &nha136CloseContext{Context: NewContextForSecuredDevice(nil), entered: make(chan struct{}), release: make(chan struct{})}
			conn := NewConnection(raw, ctx)
			t.Cleanup(func() { _ = conn.Close() })
			cipher := nha136InstallCloseCrypto(ctx, conn)
			before := ctx.lookups.Load()
			ctx.armed.Store(true)
			result := make(chan nha136IOResult, 1)
			go func() {
				var n int
				var err error
				switch method {
				case "Write":
					n, err = conn.Write([]byte("secret"))
				case "Read":
					n, err = conn.Read(make([]byte, 32))
				case "EncryptedWrite":
					n, err = conn.EncryptedWrite([]byte("secret"))
				case "DecryptedRead":
					n, err = conn.DecryptedRead(make([]byte, 32))
				}
				result <- nha136IOResult{n, err}
			}()
			nha136Await(t, ctx.entered)
			// The lookup has captured the old session. Delete it before that lookup
			// returns, reproducing the original gap before the second crypto lookup.
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			close(ctx.release)
			got := <-result
			if got.n != 0 || !errors.Is(got.err, net.ErrClosed) {
				t.Fatalf("result=%+v", got)
			}
			if ctx.lookups.Load()-before != 1 {
				t.Fatal("operation performed multiple session lookups")
			}
			if cipher.encrypts.Load() != 0 || cipher.decrypts.Load() != 0 {
				t.Fatal("crypto ran after observed Close")
			}
			if len(raw.writes) != 0 || raw.reads != 0 {
				t.Fatal("closed operation reached raw I/O")
			}
		})
	}
}

func TestNHA136ConcurrentReadWriteCloseUnblocksBothDirections(t *testing.T) {
	raw := nha136NewCloseConn(true)
	ctx := NewContextForSecuredDevice(nil)
	conn := NewConnection(raw, ctx)
	t.Cleanup(func() { _ = conn.Close() })
	nha136InstallCloseCrypto(ctx, conn)
	results := make(chan nha136IOResult, 2)
	go func() { n, err := conn.Read(make([]byte, 32)); results <- nha136IOResult{n, err} }()
	go func() { n, err := conn.Write([]byte("secret")); results <- nha136IOResult{n, err} }()
	nha136Await(t, raw.readEntered)
	nha136Await(t, raw.writeEntered)
	closed := make(chan struct{})
	go func() { _ = conn.Close(); close(closed) }()
	nha136Await(t, closed)
	for i := 0; i < 2; i++ {
		got := <-results
		if got.n != 0 || !errors.Is(got.err, net.ErrClosed) {
			t.Fatalf("result=%+v", got)
		}
	}
	if len(raw.writes) != 1 || string(raw.writes[0]) != "sealed:secret" {
		t.Fatalf("wire=%q", raw.writes)
	}
	if ctx.GetSessionForConnection(conn) != nil {
		t.Fatal("closed session remains registered")
	}
	if n, err := conn.Write([]byte("later")); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("post-Close Write=%d,%v", n, err)
	}
	if n, err := conn.Read(make([]byte, 32)); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("post-Close Read=%d,%v", n, err)
	}
}

func TestNHA136MissingSessionNeverFallsBackToPlaintext(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-pairing", true: "after-pairing"}[encrypted], func(t *testing.T) {
			raw := nha136NewCloseConn(false)
			ctx := NewContextForSecuredDevice(nil)
			conn := NewConnection(raw, ctx)
			if encrypted {
				nha136InstallCloseCrypto(ctx, conn)
			}
			ctx.DeleteSessionForConnection(conn)
			if n, err := conn.Write([]byte("secret")); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Write=%d,%v", n, err)
			}
			if n, err := conn.Read(make([]byte, 32)); n != 0 || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Read=%d,%v", n, err)
			}
			if len(raw.writes) != 0 || raw.reads != 0 || !raw.closed {
				t.Fatal("missing session did not fail closed")
			}
		})
	}
}

func TestNHA136EncryptionFailuresReturnErrorAndClose(t *testing.T) {
	sentinel := errors.New("controlled encryption failure")
	for _, kind := range []string{"Encrypt-error", "ReadAll-error", "nil-reader", "short-write", "write-error"} {
		t.Run(kind, func(t *testing.T) {
			raw := nha136NewCloseConn(false)
			ctx := NewContextForSecuredDevice(nil)
			conn := NewConnection(raw, ctx)
			cipher := nha136InstallCloseCrypto(ctx, conn)
			switch kind {
			case "Encrypt-error":
				cipher.encryptErr = sentinel
			case "ReadAll-error":
				cipher.readerErr = sentinel
			case "nil-reader":
				cipher.nilReader = true
			case "short-write":
				raw.shortWrite = true
			case "write-error":
				raw.writeErr = sentinel
			}
			n, err := conn.Write([]byte("secret"))
			if n != 0 || err == nil {
				t.Fatalf("Write=%d,%v", n, err)
			}
			if kind != "nil-reader" && kind != "short-write" && !errors.Is(err, sentinel) {
				t.Fatalf("cause lost: %v", err)
			}
			if kind == "short-write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short write lost: %v", err)
			}
			if !raw.closed {
				t.Fatal("failed encryption left the socket open")
			}
			if kind != "short-write" && kind != "write-error" && len(raw.writes) != 0 {
				t.Fatal("failed encryption reached the socket")
			}
			if _, err := conn.Write([]byte("later")); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("terminal state lost: %v", err)
			}
		})
	}
}

func TestNHA136PairingPlaintextTransitionAndWriteCount(t *testing.T) {
	raw := nha136NewCloseConn(false)
	ctx := NewContextForSecuredDevice(nil)
	conn := NewConnection(raw, ctx)
	defer conn.Close()
	if n, err := conn.Write([]byte("pairing")); n != 7 || err != nil {
		t.Fatalf("pairing Write=%d,%v", n, err)
	}
	s := ctx.GetSessionForConnection(conn)
	s.SetCryptographer(&nha136CloseCrypto{})
	// Pair-verify response remains plaintext until the next incoming Read.
	if n, err := conn.Write([]byte("response")); n != 8 || err != nil {
		t.Fatalf("response Write=%d,%v", n, err)
	}
	if _, err := conn.Read(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Write([]byte("secret")); n != 6 || err != nil {
		t.Fatalf("encrypted Write count=%d,%v", n, err)
	}
	if len(raw.writes) != 3 || string(raw.writes[0]) != "pairing" || string(raw.writes[1]) != "response" || string(raw.writes[2]) != "sealed:secret" {
		t.Fatalf("wire=%q", raw.writes)
	}
}

var _ net.Conn = (*nha136CloseConn)(nil)
var _ hcrypto.Cryptographer = (*nha136CloseCrypto)(nil)
