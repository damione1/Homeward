package mqtt

import (
	"context"
	"io"
	"log/slog"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
)

// DiscoveryDiagnostic is the existing discovery owner's domain. The protobuf
// remains the sole authority for published log codes; every conversion is
// explicit and unknown or zero diagnostics have no published representation.
func discoveryLogCode(code DiscoveryDiagnostic) (obs.LogCode, error) {
	switch code {
	case DiagnosticUnknownDiscovery:
		return obs.LogCode_LOG_CODE_MQTT_UNKNOWN_DISCOVERY, nil
	case DiagnosticInvalidDiscovery:
		return obs.LogCode_LOG_CODE_MQTT_INVALID_DISCOVERY, nil
	case DiagnosticUnconfiguredSource:
		return obs.LogCode_LOG_CODE_MQTT_UNCONFIGURED_SOURCE, nil
	case DiagnosticUnsupportedEffect:
		return obs.LogCode_LOG_CODE_MQTT_UNSUPPORTED_EFFECT, nil
	case DiagnosticUnsupportedColorMode:
		return obs.LogCode_LOG_CODE_MQTT_UNSUPPORTED_COLOR_MODE, nil
	case DiagnosticInvalidState:
		return obs.LogCode_LOG_CODE_MQTT_INVALID_STATE, nil
	case DiagnosticAvailability:
		return obs.LogCode_LOG_CODE_MQTT_INVALID_AVAILABILITY, nil
	case DiagnosticRetirement:
		return obs.LogCode_LOG_CODE_MQTT_RETIREMENT_FAILED, nil
	case DiagnosticSnapshotOverflow:
		return obs.LogCode_LOG_CODE_MQTT_SNAPSHOT_OVERFLOW, nil
	case DiagnosticUnconfiguredActionAnchor:
		return obs.LogCode_LOG_CODE_MQTT_UNCONFIGURED_ACTION_ANCHOR, nil
	case DiagnosticAmbiguousActionDevice:
		return obs.LogCode_LOG_CODE_MQTT_AMBIGUOUS_ACTION_DEVICE, nil
	case DiagnosticActionOverflow:
		return obs.LogCode_LOG_CODE_MQTT_ACTION_OVERFLOW, nil
	case DiagnosticInvalidAction:
		return obs.LogCode_LOG_CODE_MQTT_INVALID_ACTION, nil
	default:
		return obs.LogCode_LOG_CODE_UNSPECIFIED, ErrInvalidSettings
	}
}

func (m *DiscoveryMapper) emitDiscoveryDiagnostic(code obs.LogCode) {
	if m.journal == nil {
		return
	}
	event, err := observability.NewEvent(code, obs.LogLevel_LOG_LEVEL_WARNING, obs.LogComponent_LOG_COMPONENT_MQTT, time.Now().UTC(), observability.NewProcessAuthor())
	if err == nil {
		_ = m.journal.Emit(event)
	}
}

func discoveryFactoryWithJournal(factory integrations.PilotFactory, journal *observability.Journal) (integrations.PilotFactory, error) {
	if factory == nil || journal == nil {
		return nil, ErrInvalidSettings
	}
	if _, err := journal.Snapshot(); err != nil {
		return nil, ErrInvalidSettings
	}
	// A journal-enabled session publishes only into its bounded live journal.
	// The legacy slog path is still expurgated at the real diagnose call site.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return func() integrations.Pilot {
		p, ok := factory().(*discoveryPilot)
		if !ok || p == nil || p.Pilot == nil || p.mapper == nil {
			return nil
		}
		p.mapper.journal, p.mapper.logger = journal, logger
		p.Pilot.observation, _ = journal.Cells()
		return p
	}, nil
}

// The admitted existing factories retain ownership of discovery, subscriptions,
// trigger routing and per-session mapper creation. These wrappers only attach
// the journal to that exact fresh mapper before it can be started.
func NewDiscoveryPilotFactoryWithJournal(settings Settings, repository discoveryRegistry, journal *observability.Journal) (integrations.PilotFactory, error) {
	factory, err := NewDiscoveryPilotFactory(settings, repository)
	if err != nil {
		return nil, err
	}
	return discoveryFactoryWithJournal(factory, journal)
}
func NewDiscoveryPilotFactoryWithTriggerEdgesAndJournal(settings Settings, repository discoveryRegistry, filters []TopicFilter, journal *observability.Journal) (integrations.PilotFactory, error) {
	factory, err := NewDiscoveryPilotFactoryWithTriggerEdges(settings, repository, filters)
	if err != nil {
		return nil, err
	}
	return discoveryFactoryWithJournal(factory, journal)
}

func (p *Pilot) releaseObservationQueue() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.observationInstalled {
		return
	}
	remaining := len(p.observationQueue)
	for len(p.observationQueue) > 0 {
		<-p.observationQueue
	}
	_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, -1, -int64(remaining), -queueCapacity, 0, 0)
	p.observationInstalled = false
	p.observationQueue = nil
}

func (p *Pilot) AttachObservation(cells *observability.Cells, journal *observability.Journal) error {
	if p == nil || journal == nil {
		return ErrInvalidSettings
	}
	epoch, err := cells.Epoch()
	if err != nil {
		return ErrInvalidSettings
	}
	attached, err := journal.Cells()
	if err != nil || attached != cells {
		return ErrInvalidSettings
	}
	_ = epoch
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase != pilotCreated {
		return ErrInvalidSettings
	}
	p.observation = cells
	var mapper *DiscoveryMapper
	switch handler := p.handler.(type) {
	case *DiscoveryMapper:
		mapper = handler
	case *triggerEdgeHandler:
		if handler != nil {
			mapper = handler.mapper
		}
	}
	if mapper != nil {
		mapper.journal = journal
		mapper.logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return nil
}
func (p *Pilot) joinObservationQueue(ctx context.Context) error {
	p.mu.Lock()
	done := p.observationConsumerDone
	p.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.releaseObservationQueue()
	return nil
}
