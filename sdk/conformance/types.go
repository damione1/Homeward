package conformance

import (
	"context"
	"fmt"
)

// Case is a closed set of independently reset conformance scenarios.
type Case uint8

const (
	CaseUnspecified Case = iota
	CaseCore
	CaseCapacity
	CaseReconnect
	CaseClimatePartial
)

func (c Case) String() string {
	switch c {
	case CaseCore:
		return "core"
	case CaseCapacity:
		return "capacity"
	case CaseReconnect:
		return "reconnect"
	case CaseClimatePartial:
		return "climate_partial"
	case CaseUnspecified:
		return "unspecified"
	default:
		return "invalid"
	}
}

// Stimulus asks a synthetic pilot to exercise its normal Host or worker path.
// The suite reads the resulting Host state itself; Apply cannot assert success.
type Stimulus uint8

const (
	StimulusUnspecified Stimulus = iota
	StimulusAcknowledge
	StimulusUncorrelated
	StimulusWrongCorrelation
	StimulusStale
	StimulusPartialLight
	StimulusMatch
	StimulusPartialClimate
)

func (s Stimulus) String() string {
	switch s {
	case StimulusAcknowledge:
		return "ack"
	case StimulusUncorrelated:
		return "uncorrelated"
	case StimulusWrongCorrelation:
		return "wrong_correlation"
	case StimulusStale:
		return "stale"
	case StimulusPartialLight:
		return "partial_light"
	case StimulusMatch:
		return "match"
	case StimulusPartialClimate:
		return "partial_climate"
	case StimulusUnspecified:
		return "unspecified"
	default:
		return "invalid"
	}
}

// Adapter is a simulator bridge. Spec supplies the real production PilotSpec;
// all other methods merely synchronize synthetic pilot output with the suite.
// A conforming adapter never publishes directly into the fixture or mutates
// action progression. Every state crosses its actual SessionHost or gRPC RPC.
type Adapter interface {
	Spec(IntegrationInstanceID) (PilotSpec, error)
	Completion() CompletionSemantics
	Ready(context.Context) error
	Delivered(context.Context, CommandID) error
	Apply(context.Context, CommandID, Stimulus) error
	Crash(context.Context) error
	Deliveries() int
	Close() error
}

// CompletionSemantics describes what a synthetic transport completion means
// to the production action tracker. A worker ACCEPTED frame is transport only.
type CompletionSemantics uint8

const (
	CompletionUnspecified CompletionSemantics = iota
	CompletionTransportAccepted
	CompletionAcknowledged
)

// AdapterFactory must create a fresh adapter for each Case. The directory is
// caller-owned and available for Unix sockets or other synthetic controls.
type AdapterFactory func(Case, string) (Adapter, error)

type FailureCode uint8

const (
	FailureUnspecified FailureCode = iota
	FailureSetup
	FailureDiscovery
	FailureDispatch
	FailureUnprovenConfirmation
	FailureMissingConfirmation
	FailureDuplicate
	FailureTimeout
	FailureLateResponse
	FailureCapacity
	FailureReconnect
	FailureTeardown
)

func (c FailureCode) String() string {
	switch c {
	case FailureSetup:
		return "setup"
	case FailureDiscovery:
		return "discovery"
	case FailureDispatch:
		return "dispatch"
	case FailureUnprovenConfirmation:
		return "unproven_confirmation"
	case FailureMissingConfirmation:
		return "missing_confirmation"
	case FailureDuplicate:
		return "duplicate"
	case FailureTimeout:
		return "timeout"
	case FailureLateResponse:
		return "late_response"
	case FailureCapacity:
		return "capacity"
	case FailureReconnect:
		return "reconnect"
	case FailureTeardown:
		return "teardown"
	case FailureUnspecified:
		return "unspecified"
	default:
		return "invalid"
	}
}

// Failure is immutable and identifies the precise contract step that failed.
type Failure struct {
	code     FailureCode
	scenario Case
	detail   string
}

func (f Failure) Code() FailureCode { return f.code }
func (f Failure) Scenario() Case    { return f.scenario }
func (f Failure) Error() string     { return fmt.Sprintf("%s/%s: %s", f.scenario, f.code, f.detail) }

func fail(scenario Case, code FailureCode, format string, args ...any) Failure {
	return Failure{code: code, scenario: scenario, detail: fmt.Sprintf(format, args...)}
}
