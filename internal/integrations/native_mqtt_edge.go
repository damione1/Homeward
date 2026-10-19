package integrations

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtttopic"
)

var ErrInvalidNativeMQTTEdge = errors.New("invalid native MQTT edge")
var ErrNativeMQTTEdgeUnavailable = errors.New("native MQTT edge admission is unavailable")

// NativeMQTTEdge is an immutable, admitted publication from the in-process
// MQTT pilot. The Host still fences the pilot instance and live session before
// passing this value to the sequencer. It is unavailable to external workers.
type NativeMQTTEdge struct {
	topic     string
	payload   []byte
	sourceSeq uint64
	valid     bool
}

func NewNativeMQTTEdge(topic string, payload []byte, retained bool, sourceSeq uint64) (NativeMQTTEdge, error) {
	if retained || sourceSeq == 0 || !mqtttopic.ValidName(topic) || len(payload) > 64<<10 {
		return NativeMQTTEdge{}, ErrInvalidNativeMQTTEdge
	}
	return NativeMQTTEdge{topic: topic, payload: append([]byte{}, payload...), sourceSeq: sourceSeq, valid: true}, nil
}

func (e NativeMQTTEdge) Components() (topic string, payload []byte, sourceSeq uint64, err error) {
	if !e.valid || e.sourceSeq == 0 || !mqtttopic.ValidName(e.topic) || len(e.payload) > 64<<10 {
		return "", nil, 0, ErrInvalidNativeMQTTEdge
	}
	return e.topic, append([]byte{}, e.payload...), e.sourceSeq, nil
}

// Clone gives journal readers a separate payload without exposing the owned
// backing slice. Sizes validates the value without allocating another copy.
func (e NativeMQTTEdge) Clone() (NativeMQTTEdge, error) {
	if _, _, _, err := e.Sizes(); err != nil {
		return NativeMQTTEdge{}, err
	}
	e.payload = append([]byte{}, e.payload...)
	return e, nil
}

func (e NativeMQTTEdge) Sizes() (topicBytes, payloadBytes int, sourceSeq uint64, err error) {
	if !e.valid || e.sourceSeq == 0 || !mqtttopic.ValidName(e.topic) || len(e.payload) > 64<<10 {
		return 0, 0, 0, ErrInvalidNativeMQTTEdge
	}
	return len(e.topic), len(e.payload), e.sourceSeq, nil
}

// NativeMQTTEdgePublisher is probed on the native SessionHost only. It is not
// part of the versioned external worker contract.
type NativeMQTTEdgePublisher interface {
	PublishNativeMQTTEdge(context.Context, NativeMQTTEdge) error
}

// NativeMQTTEdgeSink is implemented by the one sequencer admitted by Run.
type NativeMQTTEdgeSink interface {
	AcceptNativeMQTTEdge(context.Context, domain.IntegrationInstanceID, domain.SourceStamp, NativeMQTTEdge) error
}

// EnableNativeMQTTEdges is trusted composition, before Supervisor.Start. It
// grants only one configured in-process MQTT instance this private ingress.
func (h *Manager) EnableNativeMQTTEdges(instance domain.IntegrationInstanceID) error {
	if h == nil || h.active == nil {
		return ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.composition != compositionOpen || h.admission != admissionOpen || h.nativeMQTTSink != nil {
		return ErrSupervisorState
	}
	sink, ok := h.sink.(NativeMQTTEdgeSink)
	if !ok || nilInterface(sink) {
		return ErrNativeMQTTEdgeUnavailable
	}
	h.nativeMQTTInstance, h.nativeMQTTSink = instance, sink
	return nil
}

// PublishNativeMQTTEdge is absent from SessionHost and from the external
// worker protocol. The Host lock linearizes it with session replacement,
// admission closure, and every other accepted report.
func (s *Session) PublishNativeMQTTEdge(ctx context.Context, edge NativeMQTTEdge) error {
	if s == nil || s.host == nil {
		return ErrInactiveSession
	}
	h := s.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.live() {
		return ErrInactiveSession
	}
	if h.admission != admissionOpen || h.nativeMQTTSink == nil || s.instance != h.nativeMQTTInstance {
		return ErrNativeMQTTEdgeUnavailable
	}
	if nilInterface(ctx) {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, sequence, err := edge.Sizes()
	if err != nil {
		return err
	}
	source, err := domain.NewSourceStamp(s.id, &sequence)
	if err != nil {
		return err
	}
	return h.nativeMQTTSink.AcceptNativeMQTTEdge(ctx, s.instance, source, edge)
}
