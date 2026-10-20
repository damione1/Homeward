package executionhistory

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/scriptcommands"
	"github.com/damione1/Homeward/internal/storage"
)

func nha186Sink(t *testing.T) *Sink {
	t.Helper()
	epoch, err := storage.ParseHistoryEpoch("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return &Sink{epoch: epoch, queue: make([]storage.HistoryRecord, 0, maxQueueRecords),
		lastBoundary: time.Now().UTC(), notify: make(chan struct{}, 1), done: make(chan struct{})}
}

func TestNHA186ProducerAdmissionBoundsAndSceneCorrelation(t *testing.T) {
	sink := nha186Sink(t)
	at := time.Now().UTC()
	run := automationruns.HistoryRunRef{Generation: 3, RunID: 7, ParentID: 2, Provenance: automationruns.FixtureProvenance()}
	command, _ := domain.ParseCommandID("script-186-command")
	sink.ObserveCommand(scriptcommands.CommandObservation{At: at, Run: run, NodeVisit: 4, CommandOrdinal: 1,
		Source: automationruns.HistorySource{Object: "script.fixture", File: "script.yaml", Path: "actions/0", Line: 3, Column: 2},
		ID:     command, Outcome: scriptcommands.CommandAcknowledged})
	scene, _ := domain.ParseHAAlias("scene.fixture")
	member, _ := domain.ParseHAAlias("switch.fixture")
	invocation, _ := domain.ParseCommandID("scene-186-command")
	cause := scenes.HistoryCause{Run: run, NodeVisit: 5, CommandOrdinal: 2,
		Source: automationruns.HistorySource{Object: "script.fixture", Path: "actions/1"}}
	sink.ObserveSceneMember(scenes.SceneMemberObservation{At: at, Cause: cause, SceneID: scene, Invocation: invocation,
		CommandID: command, Index: 0, Alias: member, File: "scene.yaml", Line: 8, Column: 4, Outcome: scenes.OutcomeRefused})
	sink.ObserveSceneInvocation(scenes.SceneInvocationObservation{At: at, Cause: cause, SceneID: scene,
		Invocation: invocation, Outcome: scenes.SceneInvocationNone})
	if len(sink.queue) != 3 {
		t.Fatalf("admitted %d records", len(sink.queue))
	}
	for i, record := range sink.queue {
		in := record.Input()
		if in.ID.Ordinal() != uint64(i+1) || in.RunID != 7 || in.ParentRunID != 2 ||
			in.Provenance != storage.HistoryUnboundFixture {
			t.Fatalf("lost correlation: %+v", in)
		}
	}
	if in := sink.queue[1].Input(); in.SceneOutcome != 3 || in.SceneInvocation != "scene-186-command" || in.SceneMember != 0 ||
		in.SourceFile != "scene.yaml" || in.CommandID != "script-186-command" {
		t.Fatalf("member metadata: %+v", in)
	}
	if in := sink.queue[2].Input(); in.SceneAggregate != 3 || in.SceneOutcome != 0 {
		t.Fatalf("aggregate: %+v", in)
	}

	// The lock is an admission boundary, not a wait on the SQL writer.
	sink.mu.Lock()
	sink.ObserveCommand(scriptcommands.CommandObservation{At: at, Run: run, NodeVisit: 6, CommandOrdinal: 3,
		ID: command, Outcome: scriptcommands.CommandAccepted})
	sink.mu.Unlock()
	if len(sink.queue) != 3 || sink.rejected[storage.HistoryLossContention].Load() != 1 {
		t.Fatal("contended observation waited or entered the queue")
	}
	sink.ObserveCommand(scriptcommands.CommandObservation{At: at, Run: run, NodeVisit: 6, CommandOrdinal: 3,
		ID: command, Outcome: scriptcommands.CommandOutcome(255)})
	sink.ObserveCommand(scriptcommands.CommandObservation{At: at, Run: run, NodeVisit: 6, CommandOrdinal: 3, ID: command, Outcome: scriptcommands.CommandOutcomeInvalid})
	if len(sink.queue) != 3 || sink.rejected[storage.HistoryLossInvalidMetadata].Load() != 2 {
		t.Fatal("unknown producer outcome was admitted")
	}
}
