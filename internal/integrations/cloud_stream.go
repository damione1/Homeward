package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// NewCloudPolicyWithStreams enables streams only with explicit admitted limits.
// Existing NewCloudPolicy callers retain their finite-response-only policy.
func NewCloudPolicyWithStreams(limits CloudLimits, streams CloudStreamLimits, bindings []CloudAccountBinding, clock CloudClock, timer CloudTimer, jitter Jitter) (*CloudPolicy, error) {
	if !streams.Valid() {
		return nil, &CloudError{kind: CloudInvalid}
	}
	policy, err := NewCloudPolicy(limits, bindings, clock, timer, jitter)
	if err != nil {
		return nil, err
	}
	policy.streamLimits = streams
	return policy, nil
}

type cloudStreamPhase uint8

const (
	cloudStreamOpening cloudStreamPhase = iota
	cloudStreamActive
	cloudStreamClosing
)

// CloudObservationStream owns its network resources until Done closes. The
// shared policy retains its slot until all owned operations have drained.
type CloudObservationStream struct {
	client             *CloudClient
	kind               CloudStreamKind
	limits             CloudStreamLimitsInput
	ctx                context.Context
	parent             context.Context
	cancel             context.CancelFunc
	mu                 sync.Mutex
	phase              cloudStreamPhase
	failure            *CloudError
	body               io.ReadCloser
	ws                 *websocket.Conn
	sse                *cloudSSEReader
	openDeadline       time.Time
	lastActivity       time.Time
	receiving, writing bool
	operations         sync.WaitGroup
	openDone           chan struct{}
	done               chan struct{}
	touched            chan struct{}
}

func (s *CloudObservationStream) String() string               { return "[REDACTED CLOUD OBSERVATION STREAM]" }
func (s *CloudObservationStream) GoString() string             { return s.String() }
func (s *CloudObservationStream) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func (s *CloudObservationStream) Done() <-chan struct{} {
	if s == nil || s.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

func (c *CloudClient) reserveStream(s *CloudObservationStream) error {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.streamsClosing || c.streamsClosed == nil {
		return &CloudError{kind: CloudStreamClosed}
	}
	c.policy.mu.Lock()
	defer c.policy.mu.Unlock()
	if !c.policy.streamLimits.Valid() {
		return &CloudError{kind: CloudStreamsDisabled}
	}
	if len(c.scope.streams) >= c.policy.streamLimits.value.MaxActiveStreams {
		return &CloudError{kind: CloudStreamCapacity}
	}
	if c.scope.streams == nil {
		c.scope.streams = make(map[*CloudObservationStream]struct{})
	}
	c.scope.streams[s] = struct{}{}
	c.streamWG.Add(1)
	return nil
}
func (c *CloudClient) closeObservationStreams() {
	c.streamMu.Lock()
	if !c.streamsClosing {
		c.streamsClosing = true
		if c.streamsClosed != nil {
			close(c.streamsClosed)
		}
	}
	c.streamMu.Unlock()
	c.streamWG.Wait()
}

// OpenObservationStream sends once. Provider resubscription and reconnection
// are deliberately outside this transport. ctx governs the returned stream.
func (c *CloudClient) OpenObservationStream(ctx context.Context, request CloudStreamRequest) (stream *CloudObservationStream, err error) {
	if !c.valid() || nilInterface(ctx) || !c.requestAllowed(request.request) || (request.kind != CloudStreamSSE && request.kind != CloudStreamWebSocket) {
		return nil, &CloudError{kind: CloudInvalid}
	}
	if !c.live() {
		return nil, &CloudError{kind: CloudSessionInactive}
	}
	lifetime, cancel := context.WithCancel(ctx)
	s := &CloudObservationStream{client: c, kind: request.kind, limits: c.policy.streamLimits.value, ctx: lifetime, parent: ctx, cancel: cancel, openDone: make(chan struct{}), done: make(chan struct{}), touched: make(chan struct{}, 1)}
	if err = c.reserveStream(s); err != nil {
		cancel()
		return nil, err
	}
	s.openDeadline = time.Now().Add(c.policy.limits.value.TotalTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(s.openDeadline) {
		s.openDeadline = deadline
	}
	stopTotalTimer := s.openingTimer(s.openDeadline)
	go s.watch()
	defer func() {
		stopTotalTimer()
		if err != nil {
			failure, ok := err.(*CloudError)
			if !ok {
				failure = &CloudError{kind: CloudTransportFailed}
			}
			err = s.fail(failure)
		}
		// A failed opening must admit its terminal cause before the watcher
		// may complete drain/Done. Success already admitted the active phase.
		close(s.openDone)
		if err != nil {
			_ = s.Close(context.Background())
		}
	}()
	release, acquireErr := c.policy.acquire(s.ctx, c.session.Done(), c.scope)
	if acquireErr != nil {
		return nil, acquireErr
	}
	defer release()
	s.mu.Lock()
	attemptDeadline := time.Now().Add(c.policy.limits.value.AttemptTimeout)
	if attemptDeadline.Before(s.openDeadline) {
		s.openDeadline = attemptDeadline
	}
	deadline := s.openDeadline
	s.mu.Unlock()
	stopAttemptTimer := s.openingTimer(deadline)
	defer stopAttemptTimer()
	if err = s.openTransport(request); err != nil {
		return nil, err
	}
	if err = s.fence(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.phase != cloudStreamOpening || s.ctx.Err() != nil || !time.Now().Before(s.openDeadline) {
		s.mu.Unlock()
		return nil, &CloudError{kind: CloudBudgetExhausted}
	}
	s.lastActivity = c.policy.clock.Now()
	s.phase = cloudStreamActive
	s.mu.Unlock()
	return s, nil
}

// openDone cannot close until every opening timer callback has returned.
// Stop alone does not join an AfterFunc callback that has already started.
func (s *CloudObservationStream) openingTimer(deadline time.Time) func() {
	joined := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() {
		defer close(joined)
		s.expireOpen()
	})
	return func() {
		if !timer.Stop() {
			<-joined
		}
	}
}
func (s *CloudObservationStream) expireOpen() {
	s.terminate(&CloudError{kind: CloudBudgetExhausted}, true)
}
func (s *CloudObservationStream) fail(failure *CloudError) *CloudError {
	return s.terminate(failure, false)
}
func (s *CloudObservationStream) terminate(failure *CloudError, onlyOpening bool) *CloudError {
	// Receipt admission and the first terminal cause share one epoch lock.
	// Drain may block independently, but cannot preserve a receipt from another
	// stream or later invalidate an observation admitted during recovery.
	s.client.healthMu.Lock()
	s.mu.Lock()
	if onlyOpening && s.phase != cloudStreamOpening {
		actual := s.failure
		s.mu.Unlock()
		s.client.healthMu.Unlock()
		return actual
	}
	first := s.failure == nil
	if first {
		s.failure = failure
	}
	actual := s.failure
	s.phase = cloudStreamClosing
	s.mu.Unlock()
	s.cancel()
	if first && failure.kind != CloudStreamClosed && failure.kind != CloudStreamCanceled {
		s.client.degradeLocked(failure.kind)
	}
	s.client.healthMu.Unlock()
	return actual
}

// Preserve the origin of cancellation before closing the socket can turn it
// into an undifferentiated transport error in a concurrent read or write.
func (s *CloudObservationStream) ioFailure(ctx context.Context, fallback CloudFailureKind) *CloudError {
	if !s.client.live() {
		return &CloudError{kind: CloudSessionInactive}
	}
	s.mu.Lock()
	failure := s.failure
	s.mu.Unlock()
	if failure != nil {
		return failure
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(s.parent.Err(), context.DeadlineExceeded) {
			return &CloudError{kind: CloudBudgetExhausted}
		}
		return &CloudError{kind: CloudStreamCanceled}
	}
	return &CloudError{kind: fallback}
}
func (s *CloudObservationStream) fence() error {
	if s == nil || s.client == nil || s.ctx == nil {
		return &CloudError{kind: CloudInvalid}
	}
	if !s.client.live() {
		return &CloudError{kind: CloudSessionInactive}
	}
	select {
	case <-s.client.streamsClosed:
		return &CloudError{kind: CloudStreamClosed}
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	if s.phase == cloudStreamClosing {
		return &CloudError{kind: CloudStreamClosed}
	}
	if s.ctx.Err() != nil {
		if errors.Is(s.parent.Err(), context.DeadlineExceeded) {
			return &CloudError{kind: CloudBudgetExhausted}
		}
		return &CloudError{kind: CloudStreamCanceled}
	}
	return nil
}
func (s *CloudObservationStream) touch() {
	s.mu.Lock()
	s.lastActivity = s.client.policy.clock.Now()
	s.mu.Unlock()
	select {
	case s.touched <- struct{}{}:
	default:
	}
}
func (s *CloudObservationStream) watch() {
	select {
	case <-s.ctx.Done():
		s.fail(s.ioFailure(s.ctx, CloudStreamCanceled))
	case <-s.client.session.Done():
		s.fail(&CloudError{kind: CloudSessionInactive})
	case <-s.client.streamsClosed:
		s.fail(&CloudError{kind: CloudStreamClosed})
	case <-s.openDone:
		s.watchIdle()
	}
	s.cancel()
	// Closing the available carrier must precede waiting for a Dial/Do still
	// reading its handshake. A successful late return is closed again below.
	s.closeWire()
	<-s.openDone
	s.mu.Lock()
	s.phase = cloudStreamClosing
	s.mu.Unlock()
	s.closeWire()
	s.operations.Wait()
	s.client.policy.mu.Lock()
	delete(s.client.scope.streams, s)
	s.client.policy.mu.Unlock()
	close(s.done)
	s.client.streamWG.Done()
}

func (s *CloudObservationStream) closeWire() {
	s.mu.Lock()
	body, ws := s.body, s.ws
	s.mu.Unlock()
	if ws != nil {
		_ = ws.CloseNow()
	} else if body != nil {
		_ = body.Close()
	}
}
func (s *CloudObservationStream) watchIdle() {
	for {
		s.mu.Lock()
		phase := s.phase
		remaining := s.limits.IdleTimeout - s.client.policy.clock.Now().Sub(s.lastActivity)
		s.mu.Unlock()
		if phase != cloudStreamActive {
			return
		}
		if remaining <= 0 {
			s.fail(&CloudError{kind: CloudStreamIdle})
			return
		}
		timer := s.client.policy.timer.NewTimer(remaining)
		if nilInterface(timer) || timer.C() == nil {
			s.fail(&CloudError{kind: CloudInvalid})
			return
		}
		select {
		case <-s.ctx.Done():
			timer.Stop()
			s.fail(s.ioFailure(s.ctx, CloudStreamCanceled))
			return
		case <-s.client.session.Done():
			timer.Stop()
			s.fail(&CloudError{kind: CloudSessionInactive})
			return
		case <-s.client.streamsClosed:
			timer.Stop()
			s.fail(&CloudError{kind: CloudStreamClosed})
			return
		case <-s.touched:
		case <-timer.C():
		}
		timer.Stop()
	}
}

// Close does not return success until the stream's owned tasks are joined.
// A caller deadline never releases a slot whose cleanup is still running.
func (s *CloudObservationStream) Close(ctx context.Context) error {
	if s == nil || s.done == nil || nilInterface(ctx) {
		return &CloudError{kind: CloudInvalid}
	}
	s.fail(&CloudError{kind: CloudStreamClosed})
	bounded, cancel := context.WithTimeout(ctx, s.limits.CloseTimeout)
	defer cancel()
	select {
	case <-s.done:
		return nil
	case <-bounded.Done():
		return &CloudError{kind: CloudBudgetExhausted}
	}
}
func (s *CloudObservationStream) beginOperation(write bool) error {
	if err := s.fence(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != cloudStreamActive {
		return &CloudError{kind: CloudStreamClosed}
	}
	if write {
		if s.writing {
			return &CloudError{kind: CloudStreamBusy}
		}
		s.writing = true
	} else {
		if s.receiving {
			return &CloudError{kind: CloudStreamBusy}
		}
		s.receiving = true
	}
	s.operations.Add(1)
	return nil
}
func (s *CloudObservationStream) endOperation(write bool) {
	s.mu.Lock()
	if write {
		s.writing = false
	} else {
		s.receiving = false
	}
	s.mu.Unlock()
	s.operations.Done()
}
func (s *CloudObservationStream) operationContext(ctx context.Context, timeout time.Duration) (context.Context, func()) {
	joined, cancel := context.WithCancel(s.ctx)
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { cancel(); close(stopped) })
	var timeoutCancel context.CancelFunc = func() {}
	if timeout > 0 {
		joined, timeoutCancel = context.WithTimeout(joined, timeout)
	}
	return joined, func() {
		if !stop() {
			<-stopped
		}
		timeoutCancel()
		cancel()
	}
}

func (s *CloudObservationStream) Receive(ctx context.Context) (CloudStreamMessage, error) {
	if nilInterface(ctx) {
		return CloudStreamMessage{}, &CloudError{kind: CloudInvalid}
	}
	if ctx.Err() != nil {
		return CloudStreamMessage{}, &CloudError{kind: CloudBudgetExhausted}
	}
	if err := s.beginOperation(false); err != nil {
		return CloudStreamMessage{}, err
	}
	defer s.endOperation(false)
	operation, done := s.operationContext(ctx, 0)
	defer done()
	s.client.healthMu.Lock()
	epoch := s.client.healthEpoch
	s.client.healthMu.Unlock()
	var message CloudStreamMessage
	var err error
	if s.kind == CloudStreamSSE {
		message, err = s.sse.receive(operation)
	} else {
		typ, reader, readErr := s.ws.Reader(operation)
		if readErr != nil {
			kind := CloudTransportFailed
			if errors.Is(readErr, websocket.ErrMessageTooBig) {
				kind = CloudResponseTooLarge
			}
			err = s.ioFailure(operation, kind)
		} else {
			payload, readErr := io.ReadAll(io.LimitReader(reader, s.limits.MaxReceiveBytes+1))
			if int64(len(payload)) > s.limits.MaxReceiveBytes || errors.Is(readErr, websocket.ErrMessageTooBig) {
				err = &CloudError{kind: CloudResponseTooLarge}
			} else if readErr != nil {
				err = s.ioFailure(operation, CloudTransportFailed)
			} else {
				switch typ {
				case websocket.MessageText:
					if !utf8.Valid(payload) {
						err = &CloudError{kind: CloudStreamProtocolFailed}
					}
					message.kind = CloudStreamWebSocketText
				case websocket.MessageBinary:
					message.kind = CloudStreamWebSocketBinary
				default:
					err = &CloudError{kind: CloudStreamProtocolFailed}
				}
				message.data = payload
			}
		}
	}
	if err != nil {
		failure, ok := err.(*CloudError)
		if !ok {
			failure = &CloudError{kind: CloudTransportFailed}
		}
		if operation.Err() != nil {
			failure = s.ioFailure(operation, failure.kind)
		}
		return CloudStreamMessage{}, s.fail(failure)
	}
	if err = s.fence(); err != nil {
		return CloudStreamMessage{}, err
	}
	message.owner = s
	message.healthEpoch = epoch
	return message, nil
}
func (s *CloudObservationStream) Write(ctx context.Context, message CloudWebSocketMessage) error {
	if s == nil || nilInterface(ctx) || s.kind != CloudStreamWebSocket || (message.kind != CloudStreamWebSocketText && message.kind != CloudStreamWebSocketBinary) {
		return &CloudError{kind: CloudInvalid}
	}
	if ctx.Err() != nil {
		return &CloudError{kind: CloudBudgetExhausted}
	}
	if int64(len(message.data)) > s.limits.MaxWriteBytes {
		return &CloudError{kind: CloudResponseTooLarge}
	}
	if err := s.beginOperation(true); err != nil {
		return err
	}
	defer s.endOperation(true)
	operation, done := s.operationContext(ctx, s.limits.WriteTimeout)
	defer done()
	if err := s.fence(); err != nil {
		return err
	}
	typ := websocket.MessageText
	if message.kind == CloudStreamWebSocketBinary {
		typ = websocket.MessageBinary
	}
	if err := s.ws.Write(operation, typ, message.data); err != nil {
		return s.fail(s.ioFailure(operation, CloudTransportFailed))
	}
	return s.fence()
}

// ConfirmStreamObservationAdmitted is called only after Host accepted the
// decoded observation. Control traffic and an open connection do not qualify.
func (c *CloudClient) ConfirmStreamObservationAdmitted(ctx context.Context, message CloudStreamMessage) error {
	if !c.valid() || nilInterface(ctx) || message.owner == nil || message.owner.client != c {
		return &CloudError{kind: CloudInvalid}
	}
	if _, err := message.Kind(); err != nil {
		return err
	}
	if err := message.owner.fence(); err != nil {
		return err
	}
	return c.confirmObservationEpoch(ctx, message.healthEpoch)
}
