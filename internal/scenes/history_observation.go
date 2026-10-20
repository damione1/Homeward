package scenes

import (
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
)

type SceneInvocationOutcome uint8

const (
	SceneInvocationInvalid SceneInvocationOutcome = iota
	SceneInvocationComplete
	SceneInvocationPartial
	SceneInvocationNone
	SceneInvocationIndeterminate
	SceneInvocationNotAttempted
)

type HistoryCause struct {
	Run                       automationruns.HistoryRunRef
	NodeVisit, CommandOrdinal uint64
	Source                    automationruns.HistorySource
}

type SceneInvocationObservation struct {
	At         time.Time
	Cause      HistoryCause
	SceneID    domain.HAAlias
	Invocation domain.CommandID
	Outcome    SceneInvocationOutcome
}

type SceneMemberObservation struct {
	At                    time.Time
	Cause                 HistoryCause
	SceneID               domain.HAAlias
	Invocation, CommandID domain.CommandID
	Index                 int
	Alias                 domain.HAAlias
	File                  string
	Line, Column          int
	Outcome               MemberOutcome
}

type HistoryObservationSink interface {
	ObserveSceneInvocation(SceneInvocationObservation)
	ObserveSceneMember(SceneMemberObservation)
}

// The optional sink is attached during Run composition, before any apply.
// A direct technical invocation may supply its own provenance B in HistoryCause;
// script calls carry the admitted parent run's provenance A.
func (s *Service) AttachHistoryObservationSink(sink HistoryObservationSink, fallback automationruns.DefinitionProvenance) error {
	if s == nil || nilPort(sink) || !fallback.Valid() || nilPort(s.clock) || nilPort(s.capture) || nilPort(s.generation) || nilPort(s.commands) {
		return ErrInvalidScene
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.history != nil || s.closed || len(s.applying) != 0 {
		return ErrInvalidScene
	}
	s.history, s.historyProvenance = sink, fallback
	return nil
}

func (s *Service) historyCause(cause HistoryCause) HistoryCause {
	if !cause.Run.Provenance.Valid() {
		cause.Run.Provenance = s.historyProvenance
	}
	return cause
}
