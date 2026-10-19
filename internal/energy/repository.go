package energy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

var (
	ErrConflict       = errors.New("energy: checkpoint changed concurrently")
	ErrCommitUnknown  = errors.New("energy: checkpoint commit outcome unknown")
	ErrProfileChanged = errors.New("energy: explicit profile transition required")
	ErrInvalidContext = errors.New("energy: invalid context")
)

// Repository is the sole durable authority for a counter and its pending Host
// publication. It is called only by the worker, never by the Host sink.
type transactionStore interface {
	InTransaction(context.Context, func(pgx.Tx) error) error
	InTransactionOutcome(context.Context, func(pgx.Tx) error) (storage.TransactionOutcome, error)
}
type Repository struct{ store transactionStore }

func NewRepository(store *storage.Store) (*Repository, error) {
	if store == nil {
		return nil, ErrInvalidCheckpoint
	}
	return &Repository{store: store}, nil
}

type profileWire struct {
	Name      string               `json:"name"`
	Version   uint16               `json:"version"`
	Source    string               `json:"source"`
	Output    string               `json:"output"`
	Method    Method               `json:"method"`
	Basis     TimeBasis            `json:"basis"`
	Unit      string               `json:"unit"`
	Precision int                  `json:"precision"`
	Semantic  domain.SemanticClass `json:"semantic"`
	MaxGapNS  int64                `json:"max_gap_ns"`
	TickNS    int64                `json:"tick_ns"`
}
type reportWire struct {
	Epoch    string  `json:"epoch"`
	Cursor   uint64  `json:"cursor"`
	Entity   string  `json:"entity"`
	Instance string  `json:"instance"`
	Session  string  `json:"session"`
	Sequence *uint64 `json:"sequence"`
	Revision uint64  `json:"state_revision"`
}
type pointWire struct {
	At                    time.Time  `json:"at"`
	AdmissionTickNS       int64      `json:"admission_tick_ns"`
	ReportAdmissionTickNS int64      `json:"report_admission_tick_ns"`
	PowerNum              string     `json:"power_num"`
	PowerDen              string     `json:"power_den"`
	Report                reportWire `json:"report"`
	Tick                  bool       `json:"tick"`
}
type pendingWire struct {
	Kind       *PendingKind         `json:"kind,omitempty"`
	Revision   uint64               `json:"revision"`
	Generation uint64               `json:"generation"`
	Quantized  int64                `json:"quantized"`
	Precision  int                  `json:"precision"`
	Unit       string               `json:"unit"`
	Semantic   domain.SemanticClass `json:"semantic"`
	Coverage   Coverage             `json:"coverage"`
	Freshness  domain.Freshness     `json:"freshness"`
	CreatedAt  time.Time            `json:"created_at"`
}
type checkpointWire struct {
	Profile          profileWire  `json:"profile"`
	Revision         uint64       `json:"revision"`
	Generation       uint64       `json:"generation"`
	TotalNum         string       `json:"total_num"`
	TotalDen         string       `json:"total_den"`
	CaptureWatermark time.Time    `json:"capture_watermark"`
	Previous         *pointWire   `json:"previous,omitempty"`
	LastReport       *reportWire  `json:"last_report,omitempty"`
	FenceEpoch       string       `json:"fence_epoch,omitempty"`
	FenceCursor      uint64       `json:"fence_cursor,omitempty"`
	Coverage         Coverage     `json:"coverage"`
	Diagnostic       Diagnostic   `json:"diagnostic"`
	CurrentKind      *PendingKind `json:"current_kind,omitempty"`
	Pending          *pendingWire `json:"pending,omitempty"`
	HasEnergy        bool         `json:"has_energy"`
}

func profileToWire(p Profile) profileWire {
	source, _ := p.source.Value()
	output, _ := p.output.Value()
	unit, _ := p.unit.Symbol()
	return profileWire{p.name, p.version, source, output, p.method, p.basis, unit, p.precision, p.semantic, int64(p.maxGap), int64(p.tickInterval)}
}
func (w profileWire) profile() (Profile, error) {
	source, e := domain.ParseEntityID(w.Source)
	if e != nil {
		return Profile{}, ErrInvalidCheckpoint
	}
	output, e := domain.ParseEntityID(w.Output)
	if e != nil {
		return Profile{}, ErrInvalidCheckpoint
	}
	unit, e := domain.NewUnitCatalog().Parse(w.Unit)
	if e != nil {
		return Profile{}, ErrInvalidCheckpoint
	}
	p, e := NewProfile(w.Name, w.Version, source, output, w.Method, w.Basis, unit, w.Precision, w.Semantic, time.Duration(w.MaxGapNS), time.Duration(w.TickNS))
	if e != nil {
		return Profile{}, ErrInvalidCheckpoint
	}
	return p, nil
}
func reportToWire(r ReportIdentity) reportWire {
	entity, _ := r.entity.Value()
	instance, _ := r.instance.Value()
	session, seq, _ := r.source.Components()
	return reportWire{r.epoch, r.cursor, entity, instance, session, seq, r.revision}
}
func (w reportWire) report() (ReportIdentity, error) {
	entity, e := domain.ParseEntityID(w.Entity)
	if e != nil {
		return ReportIdentity{}, ErrInvalidCheckpoint
	}
	instance, e := domain.ParseIntegrationInstanceID(w.Instance)
	if e != nil {
		return ReportIdentity{}, ErrInvalidCheckpoint
	}
	stamp, e := domain.NewSourceStamp(w.Session, w.Sequence)
	if e != nil {
		return ReportIdentity{}, ErrInvalidCheckpoint
	}
	r := ReportIdentity{w.Epoch, w.Cursor, entity, instance, stamp, w.Revision}
	if !r.valid() {
		return ReportIdentity{}, ErrInvalidCheckpoint
	}
	return r, nil
}
func checkpointToWire(c Checkpoint) checkpointWire {
	w := checkpointWire{Profile: profileToWire(c.profile), Revision: c.revision, Generation: c.generation, TotalNum: c.totalNum, TotalDen: c.totalDen, CaptureWatermark: c.captureWatermark, FenceEpoch: c.fenceEpoch, FenceCursor: c.fenceCursor, Coverage: c.coverage, Diagnostic: c.diagnostic, HasEnergy: c.hasEnergy}
	if c.currentKind.valid() {
		kind := c.currentKind
		w.CurrentKind = &kind
	}
	if c.hasPrevious {
		w.Previous = &pointWire{c.previous.at, c.previous.admissionTick.Nanoseconds(), c.previous.reportAdmissionTick.Nanoseconds(), c.previous.powerNum, c.previous.powerDen, reportToWire(c.previous.report), c.previous.tick}
	}
	if c.hasReport {
		r := reportToWire(c.lastReport)
		w.LastReport = &r
	}
	if c.hasPending {
		unit, _ := c.pending.unit.Symbol()
		kind := c.pending.kind
		w.Pending = &pendingWire{&kind, c.pending.revision, c.pending.generation, c.pending.quantized, c.pending.precision, unit, c.pending.semantic, c.pending.coverage, c.pending.freshness, c.pending.createdAt}
	}
	return w
}
func (w checkpointWire) checkpoint() (Checkpoint, error) {
	p, e := w.Profile.profile()
	if e != nil {
		return Checkpoint{}, e
	}
	c := Checkpoint{profile: p, revision: w.Revision, generation: w.Generation, totalNum: w.TotalNum, totalDen: w.TotalDen, captureWatermark: w.CaptureWatermark, fenceEpoch: w.FenceEpoch, fenceCursor: w.FenceCursor, coverage: w.Coverage, diagnostic: w.Diagnostic, hasEnergy: w.HasEnergy}
	if w.LastReport != nil {
		c.lastReport, e = w.LastReport.report()
		if e != nil {
			return Checkpoint{}, e
		}
		c.hasReport = true
	}
	if w.Previous != nil {
		r, e := w.Previous.Report.report()
		if e != nil {
			return Checkpoint{}, e
		}
		c.previous = point{at: w.Previous.At, admissionTick: time.Duration(w.Previous.AdmissionTickNS), reportAdmissionTick: time.Duration(w.Previous.ReportAdmissionTickNS), powerNum: w.Previous.PowerNum, powerDen: w.Previous.PowerDen, report: r, tick: w.Previous.Tick}
		c.hasPrevious = true
	}
	if w.Pending != nil {
		unit, e := domain.NewUnitCatalog().Parse(w.Pending.Unit)
		if e != nil {
			return Checkpoint{}, ErrInvalidCheckpoint
		}
		kind := PendingValue // v9 rows predate the explicit pending variant.
		if w.Pending.Kind != nil {
			kind = *w.Pending.Kind
			if !kind.valid() {
				return Checkpoint{}, ErrInvalidCheckpoint
			}
		}
		c.pending = PendingProjection{kind, w.Pending.Revision, w.Pending.Generation, w.Pending.Quantized, w.Pending.Precision, unit, w.Pending.Semantic, w.Pending.Coverage, w.Pending.Freshness, w.Pending.CreatedAt}
		c.hasPending = true
	}
	// Older checkpoints had no durable current-presence field. Their pending
	// value was the only published variant; an acknowledged old total is
	// value-bearing. Distinguish omitted legacy field from explicit zero.
	if w.CurrentKind != nil {
		c.currentKind = *w.CurrentKind
		if !c.currentKind.valid() {
			return Checkpoint{}, ErrInvalidCheckpoint
		}
	} else if c.hasPending {
		c.currentKind = c.pending.kind
	} else if c.hasEnergy {
		c.currentKind = PendingValue
	}
	if !c.valid() {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	return c, nil
}
func (c Checkpoint) valid() bool {
	if !c.profile.Valid() || c.generation == 0 || c.revision > 1<<63-1 || c.generation > 1<<63-1 || !c.coverage.valid() || !c.diagnostic.valid() {
		return false
	}
	if c.revision == 0 && (c.generation != 1 || c.hasReport || c.hasPrevious || c.hasPending || c.hasEnergy || !c.captureWatermark.IsZero() || c.fenceEpoch != "" || c.fenceCursor != 0) {
		return false
	}
	if c.currentKind != PendingUnspecified && !c.currentKind.valid() ||
		c.revision == 0 && c.currentKind != PendingUnspecified ||
		c.currentKind == PendingValue && !c.hasEnergy ||
		c.hasPending && c.currentKind != c.pending.kind {
		return false
	}
	if c.profile.basis == TimeBasisReceipt && !c.captureWatermark.IsZero() ||
		c.profile.basis == TimeBasisCaptureRequired && c.hasPrevious && !c.previous.at.Equal(c.captureWatermark) {
		return false
	}
	total, e := rational(c.totalNum, c.totalDen)
	if e != nil || total.Sign() < 0 {
		return false
	}
	if n, d, e := fraction(total); e != nil || n != c.totalNum || d != c.totalDen {
		return false
	}
	if _, e := quantize(c.profile, total); e != nil {
		return false
	}
	if !c.hasEnergy && total.Sign() != 0 {
		return false
	}
	if c.hasReport && (!c.lastReport.valid() || c.lastReport.entity != c.profile.source) {
		return false
	}
	if c.fenceEpoch != "" {
		if _, e := sequencer.ParseCursor(c.fenceEpoch, c.fenceCursor); e != nil {
			return false
		}
	} else if c.fenceCursor != 0 {
		return false
	}
	if c.hasPrevious {
		if !c.hasReport || !c.previous.report.valid() || c.previous.report != c.lastReport || c.previous.at.IsZero() || c.previous.admissionTick < 0 || c.previous.reportAdmissionTick < 0 || c.previous.admissionTick < c.previous.reportAdmissionTick {
			return false
		}
		if (!c.previous.tick && c.previous.admissionTick != c.previous.reportAdmissionTick) || (c.previous.tick && c.previous.admissionTick == c.previous.reportAdmissionTick) {
			return false
		}
		power, e := rational(c.previous.powerNum, c.previous.powerDen)
		if e != nil || power.Sign() < 0 {
			return false
		}
	}
	if c.hasPending {
		q, e := quantize(c.profile, total)
		if e != nil || !c.pending.kind.valid() || c.pending.revision != c.revision || c.pending.generation != c.generation || c.pending.precision != c.profile.precision || c.pending.unit != c.profile.unit || c.pending.semantic != c.profile.semantic || !c.pending.coverage.valid() || c.pending.coverage != c.coverage || (c.pending.freshness != domain.FreshnessFresh && c.pending.freshness != domain.FreshnessStale) || c.pending.createdAt.IsZero() ||
			(c.pending.kind == PendingValue && (!c.hasEnergy || c.pending.quantized != q)) ||
			(c.pending.kind == PendingMissingSource && (c.pending.quantized != 0 || c.pending.freshness != domain.FreshnessStale || c.coverage == CoverageContinuous && c.hasEnergy)) {
			return false
		}
	}
	return true
}
func encode(c Checkpoint) ([]byte, error) {
	if !c.valid() {
		return nil, ErrInvalidCheckpoint
	}
	b, e := json.Marshal(checkpointToWire(c))
	if e != nil || len(b) > 16000 {
		return nil, ErrInvalidCheckpoint
	}
	return b, nil
}
func decode(b []byte) (Checkpoint, error) {
	if len(b) == 0 || len(b) > 16384 {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	var w checkpointWire
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(&w); e != nil {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	return w.checkpoint()
}
func (r *Repository) Load(ctx context.Context, p Profile) (Checkpoint, error) {
	if ctx == nil {
		return Checkpoint{}, ErrInvalidContext
	}
	if r == nil || r.store == nil || !p.Valid() {
		return Checkpoint{}, ErrInvalidProfile
	}
	key, _ := p.output.Value()
	var raw []byte
	var rev, gen int64
	e := r.store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revision,generation,state FROM public.energy_checkpoints WHERE output_entity_id=$1`, key).Scan(&rev, &gen, &raw)
	})
	if errors.Is(e, pgx.ErrNoRows) {
		return newCheckpoint(p), nil
	}
	if e != nil {
		return Checkpoint{}, e
	}
	c, e := decode(raw)
	if e != nil {
		return Checkpoint{}, e
	}
	if c.revision != uint64(rev) || c.generation != uint64(gen) {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	if c.profile != p {
		return Checkpoint{}, ErrProfileChanged
	}
	return c, nil
}

// Save uses the previous revision as a compare-and-swap. A commit-unknown
// result is read back before any publication; an unreadable result stays inert.
func (r *Repository) Save(ctx context.Context, previous, next Checkpoint) (Checkpoint, error) {
	if ctx == nil {
		return Checkpoint{}, ErrInvalidContext
	}
	if r == nil || r.store == nil || !previous.valid() || !next.valid() || previous.profile != next.profile || next.revision != previous.revision+1 || next.generation < previous.generation || next.generation > previous.generation+1 {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	blob, e := encode(next)
	if e != nil {
		return Checkpoint{}, e
	}
	key, _ := next.profile.output.Value()
	outcome, e := r.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if previous.revision == 0 {
			command, e := tx.Exec(ctx, `INSERT INTO public.energy_checkpoints(output_entity_id,revision,generation,state) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, key, int64(next.revision), int64(next.generation), blob)
			if e != nil {
				return e
			}
			if command.RowsAffected() != 1 {
				return ErrConflict
			}
			return nil
		}
		command, e := tx.Exec(ctx, `UPDATE public.energy_checkpoints SET revision=$2,generation=$3,state=$4 WHERE output_entity_id=$1 AND revision=$5 AND generation=$6`, key, int64(next.revision), int64(next.generation), blob, int64(previous.revision), int64(previous.generation))
		if e != nil {
			return e
		}
		if command.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
	switch outcome {
	case storage.TransactionCommitted:
		return next, nil
	case storage.TransactionNotCommitted:
		if errors.Is(e, ErrConflict) {
			return Checkpoint{}, ErrConflict
		}
		return Checkpoint{}, e
	case storage.TransactionIndeterminate:
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		read, readErr := r.Load(readCtx, next.profile)
		if readErr != nil {
			return Checkpoint{}, fmt.Errorf("%w: %v", ErrCommitUnknown, readErr)
		}
		readBlob, readErr := encode(read)
		if readErr == nil && bytes.Equal(readBlob, blob) {
			return read, nil
		}
		return Checkpoint{}, ErrConflict
	default:
		return Checkpoint{}, ErrCommitUnknown
	}
}

// Acknowledgment only removes the exact pending projection after Host accepts
// it. Replaying before this CAS is safe for the monotone absolute total.
func (r *Repository) Acknowledge(ctx context.Context, c Checkpoint) (Checkpoint, error) {
	if ctx == nil {
		return Checkpoint{}, ErrInvalidContext
	}
	if !c.valid() || c.revision == 0 || c.revision >= 1<<63-1 {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	if !c.hasPending {
		return c, nil
	}
	next := c
	next.revision++
	next.hasPending = false
	next.pending = PendingProjection{}
	return r.Save(ctx, c, next)
}
