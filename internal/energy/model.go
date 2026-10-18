package energy

import (
	"errors"
	"math"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrInvalidReport = errors.New("energy: invalid source report")

// Coverage describes intervals since this new counter's first bound, not a
// claim that any historical Home Assistant total was imported.
type Coverage uint8

const (
	CoverageUnspecified Coverage = iota
	CoverageContinuous
	CoverageDegraded
)

func (c Coverage) valid() bool { return c == CoverageContinuous || c == CoverageDegraded }

// Diagnostic is the last bounded reason for degraded coverage. No unbounded
// history of gaps or duplicate identities is retained in the service.
type Diagnostic uint8

const (
	DiagnosticUnspecified Diagnostic = iota
	DiagnosticNone
	DiagnosticRestart
	DiagnosticDeliveryLoss
	DiagnosticUnavailable
	DiagnosticStale
	DiagnosticMissingCapture
	DiagnosticInvalidTime
	DiagnosticExcessiveGap
	DiagnosticSourceChanged
	DiagnosticInvalidPower
	DiagnosticNegativePower
	DiagnosticLeaseExpired
)

func (d Diagnostic) valid() bool { return d >= DiagnosticNone && d <= DiagnosticLeaseExpired }

// ReportIdentity combines Host admission identity and the protocol identity
// when present. Time alone never identifies an observation.
type ReportIdentity struct {
	epoch    string
	cursor   uint64
	entity   domain.EntityID
	instance domain.IntegrationInstanceID
	source   domain.SourceStamp
	revision uint64
}

func NewReportIdentity(entry sequencer.Entry) (ReportIdentity, error) {
	entity, state, _, ok := entry.State()
	if !ok {
		return ReportIdentity{}, ErrInvalidReport
	}
	epoch, cursor, err := entry.Cursor().Components()
	if err != nil || cursor == 0 {
		return ReportIdentity{}, ErrInvalidReport
	}
	instance, source, _, ok := entry.Origin()
	if !ok {
		return ReportIdentity{}, ErrInvalidReport
	}
	if _, err := entity.Value(); err != nil {
		return ReportIdentity{}, ErrInvalidReport
	}
	if _, err := instance.Value(); err != nil {
		return ReportIdentity{}, ErrInvalidReport
	}
	if _, _, err := source.Components(); err != nil {
		return ReportIdentity{}, ErrInvalidReport
	}
	_, _, _, stateSource, revision, err := state.Components()
	if err != nil || revision == 0 || stateSource != source {
		return ReportIdentity{}, ErrInvalidReport
	}
	return ReportIdentity{epoch, cursor, entity, instance, source, revision}, nil
}

func (r ReportIdentity) valid() bool {
	_, err := sequencer.ParseCursor(r.epoch, r.cursor)
	if err != nil || r.cursor == 0 {
		return false
	}
	if _, err := r.entity.Value(); err != nil {
		return false
	}
	if _, err := r.instance.Value(); err != nil {
		return false
	}
	_, _, err = r.source.Components()
	return err == nil && r.revision != 0
}

func (r ReportIdentity) Cursor() (string, uint64, error) {
	if !r.valid() {
		return "", 0, ErrInvalidReport
	}
	return r.epoch, r.cursor, nil
}

func (r ReportIdentity) Source() (domain.EntityID, domain.IntegrationInstanceID, domain.SourceStamp, error) {
	if !r.valid() {
		return domain.EntityID{}, domain.IntegrationInstanceID{}, domain.SourceStamp{}, ErrInvalidReport
	}
	return r.entity, r.instance, r.source, nil
}

// StateRevision is Host's existing per-entity revision. It distinguishes
// reports even when the protocol supplies no sequence in SourceStamp.
func (r ReportIdentity) StateRevision() (uint64, error) {
	if !r.valid() {
		return 0, ErrInvalidReport
	}
	return r.revision, nil
}

type point struct {
	at                  time.Time
	admissionTick       time.Duration
	reportAdmissionTick time.Duration
	powerNum            string
	powerDen            string
	report              ReportIdentity
	tick                bool
}

// Lease is process-local proof supplied by the source owner. It is never
// persisted or reconstructed from a source timestamp after restart.
type Lease struct {
	report    ReportIdentity
	startTick time.Duration
	untilTick time.Duration
}

func NewLease(report ReportIdentity, admissionTick, validUntilTick time.Duration) (Lease, error) {
	if !report.valid() || admissionTick < 0 || validUntilTick <= admissionTick {
		return Lease{}, ErrInvalidReport
	}
	return Lease{report, admissionTick, validUntilTick}, nil
}

func (l Lease) validFor(report ReportIdentity, reportTick time.Duration) bool {
	return l.report.valid() && l.report == report && l.startTick == reportTick && l.untilTick > l.startTick
}

// PendingProjection is the exact quantized live value committed with the
// checkpoint. A failed Host publication reuses this value, never an integral.
type PendingProjection struct {
	revision   uint64
	generation uint64
	quantized  int64
	precision  int
	unit       domain.Unit
	semantic   domain.SemanticClass
	coverage   Coverage
	freshness  domain.Freshness
	createdAt  time.Time
}

func (p PendingProjection) Revision() uint64   { return p.revision }
func (p PendingProjection) Generation() uint64 { return p.generation }
func (p PendingProjection) Coverage() Coverage { return p.coverage }

// Observation is reconstructed solely from committed projection fields.
// All derived energy remains estimated, even when source power was observed.
func (p PendingProjection) Observation() (integrations.StateObservation, error) {
	if p.revision == 0 || p.generation == 0 || p.quantized < 0 || p.quantized > maxQuantized || !p.coverage.valid() ||
		p.precision < 0 || p.precision > 9 || p.createdAt.IsZero() ||
		(p.freshness != domain.FreshnessFresh && p.freshness != domain.FreshnessStale) {
		return integrations.StateObservation{}, ErrInvalidCheckpoint
	}
	factor := math.Pow10(p.precision)
	measurement, err := domain.NewMeasurement(float64(p.quantized)/factor, p.unit, p.precision, p.semantic, domain.AggregationSum)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, p.freshness, domain.ProvenanceEstimated)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	return integrations.NewStateObservation("energy", quality, &value, nil, p.createdAt, nil)
}

// Checkpoint is immutable outside this package. Fractions are canonical
// decimal strings so a caller cannot mutate math/big values through a copy.
type Checkpoint struct {
	profile          Profile
	revision         uint64
	generation       uint64
	totalNum         string
	totalDen         string
	captureWatermark time.Time
	previous         point
	hasPrevious      bool
	lastReport       ReportIdentity
	hasReport        bool
	fenceEpoch       string
	fenceCursor      uint64
	coverage         Coverage
	diagnostic       Diagnostic
	pending          PendingProjection
	hasPending       bool
	hasEnergy        bool
}

func newCheckpoint(profile Profile) Checkpoint {
	return Checkpoint{profile: profile, generation: 1, totalNum: "0", totalDen: "1", coverage: CoverageContinuous, diagnostic: DiagnosticNone}
}

func (c Checkpoint) Profile() Profile                   { return c.profile }
func (c Checkpoint) Revision() uint64                   { return c.revision }
func (c Checkpoint) Generation() uint64                 { return c.generation }
func (c Checkpoint) Coverage() Coverage                 { return c.coverage }
func (c Checkpoint) Diagnostic() Diagnostic             { return c.diagnostic }
func (c Checkpoint) Pending() (PendingProjection, bool) { return c.pending, c.hasPending }
func (c Checkpoint) TotalFraction() (string, string, error) {
	if !c.valid() {
		return "", "", ErrInvalidCheckpoint
	}
	return c.totalNum, c.totalDen, nil
}
