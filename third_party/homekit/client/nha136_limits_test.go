package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brutella/hc/db"
	"github.com/brutella/hc/hap"
	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

func TestNHA136ResponseBoundsAndRedirects(t *testing.T) {
	if _, err := readResponseBody(bytes.NewReader(make([]byte, maximumResponseBytes))); err != nil {
		t.Fatal(err)
	}
	if _, err := readResponseBody(bytes.NewReader(make([]byte, maximumResponseBytes+1))); err == nil {
		t.Fatal("oversized body admitted")
	}
	if err := rejectRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirect followed")
	}
}

// Network adversaries use ordinary loopback HTTP for pair-verify, followed by
// independently framed AEAD records. No hc.Connection.Write/Encrypt is used.
type nha136Peer struct {
	server             *httptest.Server
	accessory          AccessoryConnectionConfig
	controller         *ControllerIdentity
	verify, gets, puts atomic.Int32
	readBytes          atomic.Int64
	release            chan struct{}
	joined             chan struct{}
	hijacked           atomic.Bool
	mu                 sync.Mutex
	conn               net.Conn
}

type nha136CountedConn struct {
	net.Conn
	count *atomic.Int64
}

func (c *nha136CountedConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	c.count.Add(int64(n))
	return n, e
}

type nha136FrameReader struct {
	reader io.Reader
	cipher cipher.AEAD
	count  uint64
	plain  []byte
}

func (r *nha136FrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.plain) == 0 {
		var header [2]byte
		if _, e := io.ReadFull(r.reader, header[:]); e != nil {
			return 0, e
		}
		size := int(binary.LittleEndian.Uint16(header[:]))
		if size > 1024 {
			return 0, fmt.Errorf("bad test peer record")
		}
		sealed := make([]byte, size+16)
		if _, e := io.ReadFull(r.reader, sealed); e != nil {
			return 0, e
		}
		var nonce [12]byte
		binary.LittleEndian.PutUint64(nonce[4:], r.count)
		plain, e := r.cipher.Open(nil, nonce[:], sealed, header[:])
		if e != nil {
			return 0, e
		}
		r.count++
		r.plain = plain
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}
func nha136Cipher(t *testing.T, shared [32]byte, info string) cipher.AEAD {
	t.Helper()
	key := make([]byte, 32)
	if _, e := io.ReadFull(hkdf.New(sha512.New, shared[:], []byte("Control-Salt"), []byte(info)), key); e != nil {
		t.Fatal(e)
	}
	a, e := chacha20poly1305.New(key)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func nha136HTTPResponse(total int, large bool) []byte {
	base := []byte(`{"accessories":[]}`)
	if large {
		body := append(base, bytes.Repeat([]byte(" "), maximumResponseBytes+256*1024)...)
		return append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/hap+json\r\nContent-Length: %d\r\n\r\n", len(body))), body...)
	}
	for size := len(base); size < total; size++ {
		header := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/hap+json\r\nContent-Length: %d\r\n\r\n", size)
		if len(header)+size == total {
			return append([]byte(header), append(base, bytes.Repeat([]byte(" "), size-len(base))...)...)
		}
	}
	panic("invalid literal response size")
}
func nha136NewPeer(t *testing.T, mode string, response []byte, short bool) *nha136Peer {
	t.Helper()
	database := &memoryDB{}
	device, e := hap.NewSecuredDevice("nha136-synthetic-peer", "111-22-333", database)
	if e != nil {
		t.Fatal(e)
	}
	identity, e := NewRandomControllerConfig()
	if e != nil {
		t.Fatal(e)
	}
	database.MustSaveEntity(db.NewEntity(identity.DeviceID, identity.PublicKey, nil))
	state := pair.NewVerifyServerController(database, hap.NewContextForSecuredDevice(device))
	peer := &nha136Peer{controller: identity, release: make(chan struct{}), joined: make(chan struct{})}
	var m2 []byte
	peer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pair-verify" || r.Method != http.MethodPost {
			if r.Method == http.MethodGet {
				peer.gets.Add(1)
			}
			if r.Method == http.MethodPut {
				peer.puts.Add(1)
			}
			http.Error(w, "protected request before verify", 403)
			return
		}
		call := peer.verify.Add(1)
		w.Header().Set("Content-Type", hap.HTTPContentTypePairingTLV8)
		if mode == "M4-first" {
			w.Write([]byte{6, 1, 4})
			return
		}
		if mode == "M2-repeat" && call == 2 {
			w.Write(m2)
			return
		}
		input, e := util.NewTLV8ContainerFromReader(r.Body)
		if e != nil {
			t.Error(e)
			return
		}
		out, e := state.Handle(input)
		if e != nil || out == nil {
			t.Error("synthetic handshake", e)
			return
		}
		body := out.BytesBuffer().Bytes()
		if call == 1 {
			m2 = append([]byte(nil), body...)
			w.Write(body)
			return
		}
		conn, buffered, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		peer.mu.Lock()
		peer.conn = conn
		peer.mu.Unlock()
		peer.hijacked.Store(true)
		defer close(peer.joined)
		defer conn.Close()
		fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/pairing+tlv8\r\nContent-Length: %d\r\n\r\n", len(body))
		buffered.Write(body)
		if e := buffered.Flush(); e != nil {
			return
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		incoming := &nha136FrameReader{reader: buffered, cipher: nha136Cipher(t, state.SharedKey(), "Control-Write-Encryption-Key")}
		request, e := http.ReadRequest(bufio.NewReader(incoming))
		if e != nil {
			return
		}
		if request.Method == http.MethodGet {
			peer.gets.Add(1)
		}
		if request.Method == http.MethodPut {
			peer.puts.Add(1)
		}
		request.Body.Close()
		outgoing := nha136Cipher(t, state.SharedKey(), "Control-Read-Encryption-Key")
		var wire []byte
		var count uint64
		for rest := response; len(rest) > 0; {
			size := 1024
			if short {
				size = 43
			}
			if size > len(rest) {
				size = len(rest)
			}
			var header [2]byte
			binary.LittleEndian.PutUint16(header[:], uint16(size))
			var nonce [12]byte
			binary.LittleEndian.PutUint64(nonce[4:], count)
			count++
			wire = append(wire, header[:]...)
			wire = outgoing.Seal(wire, nonce[:], rest[:size], header[:])
			rest = rest[size:]
		}
		// A single coalesced write deliberately disagrees with hc's record boundaries.
		if _, e := conn.Write(wire); e != nil {
			return
		}
		<-peer.release
	}))
	host, port, e := net.SplitHostPort(peer.server.Listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	number, e := strconv.Atoi(port)
	if e != nil {
		t.Fatal(e)
	}
	peer.accessory = AccessoryConnectionConfig{DeviceID: device.Name(), PublicKey: device.PublicKey(), IPConnectionInfo: IPConnectionInfo{IPAddress: host, Port: number}}
	t.Cleanup(func() {
		close(peer.release)
		peer.mu.Lock()
		if peer.conn != nil {
			peer.conn.Close()
		}
		peer.mu.Unlock()
		peer.server.Close()
		if peer.hijacked.Load() {
			select {
			case <-peer.joined:
			case <-time.After(time.Second):
				t.Error("synthetic encrypted peer did not join")
			}
		}
	})
	return peer
}
func (p *nha136Peer) client(t *testing.T) *AccessoryClient {
	t.Helper()
	address := p.server.Listener.Addr().String()
	c := NewAccessoryClient(func(ctx context.Context, network, target string) (net.Conn, error) {
		if network != "tcp" || target != address {
			return nil, fmt.Errorf("outside owned loopback")
		}
		raw, e := (&net.Dialer{}).DialContext(ctx, network, target)
		if e != nil {
			return nil, e
		}
		return &nha136CountedConn{Conn: raw, count: &p.readBytes}, nil
	}, p.controller, &p.accessory)
	t.Cleanup(func() { c.Close() })
	return c
}
func TestNHA136PairVerifyNetworkSequenceFailClosed(t *testing.T) {
	for _, mode := range []string{"M4-first", "M2-repeat", "nominal"} {
		t.Run(mode, func(t *testing.T) {
			peer := nha136NewPeer(t, mode, nha136HTTPResponse(1023, false), false)
			c := peer.client(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, e := c.Accessories(ctx)
			if mode == "nominal" {
				if e != nil || len(result) != 0 || peer.gets.Load() != 1 {
					t.Fatal("nominal authenticated endpoint", e)
				}
				return
			}
			if e == nil || ctx.Err() != nil || !strings.Contains(e.Error(), "sequence") {
				t.Fatal("unexpected sequence lacked explicit error", e)
			}
			expected := int32(1)
			if mode == "M2-repeat" {
				expected = 2
			}
			if peer.verify.Load() != expected || peer.gets.Load() != 0 || peer.puts.Load() != 0 {
				t.Fatal("invalid sequence reached protected HTTP or retried")
			}
		})
	}
}
func TestNHA136IndependentEncryptedHTTPResponseBound(t *testing.T) {
	for _, test := range []struct {
		name         string
		size         int
		short, large bool
	}{{"1023", 1023, false, false}, {"1024-keepalive", 1024, false, false}, {"1025", 1025, false, false}, {"coalesced-short", 1024, true, false}, {"body-over-1MiB", 0, false, true}} {
		t.Run(test.name, func(t *testing.T) {
			peer := nha136NewPeer(t, "nominal", nha136HTTPResponse(test.size, test.large), test.short)
			c := peer.client(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, e := c.Accessories(ctx)
			if test.large {
				if e == nil || ctx.Err() != nil || !strings.Contains(e.Error(), "exceeds") {
					t.Fatal("HTTP response limit was bypassed or only timed out", e)
				}
				if peer.readBytes.Load() > int64(maximumResponseBytes+64*1024) {
					t.Fatal("encrypted layer consumed beyond HTTP limit before returning", peer.readBytes.Load())
				}
			} else if e != nil {
				t.Fatal("independently framed valid response", e)
			}
			if peer.gets.Load() != 1 || peer.puts.Load() != 0 {
				t.Fatal("response framing changed protected request count")
			}
		})
	}
}
