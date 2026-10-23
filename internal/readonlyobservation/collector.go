package readonlyobservation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const RequiredWindow = 24 * time.Hour
const PollInterval = 5 * time.Second
const MaxPollGap = 15 * time.Second

var ErrSample = errors.New("observation read failed")

type Binding struct {
	Profile    string `json:"profile"`
	Executable string `json:"executable_sha256"`
	Manifest   string `json:"manifest_sha256"`
	Source     string `json:"source_sha256"`
}
type StateSummary struct {
	Total                    uint64  `json:"total"`
	Available                uint64  `json:"available"`
	Unknown                  uint64  `json:"unknown"`
	Unavailable              uint64  `json:"unavailable"`
	Fresh                    uint64  `json:"fresh"`
	Stale                    uint64  `json:"stale"`
	OldestReceivedAgeSeconds float64 `json:"oldest_received_age_seconds"`
	MissingTimestamp         uint64  `json:"missing_timestamp"`
}
type Sample struct {
	Epoch                 string            `json:"process_epoch"`
	Binding               Binding           `json:"binding"`
	ConfigRevision        uint64            `json:"config_revision"`
	DriverEntries         uint64            `json:"command_driver_entries"`
	States                StateSummary      `json:"states"`
	Health                json.RawMessage   `json:"health"`
	Metrics               json.RawMessage   `json:"metrics"`
	Integration           json.RawMessage   `json:"integration"`
	Logs                  []json.RawMessage `json:"-"`
	LogFirst              uint64            `json:"log_first"`
	LogUpper              uint64            `json:"log_upper"`
	JournalGap            bool              `json:"journal_gap"`
	ServerRSSBytes        *uint64           `json:"server_rss_bytes,omitempty"`
	ServerCPUMicroseconds *uint64           `json:"server_cpu_microseconds,omitempty"`
	ResourceSource        string            `json:"resource_source"`
	HAComparison          string            `json:"ha_comparison"`
	PilotAttempts         uint64            `json:"pilot_attempts"`
}
type Source interface {
	Read(context.Context, uint64) (Sample, error)
}
type Report struct {
	Status                   string     `json:"status"`
	RequiredSeconds          int64      `json:"required_seconds"`
	Checkpoint               Checkpoint `json:"checkpoint"`
	PhysicalActionsValidated bool       `json:"physical_actions_validated"`
	HAComparison             string     `json:"ha_comparison"`
}

// Run is a single autonomous process. A collector/server restart, journal gap
// or failed read starts a new measured window and retains its interruption.
// The saved UTC duration is never added across process incarnations.
func Run(ctx context.Context, store *Store, source Source) error {
	return run(ctx, store, source, RequiredWindow, PollInterval, MaxPollGap)
}

// Private duration seam only shortens device-free tests; the production entry
// point above always requires 24 real monotonic hours.
func run(ctx context.Context, store *Store, source Source, required, poll, maxGap time.Duration) (returnedErr error) {
	if ctx == nil || store == nil || source == nil {
		return ErrSample
	}

	// A record cap still reserves the small report/checkpoint files. Try to
	// retain a terminal incomplete report on failure; never leave an in-memory
	// Complete flag after its completion append or checkpoint failed.
	defer func() {
		if returnedErr != nil {
			store.State.Complete = false
			status := "incomplete"
			if store.State.FatalReason != "" {
				status = "failed"
			} else if returnedErr == ErrStorage {
				store.State.FatalReason = "evidence_storage_unavailable"
				status = "failed"
			}
			_ = store.Report(report(store, status))
		}
	}()
	if store.State.Complete {
		return store.Report(report(store, "complete"))
	}
	resumed := store.State.Sequence > 0
	if resumed {
		store.State.Interruptions++
		store.State.WindowStart = time.Time{}
		store.State.WindowElapsed = 0
		store.State.WindowSummary = Summary{}
		store.State.LogSequence = 0
	}
	if err := store.Append("collector_start", struct {
		Resumed bool `json:"resumed"`
	}{resumed}); err != nil {
		return err
	}
	var start, last time.Time
	var epoch string
	var binding Binding
	var revision uint64
	nextHour := time.Hour
	for {
		readStarted := time.Now()
		sample, err := source.Read(ctx, store.State.LogSequence)
		now := time.Now()
		reason := ""
		switch {
		case err != nil:
			reason = "read_unavailable"
			var failure *ReadError
			if errors.As(err, &failure) {
				reason = "read_" + failure.Code
			}
		case sample.DriverEntries != 0:
			reason = "nonzero_driver_entries"
		case sample.JournalGap:
			reason = "runtime_journal_gap"
		case now.Sub(readStarted) > maxGap || !last.IsZero() && now.Sub(last) > maxGap:
			reason = "collection_gap"
		case epoch != "" && sample.Epoch != epoch:
			reason = "server_restart"
		case epoch != "" && (sample.Binding != binding || sample.ConfigRevision != revision):
			reason = "source_or_config_changed"
		}
		if reason != "" {
			store.State.Interruptions++
			store.State.TotalSummary.failure(reason)
			store.State.TotalSummary.MaxCommandDriverEntries = max(store.State.TotalSummary.MaxCommandDriverEntries, sample.DriverEntries)
			store.State.WindowSummary = Summary{}
			store.State.WindowStart = time.Time{}
			store.State.WindowElapsed = 0
			start = time.Time{}
			nextHour = time.Hour
			if e := store.Append("interruption", struct {
				Reason string `json:"reason"`
			}{reason}); e != nil {
				return e
			}
			if reason == "nonzero_driver_entries" || reason == "source_or_config_changed" || reason == "read_binding_mismatch" {
				store.State.FatalReason = reason
				_ = store.Report(report(store, "failed"))
				return ErrSample
			}
		}
		if err == nil {
			if epoch != sample.Epoch {
				store.State.LogSequence = 0
			}
			epoch, binding, revision = sample.Epoch, sample.Binding, sample.ConfigRevision
			store.State.ProcessEpoch = epoch
			// Logs are sanitized, bounded protobuf projections supplied by Read.
			for _, row := range sample.Logs {
				if e := store.Append("runtime_log", row); e != nil {
					return e
				}
			}
			store.State.LogSequence = sample.LogUpper
			if start.IsZero() {
				start = now
				store.State.WindowStart = now.UTC()
			}
			store.State.LastSample = now.UTC()
			store.State.WindowElapsed = now.Sub(start)
			store.State.Samples++
			store.State.TotalSummary.add(sample)
			store.State.WindowSummary.add(sample)
			if e := store.Append("sample", sample); e != nil {
				return e
			}
			if store.State.WindowElapsed >= nextHour {
				if e := store.Append("hourly_summary", report(store, "collecting")); e != nil {
					return e
				}
				nextHour += time.Hour
			}
			if store.State.WindowElapsed >= required {
				store.State.Complete = true
				if e := store.Append("window_complete", report(store, "complete")); e != nil {
					return e
				}
				return store.Report(report(store, "complete"))
			}
		}
		if e := store.Report(report(store, "collecting")); e != nil {
			return e
		}
		last = now
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			store.State.Interruptions++
			store.State.Complete = false
			_ = store.Append("collector_stopped", struct {
				Reason string `json:"reason"`
			}{"context_cancelled"})
			_ = store.Report(report(store, "incomplete"))
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func report(store *Store, status string) Report {
	return Report{status, int64(RequiredWindow / time.Second), store.State, false, "unavailable: authorized Home Assistant comparison source not configured"}
}
