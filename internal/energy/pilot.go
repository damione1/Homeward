package energy

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrInvalidPilot = errors.New("energy: invalid pilot composition")

// OutputBinding binds open registry identities and a private profile; the
// engine never encodes the MVP alias names or a fixed Hilo entity set.
type OutputBinding struct {
	profile Profile
	key     integrations.SourceKey
}

func NewOutputBinding(profile Profile, key integrations.SourceKey) (OutputBinding, error) {
	if !profile.Valid() {
		return OutputBinding{}, ErrInvalidPilot
	}
	if _, _, e := key.Components(); e != nil {
		return OutputBinding{}, ErrInvalidPilot
	}
	return OutputBinding{profile, key}, nil
}

// LeaseSource is supplied by an actual source owner. Left profiles consult it
// on the current report and before each tick; it cannot extend freshness.
type LeaseSource interface {
	// The two ticks share the sequencer's process-local monotone epoch. The
	// source owner must attest continuity from the report through checkedAtTick;
	// it may never infer that continuity from a keepalive or wall timestamp.
	Lease(context.Context, Profile, ReportIdentity, time.Duration, time.Duration) (Lease, bool, error)
}

func missingLease(source LeaseSource) bool {
	if source == nil {
		return true
	}
	value := reflect.ValueOf(source)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

type Clock interface {
	NowTick() time.Duration
	After(time.Duration) <-chan time.Time
}
type RealClock struct{ Sequence *sequencer.Sequencer }

func (c RealClock) NowTick() time.Duration               { return c.Sequence.MonotonicNow() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type Pilot struct {
	sequence    *sequencer.Sequencer
	repository  *Repository
	bindings    []OutputBinding
	leaseSource LeaseSource
	clock       Clock
	mu          sync.Mutex
	sub         *sequencer.Subscription
	started     bool
	stopped     bool
	epoch       string
}
type counter struct {
	binding        integrations.Binding
	state          Checkpoint
	lease          Lease
	hasLease       bool
	awaitingReport bool
}
type deliveryEvent struct {
	entry sequencer.Entry
	loss  bool
}

func NewPilotFactory(sequence *sequencer.Sequencer, repository *Repository, bindings []OutputBinding, leases LeaseSource, clock Clock) (integrations.PilotFactory, error) {
	if sequence == nil || repository == nil || clock == nil || len(bindings) == 0 || len(bindings) > 16 {
		return nil, ErrInvalidPilot
	}
	value := reflect.ValueOf(clock)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return nil, ErrInvalidPilot
	}
	if real, ok := clock.(RealClock); ok && real.Sequence == nil {
		return nil, ErrInvalidPilot
	}
	copyBindings := append([]OutputBinding(nil), bindings...)
	seenSource := map[domain.EntityID]bool{}
	seenOutput := map[domain.EntityID]bool{}
	seenKey := map[integrations.SourceKey]bool{}
	for _, b := range copyBindings {
		if !b.profile.Valid() || seenSource[b.profile.source] || seenOutput[b.profile.output] || seenKey[b.key] || (b.profile.method == MethodLeft && (missingLease(leases) || b.profile.basis != TimeBasisReceipt || b.profile.tickInterval == 0)) {
			return nil, ErrInvalidPilot
		}
		seenSource[b.profile.source] = true
		seenOutput[b.profile.output] = true
		seenKey[b.key] = true
	}
	return func() integrations.Pilot {
		return &Pilot{sequence: sequence, repository: repository, bindings: copyBindings, leaseSource: leases, clock: clock}
	}, nil
}
func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || ctx == nil || host == nil || tasks == nil || ctx.Err() != nil {
		return ErrInvalidPilot
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return ErrInvalidPilot
	}
	p.started = true
	p.mu.Unlock()
	sub, cursor, err := p.sequence.SubscribeRecorder()
	if err != nil {
		return err
	}
	epoch, _, err := cursor.Components()
	if err != nil {
		_ = sub.Close()
		return err
	}
	p.epoch = epoch
	if !p.setSubscription(sub) {
		return ErrInvalidPilot
	}
	candidates := make([]integrations.Candidate, 0, len(p.bindings))
	for _, b := range p.bindings {
		candidate, e := b.candidate()
		if e != nil {
			sub.Close()
			return e
		}
		candidates = append(candidates, candidate)
	}
	resolved, err := host.Discover(ctx, candidates)
	if err != nil {
		sub.Close()
		return err
	}
	if len(resolved) != len(p.bindings) {
		sub.Close()
		return ErrInvalidPilot
	}
	counters := make(map[domain.EntityID]*counter, len(p.bindings))
	for i, b := range p.bindings {
		_, id, e := resolved[i].IDs()
		if e != nil || id != b.profile.output {
			sub.Close()
			return ErrInvalidPilot
		}
		state, e := p.repository.Load(ctx, b.profile)
		if e != nil {
			sub.Close()
			return e
		}
		counters[b.profile.source] = &counter{binding: resolved[i], state: state}
	}
	events := make(chan deliveryEvent, 32)
	if err := tasks.Go(func(taskCtx context.Context) error { return p.read(taskCtx, events) }); err != nil {
		sub.Close()
		return err
	}
	if err := tasks.Go(func(taskCtx context.Context) error { return p.run(taskCtx, host, counters, events) }); err != nil {
		sub.Close()
		return err
	}
	return nil
}
func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.stopped = true
	sub := p.sub
	p.sub = nil
	p.mu.Unlock()
	if sub != nil {
		err := sub.Close()
		if !errors.Is(err, sequencer.ErrSubscriptionClosed) {
			return err
		}
	}
	return nil
}
func (p *Pilot) setSubscription(sub *sequencer.Subscription) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		_ = sub.Close()
		return false
	}
	p.sub = sub
	return true
}
func (p *Pilot) read(ctx context.Context, events chan<- deliveryEvent) error {
	defer close(events)
	for ctx.Err() == nil {
		p.mu.Lock()
		sub := p.sub
		p.mu.Unlock()
		if sub == nil {
			return nil
		}
		delivery, err := sub.Next(ctx)
		if err != nil && !errors.Is(err, sequencer.ErrRecorderGap) {
			if ctx.Err() != nil || errors.Is(err, sequencer.ErrSubscriptionClosed) {
				return nil
			}
			return err
		}
		if errors.Is(err, sequencer.ErrRecorderGap) || delivery.Kind() == sequencer.DeliveryLoss || !delivery.ReplayValid() {
			_ = sub.Close()
			fresh, cursor, e := p.sequence.SubscribeRecorder()
			if e != nil {
				return e
			}
			epoch, _, e := cursor.Components()
			if e != nil || epoch != p.epoch {
				_ = fresh.Close()
				return ErrInvalidPilot
			}
			if !p.setSubscription(fresh) {
				return nil
			}
			select {
			case events <- deliveryEvent{loss: true}:
			case <-ctx.Done():
				return nil
			}
			continue
		}
		entry, ok := delivery.Entry()
		if !ok {
			return ErrInvalidPilot
		}
		select {
		case events <- deliveryEvent{entry: entry}:
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}
func (p *Pilot) run(ctx context.Context, host integrations.SessionHost, counters map[domain.EntityID]*counter, events <-chan deliveryEvent) error {
	// A committed but unacknowledged value is published exactly as stored. Only
	// afterwards may restart mark coverage stale and close the former bound.
	for _, item := range counters {
		if err := p.publishPending(ctx, host, item); err != nil {
			return err
		}
		if item.state.revision != 0 && item.state.hasPrevious {
			next, e := BreakContinuity(item.state, DiagnosticRestart, time.Now().UTC())
			if e != nil {
				return e
			}
			saved, e := p.repository.Save(ctx, item.state, next)
			if e != nil {
				return e
			}
			item.state = saved
			if e := p.publishPending(ctx, host, item); e != nil {
				return e
			}
		} else {
			// A restored checkpoint or an entirely new counter has no proven
			// live source. Publish absence through this counter's own Host binding.
			before := item.state
			next, e := MissingSource(before, time.Now().UTC())
			if e != nil {
				return e
			}
			saved, e := p.repository.Save(ctx, before, next)
			if e != nil {
				return e
			}
			item.state = saved
			if e := p.publishPending(ctx, host, item); e != nil {
				return e
			}
		}
	}
	for ctx.Err() == nil {
		var timer <-chan time.Time
		if delay, ok := p.nextTick(counters); ok {
			timer = p.clock.After(delay)
		}
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if event.loss {
				for _, item := range counters {
					item.hasLease = false
					item.awaitingReport = false
					if item.state.revision == 0 {
						continue
					}
					next, e := BreakContinuity(item.state, DiagnosticDeliveryLoss, time.Now().UTC())
					if e != nil {
						return e
					}
					saved, e := p.repository.Save(ctx, item.state, next)
					if e != nil {
						return e
					}
					item.state = saved
					if e := p.publishPending(ctx, host, item); e != nil {
						return e
					}
				}
				continue
			}
			entity, _, _, isState := event.entry.State()
			if !isState {
				continue
			}
			item := counters[entity]
			if item == nil {
				continue
			}
			item.awaitingReport = false
			if e := p.report(ctx, host, item, event.entry); e != nil {
				return e
			}
		case <-timer:
			if e := p.ticks(ctx, host, counters); e != nil {
				return e
			}
		}
	}
	return nil
}
func (p *Pilot) report(ctx context.Context, host integrations.SessionHost, item *counter, entry sequencer.Entry) error {
	before := item.state
	identity, e := NewReportIdentity(entry)
	if e != nil || identity.epoch != p.epoch {
		return ErrInvalidReport
	}
	if before.hasReport && ((identity.epoch == before.lastReport.epoch && identity.cursor <= before.lastReport.cursor) || sourceSequenceDuplicate(before.lastReport, identity)) ||
		identity.epoch == before.fenceEpoch && identity.cursor <= before.fenceCursor {
		return nil
	}
	var nextLease Lease
	var hasNextLease bool
	var superseded bool
	if before.profile.method == MethodLeft {
		_, state, _, ok := entry.State()
		if !ok {
			return ErrInvalidReport
		}
		if _, diagnostic := validPower(state); diagnostic == DiagnosticNone {
			current, present, checkErr := p.sourceCurrent(identity)
			if checkErr != nil {
				return checkErr
			}
			if !present {
				return p.closeLease(ctx, host, item, DiagnosticUnavailable, entry.ReceivedAt())
			}
			superseded = !current
			if !superseded {
				checkedAt := p.clock.NowTick()
				if checkedAt < entry.AdmissionTick() {
					return ErrInvalidPilot
				}
				candidate, leasePresent, leaseErr := p.leaseSource.Lease(ctx, before.profile, identity, entry.AdmissionTick(), checkedAt)
				if leaseErr != nil || !leasePresent || !candidate.validFor(identity, entry.AdmissionTick()) || candidate.untilTick <= checkedAt {
					// A new Host report may have superseded this entry while the
					// source owner was consulted. Let Recorder deliver it in order.
					current, present, checkErr = p.sourceCurrent(identity)
					if checkErr != nil {
						return checkErr
					}
					if present && !current {
						superseded = true
					} else {
						if err := p.closeLease(ctx, host, item, DiagnosticLeaseExpired, entry.ReceivedAt()); err != nil {
							return err
						}
						if leaseErr != nil || leasePresent {
							return ErrInvalidPilot
						}
						return nil
					}
				} else {
					nextLease, hasNextLease = candidate, true
				}
			}
			if before.hasPrevious {
				reason := DiagnosticNone
				switch {
				case !sameSource(before.previous.report, identity):
					reason = DiagnosticSourceChanged
				case !item.hasLease || !item.lease.report.valid() ||
					!sameSource(item.lease.report, before.previous.report) ||
					item.lease.startTick > before.previous.reportAdmissionTick ||
					entry.AdmissionTick() > item.lease.untilTick:
					reason = DiagnosticLeaseExpired
				}
				if reason != DiagnosticNone {
					if err := p.closeLease(ctx, host, item, reason, entry.ReceivedAt()); err != nil {
						return err
					}
					before = item.state
				}
			}
			if superseded && !before.hasPrevious {
				// An older queued report cannot establish a held-power bound
				// without proof for that report. The latest one will do so.
				return nil
			}
		}
	}
	next, changed, e := AdvanceReport(before.profile, before, entry, p.epoch)
	if e != nil {
		return e
	}
	if !changed {
		return nil
	}
	saved, e := p.repository.Save(ctx, before, next)
	if e != nil {
		return e
	}
	item.state = saved
	item.hasLease = superseded && item.hasLease && item.state.hasPrevious
	item.awaitingReport = superseded
	if e := p.publishPending(ctx, host, item); e != nil {
		return e
	}
	if hasNextLease && item.state.hasPrevious {
		item.lease, item.hasLease = nextLease, true
	}
	return nil
}

func (p *Pilot) closeLease(ctx context.Context, host integrations.SessionHost, item *counter, reason Diagnostic, at time.Time) error {
	item.hasLease = false
	if !item.state.hasPrevious {
		return nil
	}
	next, err := BreakContinuity(item.state, reason, at)
	if err != nil {
		return err
	}
	saved, err := p.repository.Save(ctx, item.state, next)
	if err != nil {
		return err
	}
	item.state = saved
	return p.publishPending(ctx, host, item)
}

// sourceCurrent compares the live Host projection with an admitted report.
// Host's existing per-entity revision distinguishes reports with no protocol
// sequence; a newer revision means the Recorder entry is still in flight.
func (p *Pilot) sourceCurrent(report ReportIdentity) (current, present bool, err error) {
	state, present, err := p.sequence.State(report.entity)
	if err != nil || !present {
		return false, present, err
	}
	_, _, _, source, revision, err := state.Components()
	if err != nil || revision < report.revision || revision == report.revision && source != report.source {
		return false, true, ErrInvalidPilot
	}
	return revision == report.revision && source == report.source, true, nil
}
func (p *Pilot) nextTick(counters map[domain.EntityID]*counter) (time.Duration, bool) {
	now := p.clock.NowTick()
	best := time.Duration(1<<63 - 1)
	found := false
	for _, item := range counters {
		if !item.hasLease || !item.state.hasPrevious || item.awaitingReport {
			continue
		}
		due := best
		if item.state.previous.admissionTick <= best-item.state.profile.tickInterval {
			due = item.state.previous.admissionTick + item.state.profile.tickInterval
		}
		if item.lease.untilTick < due {
			due = item.lease.untilTick + 1
		}
		delay := due - now
		if delay < 0 {
			delay = 0
		}
		if delay < best {
			best = delay
			found = true
		}
	}
	return best, found
}
func (p *Pilot) ticks(ctx context.Context, host integrations.SessionHost, counters map[domain.EntityID]*counter) error {
	for _, item := range counters {
		if !item.hasLease || !item.state.hasPrevious || item.awaitingReport {
			continue
		}
		current, present, err := p.sourceCurrent(item.state.lastReport)
		if err != nil {
			return err
		}
		if !present {
			if err := p.closeLease(ctx, host, item, DiagnosticUnavailable, time.Now().UTC()); err != nil {
				return err
			}
			continue
		}
		if !current {
			item.awaitingReport = true
			continue
		}
		now := p.clock.NowTick()
		if now > item.lease.untilTick {
			if err := p.closeLease(ctx, host, item, DiagnosticLeaseExpired, time.Now().UTC()); err != nil {
				return err
			}
			continue
		}
		candidate, leasePresent, leaseErr := p.leaseSource.Lease(ctx, item.state.profile, item.state.lastReport, item.state.previous.reportAdmissionTick, now)
		// Use the later monotone tick for integration, then read the current
		// source. A report admitted after this read has a tick no earlier than
		// the bound we are about to commit.
		checkedAt := p.clock.NowTick()
		current, present, err = p.sourceCurrent(item.state.lastReport)
		if err != nil {
			return err
		}
		if present && !current {
			item.awaitingReport = true
			continue
		}
		if !present {
			if err := p.closeLease(ctx, host, item, DiagnosticUnavailable, time.Now().UTC()); err != nil {
				return err
			}
			continue
		}
		if checkedAt > item.lease.untilTick || leaseErr != nil || !leasePresent ||
			!candidate.validFor(item.state.lastReport, item.state.previous.reportAdmissionTick) || candidate.untilTick <= checkedAt {
			if err := p.closeLease(ctx, host, item, DiagnosticLeaseExpired, time.Now().UTC()); err != nil {
				return err
			}
			if leaseErr != nil || leasePresent && (!candidate.validFor(item.state.lastReport, item.state.previous.reportAdmissionTick) || candidate.untilTick <= checkedAt) {
				return ErrInvalidPilot
			}
			continue
		}
		item.lease = candidate
		if checkedAt-item.state.previous.admissionTick < item.state.profile.tickInterval {
			continue
		}
		before := item.state
		next, changed, e := AdvanceTick(before.profile, before, item.lease, checkedAt)
		if e != nil {
			return e
		}
		if !changed {
			item.hasLease = false
			continue
		}
		saved, e := p.repository.Save(ctx, before, next)
		if e != nil {
			return e
		}
		item.state = saved
		if !saved.hasPrevious {
			item.hasLease = false
		}
		if e := p.publishPending(ctx, host, item); e != nil {
			return e
		}
	}
	return nil
}
func (p *Pilot) publishPending(ctx context.Context, host integrations.SessionHost, item *counter) error {
	pending, ok := item.state.Pending()
	if !ok {
		return nil
	}
	observation, e := pending.Observation()
	if e != nil {
		return e
	}
	if e := host.Publish(ctx, item.binding, observation); e != nil {
		return e
	}
	ack, e := p.repository.Acknowledge(ctx, item.state)
	if e != nil {
		return e
	}
	item.state = ack
	return nil
}
func (b OutputBinding) candidate() (integrations.Candidate, error) {
	unit, _ := b.profile.unit.Symbol()
	semantic := wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL
	if b.profile.semantic == domain.SemanticTotalIncreasing {
		semantic = wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING
	}
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Estimated energy", Endpoint: "energy", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: unit, Precision: uint32(b.profile.precision), Semantic: semantic, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM}}}
	return integrations.NewCandidate(b.key, []*wireregistry.CapabilityDescription{description})
}
