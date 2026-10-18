package domain

import (
	"errors"
	"math"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidState       = errors.New("invalid observed state")
	ErrInvalidStateReport = errors.New("invalid state report")
	ErrOutOfOrderReport   = errors.New("state report is out of order")
)

// The three dimensions are independent. A policy decides whether a state is
// usable; no single quality enum can represent their product.
type Availability uint8

const (
	AvailabilityUnspecified Availability = iota
	AvailabilityAvailable
	AvailabilityUnknown
	AvailabilityUnavailable
)

type Freshness uint8

const (
	FreshnessUnspecified Freshness = iota
	FreshnessFresh
	FreshnessStale
)

type Provenance uint8

const (
	ProvenanceUnspecified Provenance = iota
	ProvenanceObserved
	ProvenanceRestored
	ProvenanceEstimated
)

func (a Availability) valid() bool {
	switch a {
	case AvailabilityAvailable, AvailabilityUnknown, AvailabilityUnavailable:
		return true
	case AvailabilityUnspecified:
		return false
	default:
		return false
	}
}

func (f Freshness) valid() bool {
	switch f {
	case FreshnessFresh, FreshnessStale:
		return true
	case FreshnessUnspecified:
		return false
	default:
		return false
	}
}

func (p Provenance) valid() bool {
	switch p {
	case ProvenanceObserved, ProvenanceRestored, ProvenanceEstimated:
		return true
	case ProvenanceUnspecified:
		return false
	default:
		return false
	}
}

type Quality struct {
	availability Availability
	freshness    Freshness
	provenance   Provenance
}

func NewQuality(availability Availability, freshness Freshness, provenance Provenance) (Quality, error) {
	if !availability.valid() || !freshness.valid() || !provenance.valid() {
		return Quality{}, ErrInvalidStateReport
	}
	return Quality{availability, freshness, provenance}, nil
}

func (q Quality) Components() (Availability, Freshness, Provenance, error) {
	if !q.availability.valid() || !q.freshness.valid() || !q.provenance.valid() {
		return 0, 0, 0, ErrInvalidState
	}
	return q.availability, q.freshness, q.provenance, nil
}

type ValueKind uint8

const (
	ValueUnspecified ValueKind = iota
	ValueSwitch
	ValueMeasurement
	ValueLight
)

// LightValue preserves each observed field independently. Off is a present
// value; absent brightness and color temperature are never filled from zero.
type LightValue struct {
	valid           bool
	on              bool
	brightness      float64
	hasBrightness   bool
	colorTempMireds uint32
	hasColorTemp    bool
}

func NewLightValue(on bool, brightness *float64, colorTempMireds *uint32) (LightValue, error) {
	v := LightValue{valid: true, on: on}
	if brightness != nil {
		if math.IsNaN(*brightness) || math.IsInf(*brightness, 0) || *brightness < 0 || *brightness > 100 {
			return LightValue{}, ErrInvalidState
		}
		v.brightness, v.hasBrightness = *brightness, true
	}
	if colorTempMireds != nil {
		if *colorTempMireds == 0 || *colorTempMireds > 1000 {
			return LightValue{}, ErrInvalidState
		}
		v.colorTempMireds, v.hasColorTemp = *colorTempMireds, true
	}
	return v, nil
}

func (v LightValue) Components() (bool, *float64, *uint32, error) {
	if !v.valid {
		return false, nil, nil, ErrInvalidState
	}
	var brightness *float64
	var colorTemp *uint32
	if v.hasBrightness {
		brightness = &v.brightness
	}
	if v.hasColorTemp {
		colorTemp = &v.colorTempMireds
	}
	if _, err := NewLightValue(v.on, brightness, colorTemp); err != nil {
		return false, nil, nil, err
	}
	if brightness != nil {
		copyValue := *brightness
		brightness = &copyValue
	}
	if colorTemp != nil {
		copyValue := *colorTemp
		colorTemp = &copyValue
	}
	return v.on, brightness, colorTemp, nil
}

// StateValue is a closed sum. Its zero value cannot be published. False and
// numeric zero are ordinary present values, distinct from an absent value.
type StateValue struct {
	kind        ValueKind
	switchOn    bool
	measurement Measurement
	light       LightValue
}

func NewSwitchValue(on bool) StateValue { return StateValue{kind: ValueSwitch, switchOn: on} }

func NewMeasurementValue(measurement Measurement) (StateValue, error) {
	if _, err := measurement.Components(); err != nil {
		return StateValue{}, err
	}
	return StateValue{kind: ValueMeasurement, measurement: measurement}, nil
}

func NewLightStateValue(light LightValue) (StateValue, error) {
	if _, _, _, err := light.Components(); err != nil {
		return StateValue{}, err
	}
	return StateValue{kind: ValueLight, light: light}, nil
}

func (v StateValue) Kind() (ValueKind, error) {
	switch v.kind {
	case ValueSwitch:
		return ValueSwitch, nil
	case ValueMeasurement:
		if _, err := v.measurement.Components(); err == nil {
			return ValueMeasurement, nil
		}
	case ValueLight:
		if _, _, _, err := v.light.Components(); err == nil {
			return ValueLight, nil
		}
	case ValueUnspecified:
	default:
	}
	return ValueUnspecified, ErrInvalidState
}

func (v StateValue) Switch() (bool, bool) { return v.switchOn, v.kind == ValueSwitch }

func (v StateValue) Measurement() (Measurement, bool) {
	return v.measurement, v.kind == ValueMeasurement
}

func (v StateValue) Light() (LightValue, bool) { return v.light, v.kind == ValueLight }

// SourceStamp records a driver's session and optional source sequence. Their
// ordering policy belongs to the later host sequencer; revision is supplied by
// its caller rather than invented here.
type SourceStamp struct {
	session string
	seq     uint64
	hasSeq  bool
}

func NewSourceStamp(session string, seq *uint64) (SourceStamp, error) {
	if session == "" || !utf8.ValidString(session) {
		return SourceStamp{}, ErrInvalidStateReport
	}
	stamp := SourceStamp{session: session}
	if seq != nil {
		stamp.seq, stamp.hasSeq = *seq, true
	}
	return stamp, nil
}

func (s SourceStamp) Components() (string, *uint64, error) {
	if s.session == "" || !utf8.ValidString(s.session) {
		return "", nil, ErrInvalidState
	}
	if s.hasSeq {
		seq := s.seq
		return s.session, &seq, nil
	}
	return s.session, nil, nil
}

// StateReport contains one accepted observation or quality report. ObservedAt
// is the source's capture time when supplied; ReceivedAt is the local admission
// time. Unknown and unavailable reports have no current value.
type StateReport struct {
	quality    Quality
	value      StateValue
	hasValue   bool
	observedAt *time.Time
	receivedAt time.Time
	source     SourceStamp
	revision   uint64
}

func NewStateReport(quality Quality, value *StateValue, observedAt *time.Time, receivedAt time.Time, source SourceStamp, revision uint64) (StateReport, error) {
	availability, _, _, err := quality.Components()
	if err != nil || receivedAt.IsZero() || revision == 0 {
		return StateReport{}, ErrInvalidStateReport
	}
	if _, _, err := source.Components(); err != nil {
		return StateReport{}, ErrInvalidStateReport
	}
	if (availability == AvailabilityAvailable) != (value != nil) {
		return StateReport{}, ErrInvalidStateReport
	}
	report := StateReport{quality: quality, receivedAt: receivedAt.UTC(), source: source, revision: revision}
	if value != nil {
		if _, err := value.Kind(); err != nil {
			return StateReport{}, ErrInvalidStateReport
		}
		report.value, report.hasValue = *value, true
	}
	if observedAt != nil {
		if observedAt.IsZero() {
			return StateReport{}, ErrInvalidStateReport
		}
		at := observedAt.UTC()
		report.observedAt = &at
	}
	return report, nil
}

// StateTimes keeps source capture, local receipt and the three transition
// clocks distinct. LastChanged tracks value/availability, LastUpdated also
// tracks quality, and LastReported tracks every accepted report.
type StateTimes struct {
	ObservedAt   *time.Time
	ReceivedAt   time.Time
	LastChanged  time.Time
	LastUpdated  time.Time
	LastReported time.Time
}

type ObservedState struct {
	entity       EntityID
	quality      Quality
	value        StateValue
	hasValue     bool
	lastKnown    StateValue
	hasLastKnown bool
	times        StateTimes
	source       SourceStamp
	revision     uint64
}

type StateTransition struct {
	ValueChanged bool
	Updated      bool
}

// ApplyStateReport builds an immutable snapshot. The caller serializes reports
// and allocates revisions; this function does not own a store or publish events.
func ApplyStateReport(entity EntityID, previous *ObservedState, report StateReport) (ObservedState, StateTransition, error) {
	if _, err := entity.Value(); err != nil {
		return ObservedState{}, StateTransition{}, ErrInvalidStateReport
	}
	value := &report.value
	if !report.hasValue {
		value = nil
	}
	if _, err := NewStateReport(report.quality, value, report.observedAt, report.receivedAt, report.source, report.revision); err != nil {
		return ObservedState{}, StateTransition{}, err
	}
	if previous != nil {
		if _, _, _, _, _, err := previous.Components(); err != nil || previous.entity != entity {
			return ObservedState{}, StateTransition{}, ErrInvalidState
		}
		if report.revision <= previous.revision {
			return ObservedState{}, StateTransition{}, ErrOutOfOrderReport
		}
	}
	result := ObservedState{
		entity: entity, quality: report.quality, value: report.value, hasValue: report.hasValue,
		times:  StateTimes{ObservedAt: cloneTime(report.observedAt), ReceivedAt: report.receivedAt, LastReported: report.receivedAt},
		source: report.source, revision: report.revision,
	}
	transition := StateTransition{ValueChanged: true, Updated: true}
	if previous != nil {
		transition.ValueChanged = previous.quality.availability != report.quality.availability ||
			previous.hasValue != report.hasValue || (report.hasValue && previous.value != report.value)
		transition.Updated = transition.ValueChanged || previous.quality != report.quality
		result.times.LastChanged = previous.times.LastChanged
		result.times.LastUpdated = previous.times.LastUpdated
		result.lastKnown, result.hasLastKnown = previous.lastKnown, previous.hasLastKnown
		// A quality-only report does not erase the capture time of the same
		// current value. A later explicit capture time replaces it.
		if report.observedAt == nil && report.hasValue && previous.hasValue && previous.value == report.value {
			result.times.ObservedAt = cloneTime(previous.times.ObservedAt)
		}
	}
	if transition.ValueChanged {
		result.times.LastChanged = report.receivedAt
	}
	if transition.Updated {
		result.times.LastUpdated = report.receivedAt
	}
	if report.hasValue {
		result.lastKnown, result.hasLastKnown = report.value, true
	}
	return result, transition, nil
}

func cloneTime(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	copy := *at
	return &copy
}

// NewObservedStateSnapshot admits a received projection whose transition clocks
// were already assigned by its producer. It does not infer missing reports or
// synthesize a transition from two snapshots. A previous snapshot is used only
// to retain the optional last-known display value when the wire omits it.
func NewObservedStateSnapshot(entity EntityID, quality Quality, value *StateValue, times StateTimes, source SourceStamp, revision uint64, previous *ObservedState) (ObservedState, error) {
	if _, err := entity.Value(); err != nil {
		return ObservedState{}, ErrInvalidState
	}
	availability, _, _, err := quality.Components()
	if err != nil || (availability == AvailabilityAvailable) != (value != nil) || revision == 0 ||
		times.ReceivedAt.IsZero() || times.LastChanged.IsZero() || times.LastUpdated.IsZero() || times.LastReported.IsZero() {
		return ObservedState{}, ErrInvalidState
	}
	if times.ObservedAt != nil && times.ObservedAt.IsZero() {
		return ObservedState{}, ErrInvalidState
	}
	if _, _, err := source.Components(); err != nil {
		return ObservedState{}, ErrInvalidState
	}
	if previous != nil {
		if _, _, _, _, _, err := previous.Components(); err != nil || previous.entity != entity {
			return ObservedState{}, ErrInvalidState
		}
	}
	result := ObservedState{
		entity: entity, quality: quality, times: StateTimes{
			ObservedAt: cloneTime(times.ObservedAt), ReceivedAt: times.ReceivedAt.UTC(),
			LastChanged: times.LastChanged.UTC(), LastUpdated: times.LastUpdated.UTC(), LastReported: times.LastReported.UTC(),
		}, source: source, revision: revision,
	}
	if result.times.ObservedAt != nil {
		at := result.times.ObservedAt.UTC()
		result.times.ObservedAt = &at
	}
	if previous != nil {
		result.lastKnown, result.hasLastKnown = previous.lastKnown, previous.hasLastKnown
	}
	if value != nil {
		if _, err := value.Kind(); err != nil {
			return ObservedState{}, ErrInvalidState
		}
		result.value, result.hasValue = *value, true
		result.lastKnown, result.hasLastKnown = *value, true
	}
	return result, nil
}

func (s ObservedState) Components() (EntityID, Quality, StateTimes, SourceStamp, uint64, error) {
	if _, err := s.entity.Value(); err != nil {
		return EntityID{}, Quality{}, StateTimes{}, SourceStamp{}, 0, ErrInvalidState
	}
	availability, _, _, err := s.quality.Components()
	if err != nil || (availability == AvailabilityAvailable) != s.hasValue || s.revision == 0 || s.times.ReceivedAt.IsZero() ||
		s.times.LastChanged.IsZero() || s.times.LastUpdated.IsZero() || s.times.LastReported.IsZero() {
		return EntityID{}, Quality{}, StateTimes{}, SourceStamp{}, 0, ErrInvalidState
	}
	if (s.times.ObservedAt != nil && s.times.ObservedAt.IsZero()) || (s.hasValue && !s.hasLastKnown) {
		return EntityID{}, Quality{}, StateTimes{}, SourceStamp{}, 0, ErrInvalidState
	}
	if s.hasValue {
		if _, err := s.value.Kind(); err != nil {
			return EntityID{}, Quality{}, StateTimes{}, SourceStamp{}, 0, ErrInvalidState
		}
	}
	if s.hasLastKnown {
		if _, err := s.lastKnown.Kind(); err != nil {
			return EntityID{}, Quality{}, StateTimes{}, SourceStamp{}, 0, ErrInvalidState
		}
	}
	if _, _, err := s.source.Components(); err != nil {
		return EntityID{}, Quality{}, StateTimes{}, SourceStamp{}, 0, ErrInvalidState
	}
	return s.entity, s.quality, StateTimes{
		ObservedAt: cloneTime(s.times.ObservedAt), ReceivedAt: s.times.ReceivedAt,
		LastChanged: s.times.LastChanged, LastUpdated: s.times.LastUpdated, LastReported: s.times.LastReported,
	}, s.source, s.revision, nil
}

func (s ObservedState) CurrentValue() (StateValue, bool) { return s.value, s.hasValue }

// DisplayValue may return a previous known value while unavailable. Consumers
// must inspect quality; this value is never the input to CriticalValue.
func (s ObservedState) DisplayValue() (StateValue, bool) { return s.lastKnown, s.hasLastKnown }

// CriticalValue admits only a current, fresh, directly observed value.
func (s ObservedState) CriticalValue() (StateValue, bool) {
	if s.hasValue && s.quality.availability == AvailabilityAvailable &&
		s.quality.freshness == FreshnessFresh && s.quality.provenance == ProvenanceObserved {
		return s.value, true
	}
	return StateValue{}, false
}
