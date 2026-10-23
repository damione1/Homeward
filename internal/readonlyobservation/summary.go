package readonlyobservation

// Summary contains aggregate measurements only. Counts of entity states are
// sample populations, never unique devices or a claim of HA capability parity.
type Summary struct {
	MaxCommandDriverEntries         uint64  `json:"max_command_driver_entries"`
	PilotRetryIncrements            uint64  `json:"pilot_retry_increments"`
	LastPilotAttempts               uint64  `json:"last_pilot_attempts"`
	GuardViolations                 uint64  `json:"guard_violations"`
	Samples                         uint64  `json:"samples"`
	AvailableStateSamples           uint64  `json:"available_state_samples"`
	UnavailableStateSamples         uint64  `json:"unavailable_state_samples"`
	UnknownStateSamples             uint64  `json:"unknown_state_samples"`
	StaleStateSamples               uint64  `json:"stale_state_samples"`
	FreshStateSamples               uint64  `json:"fresh_state_samples"`
	MissingTimestampSamples         uint64  `json:"missing_timestamp_samples"`
	MaxReceivedAgeSeconds           float64 `json:"max_received_age_seconds"`
	MaxServerRSSBytes               uint64  `json:"max_server_rss_bytes"`
	LastServerCPUMicroseconds       uint64  `json:"last_server_cpu_microseconds"`
	MissingRSSSamples               uint64  `json:"missing_rss_samples"`
	MissingCPUSamples               uint64  `json:"missing_cpu_samples"`
	MaxPilotAttempts                uint64  `json:"max_pilot_attempts"`
	ReadFailures                    uint64  `json:"read_failures"`
	NativeAuthenticationFailures    uint64  `json:"native_authentication_failures"`
	NativeResourceExhaustedFailures uint64  `json:"native_resource_exhausted_failures"`
	JournalGaps                     uint64  `json:"journal_gaps"`
	ServerRestarts                  uint64  `json:"server_restarts"`
	CollectionGaps                  uint64  `json:"collection_gaps"`
}

func (s *Summary) add(sample Sample) {
	if s.Samples > 0 && sample.PilotAttempts >= s.LastPilotAttempts {
		s.PilotRetryIncrements += sample.PilotAttempts - s.LastPilotAttempts
	}
	s.LastPilotAttempts = sample.PilotAttempts
	s.MaxCommandDriverEntries = max(s.MaxCommandDriverEntries, sample.DriverEntries)
	s.Samples++
	s.AvailableStateSamples += sample.States.Available
	s.UnavailableStateSamples += sample.States.Unavailable
	s.UnknownStateSamples += sample.States.Unknown
	s.StaleStateSamples += sample.States.Stale
	s.FreshStateSamples += sample.States.Fresh
	s.MissingTimestampSamples += sample.States.MissingTimestamp
	s.MaxReceivedAgeSeconds = max(s.MaxReceivedAgeSeconds, sample.States.OldestReceivedAgeSeconds)
	if sample.ServerRSSBytes == nil {
		s.MissingRSSSamples++
	} else {
		s.MaxServerRSSBytes = max(s.MaxServerRSSBytes, *sample.ServerRSSBytes)
	}
	if sample.ServerCPUMicroseconds == nil {
		s.MissingCPUSamples++
	} else {
		s.LastServerCPUMicroseconds = *sample.ServerCPUMicroseconds
	}
	s.MaxPilotAttempts = max(s.MaxPilotAttempts, sample.PilotAttempts)
}
func (s *Summary) failure(reason string) {
	switch reason {
	case "nonzero_driver_entries":
		s.GuardViolations++
	case "runtime_journal_gap":
		s.JournalGaps++
	case "server_restart":
		s.ServerRestarts++
	case "collection_gap":
		s.CollectionGaps++
	default:
		s.ReadFailures++
	}
	if reason == "read_unauthenticated" || reason == "read_permission_denied" {
		s.NativeAuthenticationFailures++
	}
	if reason == "read_resource_exhausted" {
		s.NativeResourceExhaustedFailures++
	}
}
