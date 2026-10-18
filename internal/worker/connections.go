package worker

import (
	"net"
	"sync"
)

// h2c may hijack connections, so http.Server.Close alone cannot own them.
type trackedListener struct {
	net.Listener
	mu          sync.Mutex
	connections map[*trackedConn]struct{}
	closing     bool
	done        chan struct{}
}
type trackedConn struct {
	net.Conn
	owner *trackedListener
	once  sync.Once
}

func newTrackedListener(listener net.Listener) *trackedListener {
	return &trackedListener{Listener: listener, connections: make(map[*trackedConn]struct{}), done: make(chan struct{})}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{Conn: conn, owner: l}
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.connections[tracked] = struct{}{}
	l.mu.Unlock()
	return tracked, nil
}

func (c *trackedConn) Close() error {
	var result error
	c.once.Do(func() {
		result = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		if c.owner.closing && len(c.owner.connections) == 0 {
			close(c.owner.done)
		}
		c.owner.mu.Unlock()
	})
	return result
}

func (l *trackedListener) CloseAll() {
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return
	}
	l.closing = true
	connections := make([]*trackedConn, 0, len(l.connections))
	for conn := range l.connections {
		connections = append(connections, conn)
	}
	if len(connections) == 0 {
		close(l.done)
	}
	l.mu.Unlock()
	_ = l.Listener.Close()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (l *trackedListener) Done() <-chan struct{} { return l.done }
