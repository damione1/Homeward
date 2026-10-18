package mqtt

import (
	"errors"
	"io"
	"net"
)

const maxWirePacket = 128 * 1024

var ErrPacketTooLarge = errors.New("mqtt: inbound packet exceeds wire limit")
var ErrMalformedPacketLength = errors.New("mqtt: malformed inbound packet length")

// packetConn frames reads before Paho sees them. The MQTT Remaining Length is
// inspected with at most five stack bytes; a dishonest broker cannot make the
// decoder allocate an advertised giant packet before the limit is enforced.
// Paho has one connection reader. packets.NewThreadSafeConn wraps writes above
// this layer, including TLS connections.
type packetConn struct {
	net.Conn
	header    [5]byte
	headerLen int
	headerPos int
	remaining uint32
}

func (c *packetConn) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if c.headerPos < c.headerLen {
		n := copy(dst, c.header[c.headerPos:c.headerLen])
		c.headerPos += n
		return n, nil
	}
	if c.remaining > 0 {
		limit := len(dst)
		if uint32(limit) > c.remaining {
			limit = int(c.remaining)
		}
		n, err := c.Conn.Read(dst[:limit])
		c.remaining -= uint32(n)
		return n, err
	}
	if _, err := io.ReadFull(c.Conn, c.header[:1]); err != nil {
		return 0, err
	}
	c.headerLen = 1
	c.headerPos = 0
	var multiplier uint32 = 1
	var length uint32
	terminated := false
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(c.Conn, c.header[c.headerLen:c.headerLen+1]); err != nil {
			return 0, err
		}
		b := c.header[c.headerLen]
		c.headerLen++
		length += uint32(b&0x7f) * multiplier
		if b&0x80 == 0 {
			terminated = true
			break
		}
		multiplier *= 128
	}
	if !terminated {
		_ = c.Conn.Close()
		return 0, ErrMalformedPacketLength
	}
	if length+uint32(c.headerLen) > maxWirePacket {
		_ = c.Conn.Close()
		return 0, ErrPacketTooLarge
	}
	c.remaining = length
	return c.Read(dst)
}
