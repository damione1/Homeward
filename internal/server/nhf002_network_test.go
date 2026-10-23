package server

import (
	"net"
	"sync"
	"sync/atomic"
)

// A circuit around this fixture's own TCP connections. It leaves the real
// server and listener alive, while allowing a genuine transport interruption.
type nhf002Network struct {
	net.Listener
	mu          sync.Mutex
	connections map[*nhf002Connection]struct{}
	outage      atomic.Bool
	rejected    atomic.Uint64
}

type nhf002Connection struct {
	net.Conn
	owner *nhf002Network
	once  sync.Once
}

func (n *nhf002Network) Accept() (net.Conn, error) {
	for {
		connection, err := n.Listener.Accept()
		if err != nil {
			return nil, err
		}
		n.mu.Lock()
		if n.outage.Load() {
			n.mu.Unlock()
			_ = connection.Close()
			n.rejected.Add(1)
			continue
		}
		wrapped := &nhf002Connection{Conn: connection, owner: n}
		n.connections[wrapped] = struct{}{}
		n.mu.Unlock()
		return wrapped, nil
	}
}

func (n *nhf002Network) cut() {
	n.mu.Lock()
	n.outage.Store(true)
	connections := make([]*nhf002Connection, 0, len(n.connections))
	for connection := range n.connections {
		connections = append(connections, connection)
	}
	n.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (c *nhf002Connection) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		c.owner.mu.Unlock()
	})
	return err
}
