package observability

import (
	"io"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

// This writer never forwards or retains the transport's bytes. It owns no
// output stream or goroutine; the only retained value is a closed safe event.
type httpDiagnosticWriter struct {
	journal   *Journal
	component obs.LogComponent
}

func transportDiagnosticComponent(component obs.LogComponent) bool {
	switch component {
	case obs.LogComponent_LOG_COMPONENT_PUBLIC_TRANSPORT, obs.LogComponent_LOG_COMPONENT_OPERATOR_TRANSPORT, obs.LogComponent_LOG_COMPONENT_WORKER_TRANSPORT:
		return true
	default:
		return false
	}
}

func NewHTTPDiagnosticWriter(journal *Journal, component obs.LogComponent) (io.Writer, error) {
	if !journal.valid() || !transportDiagnosticComponent(component) {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CODE)
	}
	return &httpDiagnosticWriter{journal: journal, component: component}, nil
}

func (w *httpDiagnosticWriter) Write(data []byte) (int, error) {
	if w == nil || !w.journal.valid() || !transportDiagnosticComponent(w.component) {
		return 0, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CODE)
	}
	if len(data) == 0 {
		return 0, nil
	}
	event, err := NewEvent(obs.LogCode_LOG_CODE_TRANSPORT_INTERNAL_DIAGNOSTIC, obs.LogLevel_LOG_LEVEL_WARNING, w.component, time.Now().UTC(), NewProcessAuthor())
	if err == nil {
		_ = w.journal.Emit(event)
	}
	// A saturated journal never changes the transport's control flow or sends
	// the rejected input to another logger.
	return len(data), nil
}
