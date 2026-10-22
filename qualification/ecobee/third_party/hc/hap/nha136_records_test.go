package hap

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	hcrypto "github.com/brutella/hc/crypto"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Independent record framing: never calls hc.Encrypt or Connection.Write.
func nha136IndependentFrames(t *testing.T, shared [32]byte, pieces [][]byte) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha512.New, shared[:], []byte("Control-Salt"), []byte("Control-Read-Encryption-Key")), key); err != nil {
		t.Fatal(err)
	}
	cipher, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	var result []byte
	for count, piece := range pieces {
		var header [2]byte
		binary.LittleEndian.PutUint16(header[:], uint16(len(piece)))
		var nonce [12]byte
		binary.LittleEndian.PutUint64(nonce[4:], uint64(count))
		result = append(result, header[:]...)
		result = cipher.Seal(result, nonce[:], piece, header[:])
	}
	return result
}

func nha136ReadConnection(t *testing.T, encrypted []byte, check func(*Connection)) {
	t.Helper()
	left, right := net.Pipe()
	ctx := NewContextForSecuredDevice(nil)
	conn := NewConnection(left, ctx)
	cryptographer, err := hcrypto.NewSecureClientSessionFromSharedKey([32]byte{136})
	if err != nil {
		t.Fatal(err)
	}
	ctx.GetSessionForConnection(conn).SetCryptographer(cryptographer)
	release, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		if _, err := right.Write(encrypted); err == nil {
			<-release
		}
	}()
	t.Cleanup(func() {
		close(release)
		conn.Close()
		right.Close()
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("record writer did not join")
		}
	})
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	check(conn)
}

func TestNHA136IndependentRecordStreamingAndErrors(t *testing.T) {
	for _, size := range []int{1023, 1024, 1025} {
		name := map[int]string{1023: "1023", 1024: "1024", 1025: "1025"}[size]
		t.Run(name, func(t *testing.T) {
			want := bytes.Repeat([]byte{0x5a}, size)
			pieces := [][]byte{want}
			if size > 1024 {
				pieces = [][]byte{want[:1024], want[1024:]}
			}
			nha136ReadConnection(t, nha136IndependentFrames(t, [32]byte{136}, pieces), func(conn *Connection) {
				var got []byte
				for len(got) < len(want) {
					var small [3]byte
					n := len(want) - len(got)
					if n > len(small) {
						n = len(small)
					}
					count, err := conn.Read(small[:n])
					if err != nil || count == 0 {
						t.Fatal("stream failed before requested bytes", count, err)
					}
					got = append(got, small[:count]...)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("split destination lost plaintext")
				}
			})
		})
	}
	t.Run("coalesced-short-records", func(t *testing.T) {
		pieces := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
		want := bytes.Join(pieces, nil)
		nha136ReadConnection(t, nha136IndependentFrames(t, [32]byte{136}, pieces), func(conn *Connection) {
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, want) {
				t.Fatal("encrypted read-ahead lost", err)
			}
		})
	})
	t.Run("bad-MAC-no-plaintext", func(t *testing.T) {
		frame := nha136IndependentFrames(t, [32]byte{136}, [][]byte{[]byte("untrusted")})
		frame[len(frame)-1] ^= 1
		nha136ReadConnection(t, frame, func(conn *Connection) {
			b := make([]byte, 64)
			n, err := conn.Read(b)
			if n != 0 || err == nil {
				t.Fatal("MAC failure became plaintext or0,nil", n, err)
			}
		})
	})
	t.Run("oversized-length-before-payload", func(t *testing.T) {
		nha136ReadConnection(t, []byte{1, 4}, func(conn *Connection) {
			n, err := conn.Read(make([]byte, 1))
			if n != 0 || err == nil {
				t.Fatal("record length1025 admitted", n, err)
			}
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("oversized length awaited payload")
			}
		})
	})
	t.Run("partial-frame-timeout-preserved", func(t *testing.T) {
		// Complete header, incomplete ciphertext: report the original timeout.
		nha136ReadConnection(t, []byte{32, 0, 1, 2, 3}, func(conn *Connection) {
			conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			n, err := conn.Read(make([]byte, 32))
			e, ok := err.(net.Error)
			if n != 0 || !ok || !e.Timeout() {
				t.Fatal("partial-frame deadline hidden", n, err)
			}
		})
	})
}

// The finite source has two distinct records with counter0 then counter1. Full
// plaintext and a stable EOF prove that no frame was skipped or consumed twice.
func TestNHA136FiniteRecordReaderEOFAndCounter(t *testing.T) {
	for _, shape := range []string{"two-records", "empty-then-data", "empty-source", "truncated-header", "truncated-body"} {
		t.Run(shape, func(t *testing.T) {
			pieces := [][]byte{[]byte("counter-zero"), []byte("counter-one")}
			if shape == "empty-then-data" {
				pieces = [][]byte{nil, []byte("counter-one")}
			}
			wire := nha136IndependentFrames(t, [32]byte{136}, pieces)
			want := bytes.Join(pieces, nil)
			switch shape {
			case "empty-source":
				wire = nil
				want = nil
			case "truncated-header":
				wire = wire[:1]
			case "truncated-body":
				wire = wire[:len(wire)-1]
			}
			cryptographer, err := hcrypto.NewSecureClientSessionFromSharedKey([32]byte{136})
			if err != nil {
				t.Fatal(err)
			}
			reader, initial := cryptographer.Decrypt(bytes.NewReader(wire))
			if reader == nil {
				t.Fatal("decrypt omitted its terminal reader", initial)
			}
			if shape == "empty-source" {
				if initial != io.EOF {
					t.Fatal("empty stream did not report EOF", initial)
				}
			} else if shape == "truncated-header" {
				if !errors.Is(initial, io.ErrUnexpectedEOF) {
					t.Fatal("partial header lost read error", initial)
				}
			} else if initial != nil {
				t.Fatal("complete first record failed", initial)
			}
			// An empty destination must neither advance the frame nor hide an error.
			if n, err := reader.Read(nil); n != 0 || err != nil {
				t.Fatal("empty Read changed state", n, err)
			}
			got, final := io.ReadAll(reader)
			if shape == "truncated-header" || shape == "truncated-body" {
				if !errors.Is(final, io.ErrUnexpectedEOF) {
					t.Fatal("truncated finite source error lost", final)
				}
				if shape == "truncated-body" && !bytes.Equal(got, pieces[0]) {
					t.Fatal("unauthenticated trailing plaintext delivered")
				}
				if shape == "truncated-header" && len(got) != 0 {
					t.Fatal("partial header yielded plaintext")
				}
				return
			}
			if final != nil || !bytes.Equal(got, want) {
				t.Fatal("finite record stream or counter changed", final, len(got))
			}
			for i := 0; i < 2; i++ {
				if n, err := reader.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Fatal("terminal EOF not stable", n, err)
				}
			}
		})
	}
}

func TestNHA136FragmentedAndTimeoutResumedRecords(t *testing.T) {
	for _, shape := range []string{"one-byte-fragments", "header-timeout-resume", "body-timeout-resume", "mac-timeout-resume"} {
		t.Run(shape, func(t *testing.T) {
			want := []byte("complete record followed by the next nonce")
			wire := nha136IndependentFrames(t, [32]byte{136}, [][]byte{want[:15], want[15:]})
			left, right := net.Pipe()
			ctx := NewContextForSecuredDevice(nil)
			conn := NewConnection(left, ctx)
			cryptographer, err := hcrypto.NewSecureClientSessionFromSharedKey([32]byte{136})
			if err != nil {
				t.Fatal(err)
			}
			ctx.GetSessionForConnection(conn).SetCryptographer(cryptographer)
			resume, joined := make(chan struct{}), make(chan struct{})
			outcome := make(chan error, 1)
			go func() {
				defer close(joined)
				defer right.Close()
				if shape == "one-byte-fragments" {
					for _, value := range wire {
						if _, err := right.Write([]byte{value}); err != nil {
							outcome <- err
							return
						}
					}
				} else {
					prefix := 1
					if shape == "body-timeout-resume" {
						prefix = 5
					}
					if shape == "mac-timeout-resume" {
						prefix = 2 + 15 + 4
					}
					if _, err := right.Write(wire[:prefix]); err != nil {
						outcome <- err
						return
					}
					<-resume
					if _, err := right.Write(wire[prefix:]); err != nil {
						outcome <- err
						return
					}
				}
				outcome <- nil
			}()
			released := false
			t.Cleanup(func() {
				if !released {
					close(resume)
				}
				conn.Close()
				right.Close()
				select {
				case <-joined:
				case <-time.After(time.Second):
					t.Error("fragmented record writer did not join")
				}
			})
			if shape != "one-byte-fragments" {
				if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				n, err := conn.Read(make([]byte, 64))
				timeout, ok := err.(net.Error)
				if n != 0 || !ok || !timeout.Timeout() {
					t.Fatal("partial frame did not preserve timeout", n, err)
				}
				close(resume)
				released = true
			}
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, want) {
				t.Fatal("fragmented/resumed stream lost bytes or advanced nonce", err)
			}
			if n, err := conn.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatal("finite connection did not reach EOF", n, err)
			}
			select {
			case err := <-outcome:
				if err != nil {
					t.Fatal("fragmented record writer", err)
				}
			case <-time.After(time.Second):
				t.Fatal("writer outcome missing")
			}
		})
	}
}

func TestNHA136RejectedRecordDoesNotConsumeNonce(t *testing.T) {
	want := []byte("authenticated nonce zero then one")
	wire := nha136IndependentFrames(t, [32]byte{136}, [][]byte{want[:13], want[13:]})
	bad := append([]byte(nil), wire[:2+13+16]...)
	bad[len(bad)-1] ^= 1
	cryptographer, err := hcrypto.NewSecureClientSessionFromSharedKey([32]byte{136})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := cryptographer.Decrypt(bytes.NewReader(bad))
	if err == nil || failed == nil {
		t.Fatal("bad MAC did not return a terminal reader and error")
	}
	if n, again := failed.Read(make([]byte, 64)); n != 0 || again == nil {
		t.Fatal("failed MAC exposed plaintext or forgot its error", n, again)
	}
	// A new source on the same cryptographer starts with the same nonce0. This
	// directly checks AEAD failure does not commit a receive counter. The HAP
	// connection still closes on authentication failure and is never reused.
	good, err := cryptographer.Decrypt(bytes.NewReader(wire))
	if err != nil {
		t.Fatal("failed MAC consumed nonce0", err)
	}
	got, err := io.ReadAll(good)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("receive counter moved before authentication", err)
	}
}
