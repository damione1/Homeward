package mqtt

import (
	"errors"
	"io"
	"net"
	"testing"
)

func TestNHA068PacketFramingRejectsOversizeAndMalformedLengthBeforePayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header []byte
		want   error
	}{
		{"oversize", []byte{0x30, 0x80, 0x80, 0x08}, ErrPacketTooLarge},
		{"malformed", []byte{0x30, 0xff, 0xff, 0xff, 0xff}, ErrMalformedPacketLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			written := make(chan struct{})
			go func() {
				for _, b := range tc.header {
					_, _ = right.Write([]byte{b})
				}
				close(written)
			}()
			buf := make([]byte, 32) // no packet body is sent
			n, err := (&packetConn{Conn: left}).Read(buf)
			if n != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("read = %d, %v", n, err)
			}
			<-written
		})
	}
}

func TestNHA068PacketFramingPreservesFragmentedAdjacentPackets(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	input := []byte{0xd0, 0, 0x30, 3, 'a', 'b', 'c'}
	go func() {
		for _, b := range input {
			_, _ = right.Write([]byte{b})
		}
	}()
	conn := &packetConn{Conn: left}
	got := make([]byte, len(input))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(input) {
		t.Fatalf("packet bytes changed: %x", got)
	}
}
