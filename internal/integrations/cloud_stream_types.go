package integrations

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// CloudStreamLimitsInput describes transport bounds, not provider quotas.
type CloudStreamLimitsInput struct {
	MaxActiveStreams                                int
	MaxReceiveBytes, MaxSSELineBytes, MaxWriteBytes int64
	IdleTimeout, WriteTimeout, CloseTimeout         time.Duration
}

type CloudStreamLimits struct{ value CloudStreamLimitsInput }

func (l CloudStreamLimits) Input() CloudStreamLimitsInput { return l.value }
func (l CloudStreamLimits) Valid() bool                   { _, err := AdmitCloudStreamLimits(l.value); return err == nil }
func AdmitCloudStreamLimits(v CloudStreamLimitsInput) (CloudStreamLimits, error) {
	if v.MaxActiveStreams < 1 || v.MaxActiveStreams > 32 || v.MaxReceiveBytes < 1 || v.MaxReceiveBytes > 4<<20 ||
		v.MaxSSELineBytes < 1 || v.MaxSSELineBytes > 1<<20 || v.MaxSSELineBytes > v.MaxReceiveBytes ||
		v.MaxWriteBytes < 1 || v.MaxWriteBytes > 1<<20 || v.IdleTimeout < time.Millisecond || v.IdleTimeout > 30*time.Minute ||
		v.WriteTimeout < time.Millisecond || v.WriteTimeout > 30*time.Second || v.CloseTimeout < time.Millisecond || v.CloseTimeout > 30*time.Second {
		return CloudStreamLimits{}, &CloudError{kind: CloudInvalid}
	}
	return CloudStreamLimits{value: v}, nil
}
func DefaultCloudStreamLimits() CloudStreamLimits {
	limits, _ := AdmitCloudStreamLimits(CloudStreamLimitsInput{MaxActiveStreams: 2, MaxReceiveBytes: 1 << 20, MaxSSELineBytes: 64 << 10, MaxWriteBytes: 64 << 10, IdleTimeout: 180 * time.Second, WriteTimeout: 5 * time.Second, CloseTimeout: 5 * time.Second})
	return limits
}

type CloudStreamKind uint8

const (
	CloudStreamUnspecified CloudStreamKind = iota
	CloudStreamSSE
	CloudStreamWebSocket
)

// CloudStreamRequest contains no authority to retry or follow redirects.
type CloudStreamRequest struct {
	kind    CloudStreamKind
	request CloudRequest
}

func NewCloudStreamRequest(kind CloudStreamKind, method, rawURL string, header http.Header, body []byte) (CloudStreamRequest, error) {
	switch kind {
	case CloudStreamSSE:
		if method != http.MethodGet && method != http.MethodPost {
			return CloudStreamRequest{}, &CloudError{kind: CloudInvalid}
		}
	case CloudStreamWebSocket:
		if method != http.MethodGet || len(body) != 0 {
			return CloudStreamRequest{}, &CloudError{kind: CloudInvalid}
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed == nil {
			return CloudStreamRequest{}, &CloudError{kind: CloudInvalid}
		}
		switch parsed.Scheme {
		case "ws":
			parsed.Scheme = "http"
		case "wss":
			parsed.Scheme = "https"
		default:
			return CloudStreamRequest{}, &CloudError{kind: CloudInvalid}
		}
		rawURL = parsed.String()
	default:
		return CloudStreamRequest{}, &CloudError{kind: CloudInvalid}
	}
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "host" || lower == "connection" || lower == "upgrade" || strings.HasPrefix(lower, "sec-websocket-") {
			return CloudStreamRequest{}, &CloudError{kind: CloudInvalid}
		}
	}
	request, err := NewCloudRequest(method, rawURL, header, body)
	if err != nil {
		return CloudStreamRequest{}, err
	}
	return CloudStreamRequest{kind: kind, request: request}, nil
}
func (r CloudStreamRequest) String() string               { return "[REDACTED CLOUD STREAM REQUEST]" }
func (r CloudStreamRequest) GoString() string             { return r.String() }
func (r CloudStreamRequest) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

type CloudStreamMessageKind uint8

const (
	CloudStreamMessageUnspecified CloudStreamMessageKind = iota
	CloudStreamSSEEvent
	CloudStreamWebSocketText
	CloudStreamWebSocketBinary
)

type CloudWebSocketMessage struct {
	kind CloudStreamMessageKind
	data []byte
}

func NewCloudWebSocketMessage(kind CloudStreamMessageKind, data []byte) (CloudWebSocketMessage, error) {
	if len(data) > 1<<20 {
		return CloudWebSocketMessage{}, &CloudError{kind: CloudInvalid}
	}
	switch kind {
	case CloudStreamWebSocketText:
		if !utf8.Valid(data) {
			return CloudWebSocketMessage{}, &CloudError{kind: CloudInvalid}
		}
	case CloudStreamWebSocketBinary:
	default:
		return CloudWebSocketMessage{}, &CloudError{kind: CloudInvalid}
	}
	return CloudWebSocketMessage{kind: kind, data: append([]byte(nil), data...)}, nil
}
func (m CloudWebSocketMessage) String() string               { return "[REDACTED CLOUD STREAM WRITE]" }
func (m CloudWebSocketMessage) GoString() string             { return m.String() }
func (m CloudWebSocketMessage) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

// CloudStreamMessage is an owned frame and an unforgeable health receipt.
// Its payload is untrusted provider data, not a Host-admitted state.
type CloudStreamMessage struct {
	kind          CloudStreamMessageKind
	data          []byte
	eventType, id string
	hasID         bool
	owner         *CloudObservationStream
	healthEpoch   uint64
}

func (m CloudStreamMessage) Kind() (CloudStreamMessageKind, error) {
	if m.owner == nil || m.kind < CloudStreamSSEEvent || m.kind > CloudStreamWebSocketBinary {
		return CloudStreamMessageUnspecified, &CloudError{kind: CloudInvalid}
	}
	return m.kind, nil
}
func (m CloudStreamMessage) Bytes() ([]byte, error) {
	if _, err := m.Kind(); err != nil {
		return nil, err
	}
	return append([]byte(nil), m.data...), nil
}
func (m CloudStreamMessage) SSEFields() (eventType, id string, idPresent bool, err error) {
	if _, err = m.Kind(); err != nil || m.kind != CloudStreamSSEEvent {
		return "", "", false, &CloudError{kind: CloudInvalid}
	}
	return m.eventType, m.id, m.hasID, nil
}
func (m CloudStreamMessage) String() string               { return "[REDACTED CLOUD STREAM MESSAGE]" }
func (m CloudStreamMessage) GoString() string             { return m.String() }
func (m CloudStreamMessage) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }
