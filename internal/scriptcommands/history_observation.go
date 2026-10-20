package scriptcommands

import (
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
)

type CommandOutcome uint8

const (
	CommandOutcomeInvalid CommandOutcome = iota
	CommandAccepted
	CommandDispatched
	CommandAcknowledged
	CommandConfirmed
	CommandIndeterminate
	CommandRefused
	CommandNotAttempted
)

type CommandObservation struct {
	At                        time.Time
	Run                       automationruns.HistoryRunRef
	NodeVisit, CommandOrdinal uint64
	Source                    automationruns.HistorySource
	ID                        domain.CommandID
	Outcome                   CommandOutcome
}

type HistoryObservationSink interface{ ObserveCommand(CommandObservation) }

func (a *Adapter) observeCommand(id domain.CommandID, execution automationruns.CommandExecution, outcome CommandOutcome) {
	if a.input.History == nil {
		return
	}
	a.input.History.ObserveCommand(CommandObservation{At: a.input.Clock.Now().UTC(),
		Run:       automationruns.HistoryRunRef{Generation: execution.Generation(), RunID: execution.RunID(), ParentID: execution.ParentID(), Provenance: execution.Provenance()},
		NodeVisit: execution.NodeVisit(), CommandOrdinal: execution.Ordinal(), Source: execution.HistorySource(), ID: id, Outcome: outcome})
}
