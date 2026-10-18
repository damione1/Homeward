package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

const maxControlFrame = 4096

// NewWorkerAdapter uses the shipped synthetic Python worker's local control
// socket. The executable and script are explicit caller choices. The control
// socket only requests simulator behavior; every reported state still crosses
// the production gRPC adapter and SessionHost.
func NewWorkerAdapter(executable, script, socketRoot string) (Adapter, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(script) || !filepath.IsAbs(socketRoot) {
		return nil, errors.New("worker conformance paths must be absolute")
	}
	for _, path := range []string{executable, script} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("worker conformance file %s unavailable: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("worker conformance file %s is not regular", path)
		}
	}
	info, err := os.Stat(socketRoot)
	if err != nil {
		return nil, fmt.Errorf("worker control directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("worker control directory is not a directory")
	}
	dir, err := os.MkdirTemp(socketRoot, "h95-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "c.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &workerAdapter{executable: executable, script: script, dir: dir, socket: path, listener: listener}, nil
}

type workerAdapter struct {
	mu         sync.Mutex
	executable string
	script     string
	dir        string
	socket     string
	listener   net.Listener
	conn       net.Conn
	reader     *bufio.Reader
	deliveries int
	closed     bool
}

type workerControlEvent struct {
	Event    string `json:"event"`
	ID       string `json:"id"`
	Stimulus string `json:"stimulus"`
	Code     string `json:"code"`
	Reason   string `json:"reason"`
	Rejected string `json:"rejected"`
}

type workerControlInstruction struct {
	Op       string `json:"op,omitempty"`
	ID       string `json:"id,omitempty"`
	Stimulus string `json:"stimulus,omitempty"`
}

func (w *workerAdapter) Spec(instance IntegrationInstanceID) (PilotSpec, error) {
	return NewWorkerSpec(instance, w.executable, []string{w.script, "--mode=conformance", "--control-socket", w.socket})
}

func (w *workerAdapter) Completion() CompletionSemantics { return CompletionTransportAccepted }

func (w *workerAdapter) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("worker control is closed")
	}
	old := w.conn
	w.conn, w.reader = nil, nil
	listener, ok := w.listener.(*net.UnixListener)
	w.mu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("close old worker control connection: %w", err)
		}
	}
	if !ok {
		return errors.New("worker control listener is not Unix")
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := listener.SetDeadline(deadline); err != nil {
			return err
		}
	}
	conn, err := listener.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		_ = conn.Close()
		return errors.New("worker control closed during accept")
	}
	w.conn, w.reader = conn, bufio.NewReaderSize(conn, maxControlFrame)
	w.mu.Unlock()
	event, err := w.read(ctx)
	if err != nil {
		return err
	}
	if event.Event != "ready" {
		return fmt.Errorf("worker first control event is %q", event.Event)
	}
	return nil
}

func (w *workerAdapter) read(ctx context.Context) (workerControlEvent, error) {
	w.mu.Lock()
	conn, reader := w.conn, w.reader
	w.mu.Unlock()
	if conn == nil || reader == nil {
		return workerControlEvent{}, errors.New("worker control is disconnected")
	}
	if err := ctx.Err(); err != nil {
		return workerControlEvent{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return workerControlEvent{}, err
		}
	}
	frame, err := reader.ReadSlice('\n')
	if err != nil {
		if ctx.Err() != nil {
			return workerControlEvent{}, ctx.Err()
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			return workerControlEvent{}, errors.New("worker control frame exceeds 4096 bytes")
		}
		return workerControlEvent{}, err
	}
	var event workerControlEvent
	if err := json.Unmarshal(frame, &event); err != nil {
		return workerControlEvent{}, err
	}
	if event.Event == "stream_error" || event.Event == "error" {
		return workerControlEvent{}, fmt.Errorf("worker control %s: %s %s", event.Event, event.Code, event.Reason)
	}
	return event, nil
}

func (w *workerAdapter) write(ctx context.Context, value workerControlInstruction) error {
	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn == nil {
		return errors.New("worker control is disconnected")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	frame, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(frame)+1 > maxControlFrame {
		return errors.New("worker control instruction exceeds 4096 bytes")
	}
	frame = append(frame, '\n')
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	for len(frame) > 0 {
		n, err := conn.Write(frame)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		frame = frame[n:]
	}
	return nil
}

func (w *workerAdapter) Delivered(ctx context.Context, want CommandID) error {
	text, err := want.Value()
	if err != nil {
		return err
	}
	event, err := w.read(ctx)
	if err != nil {
		return err
	}
	got, parseErr := ParseCommandID(event.ID)
	if event.Event != "delivered" || parseErr != nil || got != want {
		return fmt.Errorf("worker delivered event %+v, want %s", event, text)
	}
	w.mu.Lock()
	w.deliveries++
	w.mu.Unlock()
	return nil
}

func (w *workerAdapter) Apply(ctx context.Context, id CommandID, stimulus Stimulus) error {
	text, err := id.Value()
	if err != nil {
		return err
	}
	switch stimulus {
	case StimulusAcknowledge, StimulusUncorrelated, StimulusWrongCorrelation, StimulusStale, StimulusPartialLight, StimulusMatch:
	default:
		return errors.New("invalid worker stimulus")
	}
	if err := w.write(ctx, workerControlInstruction{ID: text, Stimulus: stimulus.String()}); err != nil {
		return err
	}
	event, err := w.read(ctx)
	if err != nil {
		return err
	}
	if event.Event != "applied" || event.ID != text || event.Stimulus != stimulus.String() {
		return fmt.Errorf("worker applied event %+v, want %s/%s", event, text, stimulus)
	}
	if stimulus == StimulusWrongCorrelation {
		if event.Rejected != "PERMISSION_DENIED" {
			return fmt.Errorf("foreign worker correlation was not denied: %+v", event)
		}
	} else if event.Rejected != "" {
		return fmt.Errorf("worker unexpectedly rejected %s: %+v", stimulus, event)
	}
	return nil
}

func (w *workerAdapter) Crash(ctx context.Context) error {
	if err := w.write(ctx, workerControlInstruction{Op: "crash"}); err != nil {
		return err
	}
	_, err := w.read(ctx)
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("worker crash did not close control channel: %v", err)
	}
	return nil
}

func (w *workerAdapter) Deliveries() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deliveries
}

func (w *workerAdapter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.closed = true
	conn, listener := w.conn, w.listener
	w.conn, w.reader, w.listener = nil, nil, nil
	w.mu.Unlock()
	var closeErr error
	if conn != nil {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return errors.Join(closeErr, os.RemoveAll(w.dir))
}
