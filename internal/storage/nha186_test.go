package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNHA186HistoryFailureRedactsProvider(t *testing.T) {
	provider := errors.New("postgres provider secret DSN=canary-hidden")
	err := historyFailure(ErrHistoryUnavailable, provider)
	if !errors.Is(err, ErrHistoryUnavailable) || !errors.Is(err, provider) {
		t.Fatal("classification or cause was lost")
	}
	if strings.Contains(err.Error(), "canary-hidden") || err.Error() != ErrHistoryUnavailable.Error() {
		t.Fatal("provider error escaped formatted runtime diagnostic")
	}
}

func nha186Epoch(t *testing.T, text string) HistoryEpoch {
	t.Helper()
	epoch, err := ParseHistoryEpoch(text)
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

func nha186Record(t *testing.T, epoch HistoryEpoch, ordinal uint64, at time.Time, decision uint8) HistoryRecord {
	t.Helper()
	id, err := NewHistoryTraceID(epoch, ordinal)
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewHistoryRecord(HistoryRecordInput{ID: id, EmissionTime: at, Kind: HistoryDecision,
		Decision: decision, ControllerGeneration: 7, RunID: 9, Provenance: HistoryUnboundFixture,
		ObjectID: "automation.fixture", TriggerKey: "edge-1"})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func nha186Batch(t *testing.T, epoch HistoryEpoch, number uint64, records ...HistoryRecord) HistoryBatch {
	t.Helper()
	batch, err := NewHistoryBatch(epoch, number, records, nil)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestNHA186StoreCommitReopenConflictAndExactPurge(t *testing.T) {
	store, settings := nha051Store(t)
	ctx := context.Background()
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	if _, err := store.OpenExecutionHistoryEpoch(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	original := nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, at, 2))
	if got, err := store.CommitExecutionHistoryBatch(ctx, original); err != nil || got != HistoryCommitted {
		t.Fatalf("commit: %v %v", got, err)
	}
	if got, err := store.CommitExecutionHistoryBatch(ctx, original); err != nil || got != HistoryAlreadyCommitted {
		t.Fatalf("retry: %v %v", got, err)
	}
	if got, err := store.ResolveExecutionHistoryBatch(ctx, original); err != nil || got != HistoryResolutionCommitted {
		t.Fatalf("resolve: %v %v", got, err)
	}
	conflict := nha186Batch(t, epoch, 1, nha186Record(t, epoch, 1, at, 12))
	if _, err := store.CommitExecutionHistoryBatch(ctx, conflict); !errors.Is(err, ErrHistoryIntegrity) {
		t.Fatalf("conflict: %v", err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err := reopened.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16})
	if err != nil || len(page.Records) != 1 || page.Records[0].Record.ID().Ordinal() != 1 || page.Records[0].Record.Input().Decision != 2 || page.Checkpoint != 1 {
		t.Fatalf("reopen: page=%+v err=%v", page, err)
	}
	if _, err := reopened.PurgeExecutionHistory(ctx, at); err != nil {
		t.Fatal(err)
	}
	page, err = reopened.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("equality lost: %+v %v", page, err)
	}
	cutoff := at.Add(time.Microsecond)
	first, err := reopened.PurgeExecutionHistory(ctx, cutoff)
	if err != nil || first.DeletedRecords != 1 {
		t.Fatalf("purge: %+v %v", first, err)
	}
	second, err := reopened.PurgeExecutionHistory(ctx, cutoff)
	if err != nil || second.DeletedRecords != 0 || second.MoreExpired {
		t.Fatalf("repeat purge: %+v %v", second, err)
	}
	if _, err := reopened.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Since: at, Limit: 16}); !errors.Is(err, ErrHistoryOutsideRetention) {
		t.Fatalf("outside retention: %v", err)
	}
	late := nha186Batch(t, epoch, 2, nha186Record(t, epoch, 2, at, 12))
	if _, err := reopened.CommitExecutionHistoryBatch(ctx, late); err != nil {
		t.Fatalf("late batch: %v", err)
	}
	page, err = reopened.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: epoch, Limit: 16})
	if err != nil || len(page.Records) != 0 || page.Checkpoint != 2 {
		t.Fatalf("resurrection: %+v %v", page, err)
	}
}

func TestNHA186HistoryRecordRejectsUnknownAndUnsafeMetadata(t *testing.T) {
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	at := time.Now().UTC()
	id, _ := NewHistoryTraceID(epoch, 1)
	base := HistoryRecordInput{ID: id, EmissionTime: at, Kind: HistoryDecision, Decision: 2, Provenance: HistoryUnboundFixture}
	for name, mutate := range map[string]func(*HistoryRecordInput){
		"zero issue":      func(in *HistoryRecordInput) { in.Decision = 0 },
		"future issue":    func(in *HistoryRecordInput) { in.Decision = 255 },
		"raw path":        func(in *HistoryRecordInput) { in.SourceFile = "../secrets.yaml" },
		"invalid utf8":    func(in *HistoryRecordInput) { in.ObjectID = string([]byte{0xff}) },
		"zero bound hash": func(in *HistoryRecordInput) { in.Provenance = HistoryBoundProgram; in.ConfigRevision = 1 },
	} {
		in := base
		mutate(&in)
		if _, err := NewHistoryRecord(in); !errors.Is(err, ErrHistoryInvalid) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
}

func TestNHA186AdmissionMatchesMigration20IssueChecks(t *testing.T) {
	epoch := nha186Epoch(t, "0123456789abcdef0123456789abcdef")
	id, _ := NewHistoryTraceID(epoch, 1)
	for _, family := range []struct {
		kind    HistoryRecordKind
		maximum uint8
		check   string
		set     func(*HistoryRecordInput, uint8)
	}{
		{HistoryDecision, 18, "decision smallint CHECK (decision BETWEEN 1 AND 18)", func(in *HistoryRecordInput, v uint8) { in.Decision = v }},
		{HistoryNode, 6, "node_stage smallint CHECK (node_stage BETWEEN 1 AND 6)", func(in *HistoryRecordInput, v uint8) { in.NodeStage = v; in.NodeVisit = 1; in.ActionKind = 1 }},
		{HistoryCommand, 7, "command_outcome smallint CHECK (command_outcome BETWEEN 1 AND 7)", func(in *HistoryRecordInput, v uint8) { in.CommandOutcome = v }},
		{HistorySceneMember, 8, "scene_outcome smallint CHECK (scene_outcome BETWEEN 1 AND 8)", func(in *HistoryRecordInput, v uint8) { in.SceneOutcome = v }},
		{HistorySceneInvocation, 5, "scene_aggregate smallint CHECK (scene_aggregate BETWEEN 1 AND 5)", func(in *HistoryRecordInput, v uint8) { in.SceneAggregate = v }},
		{HistoryScriptLink, 2, "script_mode smallint CHECK (script_mode BETWEEN 1 AND 2)", func(in *HistoryRecordInput, v uint8) { in.ScriptMode = v }},
	} {
		if !strings.Contains(executionHistorySchemaSQL, family.check) {
			t.Fatalf("migration20 lacks %s", family.check)
		}
		for issue := uint8(1); issue <= family.maximum+1; issue++ {
			in := HistoryRecordInput{ID: id, EmissionTime: time.Now().UTC(), Kind: family.kind, Provenance: HistoryUnboundFixture}
			family.set(&in, issue)
			_, err := NewHistoryRecord(in)
			if (err == nil) != (issue <= family.maximum) {
				t.Errorf("kind=%d issue=%d admission=%v", family.kind, issue, err)
			}
		}
	}
}
