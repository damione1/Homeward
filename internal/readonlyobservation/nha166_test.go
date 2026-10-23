package readonlyobservation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNHA166DurableSegmentsRecoverWithoutOverwritingEvidence(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir); err == nil {
		t.Fatal("two collectors acquired one evidence store")
	}
	for i := 0; i < 10; i++ {
		if err := store.Append("synthetic", struct {
			Index int `json:"index"`
		}{i}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := os.ReadFile(filepath.Join(dir, "segment-000001.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	sequence := store.State.Sequence
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.State.Sequence != sequence || store.State.Segment != 2 {
		t.Fatal("checkpoint/segment recovery lost its durable cursor", store.State)
	}
	if err := store.Append("recovered", struct{}{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "segment-000001.ndjson"))
	if err != nil || string(after) != string(first) {
		t.Fatal("recovery overwrote old evidence")
	}
	rows, partial, err := ValidateEvidence(dir)
	if err != nil || rows != 11 || partial {
		t.Fatal("durable evidence invalid", rows, partial, err)
	}
}
func TestNHA166EvidenceCapsRefuseWithoutDiscardingHistory(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append("oversized", strings.Repeat("x", MaxRecordBytes)); err != ErrStorage {
		t.Fatal("oversized record admitted", err)
	}
	if store.State.Sequence != 0 {
		t.Fatal("oversized record advanced checkpoint")
	}
	store.State.Bytes = RunBytes - 10
	if err := store.Append("full", struct{}{}); err != ErrStorage {
		t.Fatal("run cap did not fail closed", err)
	}
}

type stoppedSource struct {
	cancel context.CancelFunc
	reads  int
}

func (s *stoppedSource) Read(context.Context, uint64) (Sample, error) {
	s.reads++
	s.cancel()
	return Sample{Epoch: "synthetic-epoch", Binding: Binding{Profile: "synthetic"}, ConfigRevision: 1}, nil
}
func TestNHA166CollectorRestartNeverAddsSavedDuration(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.State.WindowStart = time.Now().Add(-25 * time.Hour)
	store.State.WindowElapsed = 25 * time.Hour
	if err := store.Append("synthetic_saved_interval", struct{}{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	source := &stoppedSource{cancel: cancel}
	if err := Run(ctx, store, source); err != context.Canceled {
		t.Fatal("cancelled collector did not stop", err)
	}
	if store.State.Complete || store.State.WindowElapsed >= RequiredWindow || store.State.Interruptions < 2 || source.reads != 1 {
		t.Fatal("recovery invented contiguous24h", store.State)
	}
	raw, err := os.ReadFile(filepath.Join(store.dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if json.Unmarshal(raw, &report) != nil || report.Status != "incomplete" || report.PhysicalActionsValidated {
		t.Fatal("report claimed completed or physically validated observation")
	}
}
func TestNHA166ProtectedConfigurationRejectsHTTPAndUnexpectedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observe.json")
	for _, raw := range []string{`{"url":"http://127.0.0.1:1"}`, `{"url":"https://127.0.0.1:1","password":"NHA166_CANARY"}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil || strings.Contains(err.Error(), "NHA166_CANARY") {
			t.Fatal("unsafe config admitted or leaked", err)
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	var target Config
	if err := readProtectedJSON(path, &target); err == nil {
		t.Fatal("public credentials/config file admitted")
	}
}

func TestNHA166RecoveryRetainsFsyncedRowsBeyondCheckpointAndPartialTail(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("checkpointed", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "segment-000001.ndjson")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a fatal after evidence fsync but before checkpoint rename, then a
	// later partially written row. Both old bytes must remain inspectable.
	row, _ := json.Marshal(Record{Sequence: 2, UTC: time.Now().UTC(), Kind: "fsynced_uncheckpointed", Data: json.RawMessage(`{}`)})
	if _, err := file.Write(append(row, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`{"sequence":3,"kind":"partial`)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.State.Sequence != 2 || !store.State.RecoveredPartial {
		t.Fatal("recovery lost an uncertain append or its partial tail", store.State)
	}
	if err := store.Append("recovery", struct{}{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(old) {
		t.Fatal("recovery rewrote interrupted evidence")
	}
	rows, partial, err := ValidateEvidence(dir)
	if err != nil || rows != 3 || !partial {
		t.Fatal("partial durable evidence was hidden", rows, partial, err)
	}
}
