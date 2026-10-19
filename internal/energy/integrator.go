package energy

import (
	"errors"
	"math"
	"math/big"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

var (
	ErrInvalidCheckpoint = errors.New("energy: invalid checkpoint")
	ErrNumericBounds     = errors.New("energy: numeric bound exceeded")
)

const maxFractionDigits = 512
const maxQuantized = 1 << 52

func rational(num, den string) (*big.Rat, error) {
	if len(num) == 0 || len(den) == 0 || len(num) > maxFractionDigits || len(den) > maxFractionDigits {
		return nil, ErrNumericBounds
	}
	n, ok := new(big.Int).SetString(num, 10)
	if !ok {
		return nil, ErrInvalidCheckpoint
	}
	d, ok := new(big.Int).SetString(den, 10)
	if !ok || d.Sign() <= 0 {
		return nil, ErrInvalidCheckpoint
	}
	return new(big.Rat).SetFrac(n, d), nil
}

func fraction(r *big.Rat) (string, string, error) {
	if r == nil {
		return "", "", ErrNumericBounds
	}
	n, d := r.Num().String(), r.Denom().String()
	if len(n) > maxFractionDigits || len(d) > maxFractionDigits {
		return "", "", ErrNumericBounds
	}
	return n, d, nil
}

func powerOfTen(precision int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
}

// roundHalfEven rounds a nonnegative rational to an integer without floating
// point loss. Consumption profiles reject negative power before this point.
func roundHalfEven(r *big.Rat) (*big.Int, error) {
	if r == nil || r.Sign() < 0 {
		return nil, ErrNumericBounds
	}
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	comparison := new(big.Int).Lsh(rem, 1).Cmp(r.Denom())
	if comparison > 0 || (comparison == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	return q, nil
}

func quantize(profile Profile, total *big.Rat) (int64, error) {
	if total == nil || total.Sign() < 0 || total.Cmp(big.NewRat(1_000_000_000_000_000, 1)) > 0 {
		return 0, ErrNumericBounds
	}
	factor := powerOfTen(profile.precision)
	symbol, _ := profile.unit.Symbol()
	if symbol == "Wh" {
		factor.Mul(factor, big.NewInt(1000))
	}
	scaled := new(big.Rat).Mul(total, new(big.Rat).SetInt(factor))
	if scaled.Cmp(new(big.Rat).SetInt64(maxQuantized)) > 0 {
		return 0, ErrNumericBounds
	}
	quantized, err := roundHalfEven(scaled)
	if err != nil || !quantized.IsInt64() || quantized.Int64() > maxQuantized {
		return 0, ErrNumericBounds
	}
	// The public domain Measurement holds float64. Refuse a projection whose
	// decimal quantum would change after that conversion.
	projected := float64(quantized.Int64()) / float64(factor.Int64())
	back := new(big.Rat).SetFloat64(projected)
	if back == nil {
		return 0, ErrNumericBounds
	}
	back.Mul(back, new(big.Rat).SetInt(factor))
	check, err := roundHalfEven(back)
	if err != nil || check.Cmp(quantized) != 0 {
		return 0, ErrNumericBounds
	}
	return quantized.Int64(), nil
}

func sourceSequenceDuplicate(previous, current ReportIdentity) bool {
	if previous.entity != current.entity || previous.instance != current.instance {
		return false
	}
	oldSession, oldSequence, oldErr := previous.source.Components()
	newSession, newSequence, newErr := current.source.Components()
	return oldErr == nil && newErr == nil && oldSession == newSession &&
		oldSequence != nil && newSequence != nil && *newSequence <= *oldSequence
}

func sameSource(previous, current ReportIdentity) bool {
	if previous.entity != current.entity || previous.instance != current.instance {
		return false
	}
	oldSession, _, oldErr := previous.source.Components()
	newSession, _, newErr := current.source.Components()
	return oldErr == nil && newErr == nil && oldSession == newSession
}

func markGap(c *Checkpoint, reason Diagnostic, at time.Time) error {
	c.previous = point{}
	c.hasPrevious = false
	c.coverage = CoverageDegraded
	c.diagnostic = reason
	return setPendingMissing(c, at)
}

// MissingSource commits the initial or restored absence without changing the
// durable total. It is also the restart bound when there is no live report.
func MissingSource(original Checkpoint, at time.Time) (Checkpoint, error) {
	if !original.valid() || original.revision >= 1<<63-1 || at.IsZero() {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	c := original
	c.revision++
	if err := markGap(&c, DiagnosticUnavailable, at); err != nil {
		return Checkpoint{}, err
	}
	if !original.hasEnergy && !original.hasReport {
		// Coverage begins with the first actual power bound. Initial absence
		// is observable but cannot create a historical interval gap.
		c.coverage = CoverageContinuous
		c.pending.coverage = CoverageContinuous
	}
	return c, nil
}

func setPendingMissing(c *Checkpoint, at time.Time) error {
	if at.IsZero() {
		return ErrInvalidCheckpoint
	}
	c.pending = PendingProjection{kind: PendingMissingSource, revision: c.revision, generation: c.generation,
		precision: c.profile.precision, unit: c.profile.unit, semantic: c.profile.semantic,
		coverage: c.coverage, freshness: domain.FreshnessStale, createdAt: at.UTC()}
	c.currentKind = PendingMissingSource
	c.hasPending = true
	return nil
}

func setPending(c *Checkpoint, freshness domain.Freshness, at time.Time) error {
	if !c.hasEnergy || at.IsZero() {
		return nil
	}
	total, err := rational(c.totalNum, c.totalDen)
	if err != nil {
		return err
	}
	q, err := quantize(c.profile, total)
	if err != nil {
		return err
	}
	c.pending = PendingProjection{kind: PendingValue, revision: c.revision, generation: c.generation, quantized: q,
		precision: c.profile.precision, unit: c.profile.unit, semantic: c.profile.semantic,
		coverage: c.coverage, freshness: freshness, createdAt: at.UTC()}
	c.currentKind = PendingValue
	c.hasPending = true
	return nil
}

func validPower(state domain.ObservedState) (*big.Rat, Diagnostic) {
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		return nil, DiagnosticInvalidPower
	}
	availability, freshness, provenance, err := quality.Components()
	if err != nil || availability != domain.AvailabilityAvailable {
		return nil, DiagnosticUnavailable
	}
	if freshness != domain.FreshnessFresh {
		return nil, DiagnosticStale
	}
	if provenance != domain.ProvenanceObserved && provenance != domain.ProvenanceEstimated {
		return nil, DiagnosticInvalidPower
	}
	value, ok := state.CurrentValue()
	if !ok {
		return nil, DiagnosticUnavailable
	}
	measurement, ok := value.Measurement()
	if !ok {
		return nil, DiagnosticInvalidPower
	}
	components, err := measurement.Components()
	if err != nil || components.Dimension != domain.DimensionPower {
		return nil, DiagnosticInvalidPower
	}
	watt, err := domain.NewUnitCatalog().Parse("W")
	if err != nil {
		return nil, DiagnosticInvalidPower
	}
	converted, err := measurement.Convert(watt)
	if err != nil {
		return nil, DiagnosticInvalidPower
	}
	parts, err := converted.Components()
	if err != nil || math.IsNaN(parts.Value) || math.IsInf(parts.Value, 0) || math.Abs(parts.Value) > 1_000_000_000 {
		return nil, DiagnosticInvalidPower
	}
	if parts.Value < 0 {
		return nil, DiagnosticNegativePower
	}
	power := new(big.Rat).SetFloat64(parts.Value)
	if power == nil {
		return nil, DiagnosticInvalidPower
	}
	return power, DiagnosticNone
}

func pointTime(profile Profile, times domain.StateTimes) (time.Time, Diagnostic) {
	switch profile.basis {
	case TimeBasisCaptureRequired:
		if times.ObservedAt == nil {
			return time.Time{}, DiagnosticMissingCapture
		}
		return *times.ObservedAt, DiagnosticNone
	case TimeBasisReceipt:
		if times.ReceivedAt.IsZero() {
			return time.Time{}, DiagnosticInvalidTime
		}
		return times.ReceivedAt, DiagnosticNone
	case TimeBasisUnspecified:
		return time.Time{}, DiagnosticInvalidTime
	default:
		return time.Time{}, DiagnosticInvalidTime
	}
}

func addInterval(c *Checkpoint, previous point, currentPower *big.Rat, delta time.Duration) error {
	previousPower, err := rational(previous.powerNum, previous.powerDen)
	if err != nil {
		return err
	}
	if delta <= 0 || delta > c.profile.maxGap {
		return ErrInvalidReport
	}
	used := new(big.Rat).Set(previousPower)
	if c.profile.method == MethodTrapezoidal {
		used.Add(used, currentPower)
		used.Quo(used, big.NewRat(2, 1))
	}
	// W * ns / (1000 W/kW * 3600 s/h * 10^9 ns/s) = kWh.
	contribution := new(big.Rat).Mul(used, new(big.Rat).SetInt64(delta.Nanoseconds()))
	contribution.Quo(contribution, big.NewRat(3_600_000_000_000_000, 1))
	total, err := rational(c.totalNum, c.totalDen)
	if err != nil {
		return err
	}
	total.Add(total, contribution)
	if _, err := quantize(c.profile, total); err != nil {
		return err
	}
	c.totalNum, c.totalDen, err = fraction(total)
	if err != nil {
		return err
	}
	c.hasEnergy = true
	if c.coverage == CoverageContinuous {
		c.diagnostic = DiagnosticNone
	}
	return nil
}

// AdvanceReport applies one admitted Host report. A duplicate report returns
// the original checkpoint and false. The caller commits the returned value
// atomically with its pending projection before publishing through Host.
func AdvanceReport(profile Profile, original Checkpoint, entry sequencer.Entry, activeEpoch string) (Checkpoint, bool, error) {
	if !profile.Valid() {
		return Checkpoint{}, false, ErrInvalidProfile
	}
	entity, state, _, ok := entry.State()
	if !ok || entity != profile.source {
		return Checkpoint{}, false, ErrInvalidReport
	}
	id, err := NewReportIdentity(entry)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if activeEpoch == "" || id.epoch != activeEpoch {
		return Checkpoint{}, false, ErrInvalidReport
	}
	if !original.valid() || original.profile != profile {
		return Checkpoint{}, false, ErrInvalidCheckpoint
	}
	c := original
	if c.revision == 0 {
		c = newCheckpoint(profile)
	} else if c.profile != profile || c.generation == 0 || c.revision >= 1<<63-1 {
		return Checkpoint{}, false, ErrInvalidCheckpoint
	}
	if c.hasReport {
		if (id.epoch == c.lastReport.epoch && id.cursor <= c.lastReport.cursor) || sourceSequenceDuplicate(c.lastReport, id) {
			return original, false, nil
		}
	}
	if id.epoch == c.fenceEpoch && id.cursor <= c.fenceCursor {
		return original, false, nil
	}
	c.revision++
	if c.hasReport && id.epoch != c.lastReport.epoch {
		if err := markGap(&c, DiagnosticRestart, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
	}
	c.lastReport, c.hasReport = id, true
	_, _, times, source, _, err := state.Components()
	if err != nil || source != id.source {
		return Checkpoint{}, false, ErrInvalidReport
	}
	power, diagnostic := validPower(state)
	if diagnostic != DiagnosticNone {
		if err := markGap(&c, diagnostic, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
		return c, true, nil
	}
	at, diagnostic := pointTime(profile, times)
	if diagnostic != DiagnosticNone {
		if err := markGap(&c, diagnostic, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
		return c, true, nil
	}
	// Entry.State materializes an ObservedState. Host may carry an earlier
	// capture through identical reports without exposing capture presence, so
	// the last admitted capture remains a durable watermark across gaps.
	if profile.basis == TimeBasisCaptureRequired && !c.captureWatermark.IsZero() && !at.After(c.captureWatermark) {
		if err := markGap(&c, DiagnosticInvalidTime, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
		return c, true, nil
	}
	previous := c.previous
	hasPrevious := c.hasPrevious
	if hasPrevious && !sameSource(previous.report, id) {
		if err := markGap(&c, DiagnosticSourceChanged, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
		hasPrevious = false
	}
	if hasPrevious {
		delta := at.Sub(previous.at)
		if profile.basis == TimeBasisReceipt {
			if previous.report.epoch != id.epoch {
				delta = 0
			} else {
				delta = entry.AdmissionTick() - previous.admissionTick
			}
		}
		switch {
		case delta == 0 && previous.tick:
			// A source report racing a scheduled left tick replaces that bound.
		case delta <= 0:
			if err := markGap(&c, DiagnosticInvalidTime, entry.ReceivedAt()); err != nil {
				return Checkpoint{}, false, err
			}
			return c, true, nil
		case delta > profile.maxGap:
			if err := markGap(&c, DiagnosticExcessiveGap, entry.ReceivedAt()); err != nil {
				return Checkpoint{}, false, err
			}
		default:
			if err := addInterval(&c, previous, power, delta); err != nil {
				return Checkpoint{}, false, err
			}
			if err := setPending(&c, domain.FreshnessFresh, entry.ReceivedAt()); err != nil {
				return Checkpoint{}, false, err
			}
		}
	}
	c.previous = point{at: at.UTC(), admissionTick: entry.AdmissionTick(), reportAdmissionTick: entry.AdmissionTick(), report: id}
	if profile.basis == TimeBasisCaptureRequired {
		c.captureWatermark = at.UTC()
	}
	c.previous.powerNum, c.previous.powerDen, err = fraction(power)
	if err != nil {
		return Checkpoint{}, false, err
	}
	c.hasPrevious = true
	if c.currentKind == PendingMissingSource && (!c.hasPending || c.pending.revision != c.revision) {
		// A newly recovered power report is only the first bound of a new
		// interval. Keep the old total durable but leave current energy absent
		// until a second admitted report can integrate a real interval.
		if err := setPendingMissing(&c, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
	} else if c.hasEnergy && (!c.hasPending || c.pending.revision != c.revision) {
		if err := setPending(&c, domain.FreshnessStale, entry.ReceivedAt()); err != nil {
			return Checkpoint{}, false, err
		}
	}
	return c, true, nil
}

// AdvanceTick subdivides a left-method interval only while the source owner
// supplies a live process-local lease. The source's report identity and
// freshness are unchanged. No synthetic admission sequence is allocated.
func AdvanceTick(profile Profile, original Checkpoint, lease Lease, nowTick time.Duration) (Checkpoint, bool, error) {
	if !profile.Valid() || profile.method != MethodLeft || profile.tickInterval == 0 ||
		original.revision == 0 || original.revision >= 1<<63-1 || !original.valid() || original.profile != profile ||
		!lease.report.valid() || nowTick < lease.startTick {
		return Checkpoint{}, false, ErrInvalidReport
	}
	if !original.hasPrevious || original.previous.report != lease.report {
		return original, false, nil
	}
	if !lease.validFor(original.previous.report, original.previous.reportAdmissionTick) ||
		(original.previous.admissionTick == lease.startTick && original.previous.tick) {
		return Checkpoint{}, false, ErrInvalidReport
	}
	if nowTick < original.previous.admissionTick {
		return Checkpoint{}, false, ErrInvalidReport
	}
	delta := nowTick - original.previous.admissionTick
	wallTarget := original.previous.at.Add(delta)
	if nowTick > lease.untilTick {
		c, err := BreakContinuity(original, DiagnosticLeaseExpired, wallTarget)
		return c, err == nil, err
	}
	if delta < profile.tickInterval {
		return original, false, nil
	}
	if delta > profile.maxGap {
		c, err := BreakContinuity(original, DiagnosticExcessiveGap, wallTarget)
		return c, err == nil, err
	}
	c := original
	c.revision++
	power, err := rational(c.previous.powerNum, c.previous.powerDen)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if err := addInterval(&c, c.previous, power, delta); err != nil {
		return Checkpoint{}, false, err
	}
	c.previous.at = wallTarget.UTC()
	c.previous.admissionTick = nowTick
	c.previous.tick = true
	if err := setPending(&c, domain.FreshnessFresh, wallTarget); err != nil {
		return Checkpoint{}, false, err
	}
	return c, true, nil
}

// BreakContinuity is used on restart, recorder loss, and cancellation. It
// never treats a snapshot or a restored value as a fresh source sample.
func BreakContinuity(original Checkpoint, reason Diagnostic, at time.Time) (Checkpoint, error) {
	if original.revision == 0 || original.revision >= 1<<63-1 || !original.valid() || !reason.valid() || reason == DiagnosticNone || at.IsZero() {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	c := original
	c.revision++
	if err := markGap(&c, reason, at); err != nil {
		return Checkpoint{}, err
	}
	return c, nil
}

// Reset is a software-only, generation-fenced reset. The service supplies a
// current admission cursor as a fence; no device counter is written.
func Reset(original Checkpoint, fence sequencer.Cursor, at time.Time) (Checkpoint, error) {
	if original.revision == 0 || !original.valid() || original.revision >= 1<<63-1 || original.generation >= 1<<63-1 || at.IsZero() {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	epoch, cursor, err := fence.Components()
	if err != nil || (original.hasReport && original.lastReport.epoch == epoch && cursor < original.lastReport.cursor) {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	c := newCheckpoint(original.profile)
	c.revision = original.revision + 1
	c.generation = original.generation + 1
	c.coverage, c.diagnostic = CoverageDegraded, DiagnosticRestart
	c.lastReport, c.hasReport = original.lastReport, original.hasReport
	c.captureWatermark = original.captureWatermark
	c.fenceEpoch, c.fenceCursor = epoch, cursor
	c.hasEnergy = true
	if err := setPending(&c, domain.FreshnessStale, at); err != nil {
		return Checkpoint{}, err
	}
	return c, nil
}
