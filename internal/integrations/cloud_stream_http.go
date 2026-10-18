package integrations

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// The decorator is owned by this one opening. The underlying Transport is
// the exact client-owned carrier used by finite HTTP requests.
type cloudStreamRoundTripper struct {
	s       *CloudObservationStream
	entered atomic.Bool
}

func (t *cloudStreamRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	s := t.s
	c := s.client
	if !t.entered.CompareAndSwap(false, true) {
		return nil, &CloudError{kind: CloudInvalid}
	}
	if err := s.fence(); err != nil {
		return nil, err
	}
	if c.admission != nil {
		s.mu.Lock()
		deadline := s.openDeadline
		s.mu.Unlock()
		attempt, cancel := context.WithDeadline(s.ctx, deadline)
		retryAt, err := c.admission.ReserveAttempt(attempt)
		cancel()
		if failure := s.fence(); failure != nil {
			return nil, failure
		}
		if err != nil {
			return nil, &CloudError{kind: CloudQuotaStoreUnavailable}
		}
		if !retryAt.IsZero() {
			return nil, &CloudError{kind: CloudLocalQuotaExhausted, retryAt: retryAt}
		}
	}
	if err := s.fence(); err != nil {
		return nil, err
	}
	c.policy.mu.Lock()
	notBefore := c.scope.notBefore
	c.policy.mu.Unlock()
	if c.policy.clock.Now().Before(notBefore) {
		return nil, &CloudError{kind: CloudQuotaDeferred, retryAt: notBefore}
	}
	response, err := c.http.Transport.RoundTrip(request)
	// Own every returned body before status/handshake processing. In particular,
	// coder may still be diagnosing an invalid 101 when opening is cancelled.
	var rawBody io.ReadCloser
	var owned *cloudStreamBody
	if response != nil && response.Body != nil {
		rawBody = response.Body
		owned = &cloudStreamBody{ReadCloser: rawBody, s: s}
		response.Body = owned
		s.mu.Lock()
		s.body = owned
		s.mu.Unlock()
	}
	if err != nil {
		failure := s.fail(s.ioFailure(request.Context(), CloudTransportFailed))
		if owned != nil {
			_ = owned.Close()
		}
		return nil, failure
	}
	if owned == nil {
		return nil, &CloudError{kind: CloudTransportFailed}
	}
	reject := func(failure *CloudError) (*http.Response, error) {
		actual := s.fail(failure)
		_ = response.Body.Close()
		return nil, actual
	}
	if response.StatusCode == http.StatusTooManyRequests {
		now := c.policy.clock.Now()
		until, valid := parseCloudRetryAfter(response.Header.Get("Retry-After"), now)
		floor := now.Add(c.policy.limits.value.BackoffBase)
		if until.Before(floor) {
			until = floor
		}
		c.policy.deferScope(c.scope, until)
		return reject(&CloudError{kind: CloudQuotaDeferred, retryAt: until, retryable: valid})
	}
	if failure := s.fence(); failure != nil {
		_ = owned.Close()
		return nil, failure
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return reject(&CloudError{kind: CloudCredentialFailed})
	}
	switch s.kind {
	case CloudStreamWebSocket:
		if response.StatusCode != http.StatusSwitchingProtocols {
			return reject(&CloudError{kind: CloudHTTPFailed})
		}
		duplex, ok := rawBody.(io.ReadWriteCloser)
		if !ok {
			return reject(&CloudError{kind: CloudStreamProtocolFailed})
		}
		response.Body = &cloudStreamDuplex{cloudStreamBody: owned, writer: duplex}
	case CloudStreamSSE:
		if response.StatusCode != http.StatusOK {
			return reject(&CloudError{kind: CloudHTTPFailed})
		}
		media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || media != "text/event-stream" {
			return reject(&CloudError{kind: CloudStreamProtocolFailed})
		}
	default:
		return reject(&CloudError{kind: CloudInvalid})
	}
	return response, nil
}

type cloudStreamBody struct {
	io.ReadCloser
	s         *CloudObservationStream
	closeOnce sync.Once
	closeErr  error
}

func (b *cloudStreamBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.ReadCloser.Close() })
	return b.closeErr
}

func (b *cloudStreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.s.touch()
	}
	return n, err
}

type cloudStreamDuplex struct {
	*cloudStreamBody
	writer io.Writer
}

func (b *cloudStreamDuplex) Write(p []byte) (int, error) {
	if err := b.s.fence(); err != nil {
		return 0, err
	}
	return b.writer.Write(p)
}

func (s *CloudObservationStream) openTransport(request CloudStreamRequest) error {
	client := *s.client.http
	client.Transport = &cloudStreamRoundTripper{s: s}
	if s.kind == CloudStreamWebSocket {
		conn, _, err := websocket.Dial(s.ctx, request.request.url.String(), &websocket.DialOptions{HTTPClient: &client, HTTPHeader: request.request.header.Clone(), CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			// Never propagate a library/url.Error or its captured diagnostic body.
			var typed *CloudError
			if errors.As(err, &typed) {
				return typed
			}
			s.mu.Lock()
			failure := s.failure
			s.mu.Unlock()
			if failure != nil {
				return failure
			}
			if failure := s.fence(); failure != nil {
				return failure
			}
			return &CloudError{kind: CloudStreamProtocolFailed}
		}
		conn.SetReadLimit(s.limits.MaxReceiveBytes)
		s.mu.Lock()
		s.ws = conn
		s.mu.Unlock()
		return nil
	}
	var body io.Reader
	if len(request.request.body) > 0 {
		body = bytes.NewReader(request.request.body)
	}
	wire, err := http.NewRequestWithContext(s.ctx, request.request.method, request.request.url.String(), body)
	if err != nil {
		return &CloudError{kind: CloudInvalid}
	}
	wire.Header = request.request.header.Clone()
	wire.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(wire)
	if err != nil {
		var typed *CloudError
		if errors.As(err, &typed) {
			return typed
		}
		s.mu.Lock()
		failure := s.failure
		s.mu.Unlock()
		if failure != nil {
			return failure
		}
		if failure := s.fence(); failure != nil {
			return failure
		}
		return &CloudError{kind: CloudTransportFailed}
	}
	s.mu.Lock()
	s.body = response.Body
	s.sse = newCloudSSEReader(s, response.Body)
	s.mu.Unlock()
	return nil
}
