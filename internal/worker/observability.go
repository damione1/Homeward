package worker

import (
	"io"
	"log"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
)

func admittedWorkerJournal(journal *observability.Journal) bool {
	if journal == nil {
		return false
	}
	_, err := journal.Snapshot()
	return err == nil
}

// NewPilotWithJournal delegates all operational admission to NewPilot and
// attaches diagnostics before the returned worker can own a process or socket.
func NewPilotWithJournal(config Config, journal *observability.Journal) (integrations.Pilot, error) {
	if !admittedWorkerJournal(journal) {
		return nil, ErrInvalidConfig
	}
	existing, err := NewPilot(config)
	if err != nil {
		return nil, err
	}
	p, ok := existing.(*pilot)
	if !ok || p == nil {
		return nil, ErrInvalidConfig
	}
	p.journal = journal
	p.observation, _ = journal.Cells()
	return p, nil
}
func NewPilotSpecWithJournal(config Config, journal *observability.Journal) (integrations.PilotSpec, error) {
	instance, err := config.Instance()
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	if !admittedWorkerJournal(journal) {
		return integrations.PilotSpec{}, ErrInvalidConfig
	}
	return integrations.NewPilotSpec(instance, func() integrations.Pilot {
		p, err := NewPilotWithJournal(config, journal)
		if err != nil {
			return nil
		}
		return p
	})
}

func (p *pilot) diagnosticErrorLog() *log.Logger {
	var writer io.Writer = io.Discard
	if p != nil && p.journal != nil {
		if safe, err := observability.NewHTTPDiagnosticWriter(p.journal, obs.LogComponent_LOG_COMPONENT_WORKER_TRANSPORT); err == nil {
			writer = safe
		}
	}
	// Legacy constructors suppress untrusted transport diagnostics too. Only a
	// journal-enabled composition produces a bounded closed event from them.
	return log.New(writer, "", 0)
}

func (p *pilot) observeWorkerQueued(item *delivery) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.observationInstalled || item.observationDequeued {
		return
	}
	item.observationQueued = true
	p.observationQueued++
	_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, 0, 1, 0, 0, 0)
}
func (p *pilot) observeWorkerDequeued(item *delivery) {
	p.mu.Lock()
	defer p.mu.Unlock()
	item.observationDequeued = true
	if item.observationQueued {
		item.observationQueued = false
		p.observationQueued--
		_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, 0, -1, 0, 0, 0)
	}
}

// Pending and handed-off entries are tracked independently from the channel:
// cancellation can leave a stale queued delivery, so no subtraction derives
// in-flight or pending from queue depth.
func (p *pilot) observeWorkerRemoveLocked(item *delivery) {
	if !p.observationInstalled {
		return
	}
	p.observationPending--
	inflight := int64(0)
	if item.delivered {
		p.observationInflight--
		inflight = -1
	}
	_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, 0, 0, 0, inflight, -1)
}
func (p *pilot) releaseWorkerObservation() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.observationInstalled {
		return
	}
	for len(p.queue) > 0 {
		<-p.queue
	}
	_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, -1, -int64(p.observationQueued), -maxPending, -int64(p.observationInflight), -int64(p.observationPending))
	p.observationInstalled = false
	p.observationQueued = 0
	p.observationInflight = 0
	p.observationPending = 0
}

func (p *pilot) AttachObservation(cells *observability.Cells, journal *observability.Journal) error {
	if p == nil || journal == nil {
		return ErrInvalidConfig
	}
	if _, err := cells.Epoch(); err != nil {
		return ErrInvalidConfig
	}
	attached, err := journal.Cells()
	if err != nil || attached != cells {
		return ErrInvalidConfig
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active || p.process != nil || p.server != nil {
		return ErrInvalidConfig
	}
	p.observation, p.journal = cells, journal
	return nil
}
