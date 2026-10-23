package readonlyobservation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA166SummaryDistinguishesSamplesRetriesAndUnmeasuredResources(t *testing.T) {
	var summary Summary
	rss, cpu := uint64(123), uint64(456)
	sample := Sample{States: StateSummary{Total: 3, Available: 2, Unavailable: 1, Fresh: 2, Stale: 1, OldestReceivedAgeSeconds: 42}, ServerRSSBytes: &rss, ServerCPUMicroseconds: &cpu, PilotAttempts: 2}
	summary.add(sample)
	sample.ServerRSSBytes = nil
	sample.ServerCPUMicroseconds = nil
	sample.PilotAttempts = 4
	summary.add(sample)
	summary.failure("read_unauthenticated")
	summary.failure("read_resource_exhausted")
	summary.failure("runtime_journal_gap")
	if summary.Samples != 2 || summary.AvailableStateSamples != 4 || summary.StaleStateSamples != 2 || summary.MaxReceivedAgeSeconds != 42 || summary.MaxServerRSSBytes != 123 || summary.LastServerCPUMicroseconds != 456 || summary.MissingRSSSamples != 1 || summary.MissingCPUSamples != 1 || summary.PilotRetryIncrements != 2 || summary.NativeAuthenticationFailures != 1 || summary.NativeResourceExhaustedFailures != 1 || summary.JournalGaps != 1 {
		t.Fatal("summary invented zero or confused retries/populations", summary)
	}
}
func TestNHA166DurableLogProjectionOmitsHumanIdentity(t *testing.T) {
	row := &obs.RuntimeLogRow{Sequence: 1, Event: &obs.RuntimeEvent{Code: obs.LogCode_LOG_CODE_PROCESS_READY, Level: obs.LogLevel_LOG_LEVEL_INFO, Component: obs.LogComponent_LOG_COMPONENT_PROCESS, OccurredAt: timestamppb.New(time.Now()), Author: &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_HumanAccount{HumanAccount: &auth.AccountId{Value: "NHA166_PRIVATE_HUMAN_ACCOUNT_CANARY"}}}}}
	raw, err := sanitizedRuntimeLog(row)
	if err != nil || strings.Contains(string(raw), "CANARY") || strings.Contains(string(raw), "human") || strings.Contains(string(raw), "author") || !strings.Contains(string(raw), "LOG_CODE_PROCESS_READY") {
		t.Fatal("durable projection retained a private author or lost finite event", err)
	}
}

func TestNHA166EvidenceCapRetainsIncompleteReport(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.State.Bytes = RunBytes - 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &stoppedSource{cancel: cancel}
	if err := Run(ctx, store, source); err != ErrStorage {
		t.Fatal("record cap did not terminate", err)
	}
	raw, err := os.ReadFile(filepath.Join(store.dir, "report.json"))
	if err != nil {
		t.Fatal("record cap lost small terminal report", err)
	}
	var report Report
	if json.Unmarshal(raw, &report) != nil || report.Status != "failed" || report.Checkpoint.Complete || report.Checkpoint.FatalReason != "evidence_storage_unavailable" {
		t.Fatal("record cap retained a misleading collection state")
	}
}
