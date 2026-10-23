package integrations

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

var (
	ErrDurableAdmissionCapacity = errors.New("durable command admission waiters exhausted")
	ErrDurablePauseConflict     = errors.New("durable command activation pause already reserved")
	ErrDurablePauseHandle       = errors.New("durable command activation pause handle invalid")
)

// Run injects the sum of its already-admitted producer capacities. This low
// port owns no duplicate limit for public requests, jobs or automation runs.
type DurableGateLimits struct {
	waiters int
	drain   time.Duration
}

func NewDurableGateLimits(waiters int, drain time.Duration) (DurableGateLimits, error) {
	limits := DurableGateLimits{waiters: waiters, drain: drain}
	if !limits.valid() {
		return DurableGateLimits{}, ErrInvalidInput
	}
	return limits, nil
}

func (l DurableGateLimits) valid() bool {
	return l.waiters > 0 && l.drain > 0 && l.drain <= time.Minute
}

type durableGateState uint8

const (
	gateInvalid durableGateState = iota
	gateOpen
	gatePausing
	gatePaused
	gateQuarantined
	gateClosing
)

type durableGate struct {
	state       durableGateState
	changed     chan struct{}
	limits      DurableGateLimits
	occurrences int
	waiters     int
	cycle       uint64
	pause       *ActivationPause
}

// ActivationPause is an opaque, one-cycle capability. C4 owns SQL proof and
// publication; the durable service owns only admissions and lease draining.
type ActivationPause struct {
	service *DurableCommandService
	cycle   uint64
}

// gateChangedLocked wakes admission and drain waiters after a state/count
// transition. Callers hold s.mu; no callback, SQL, Host or driver runs under it.
func (s *DurableCommandService) gateChangedLocked() {
	close(s.gate.changed)
	s.gate.changed = make(chan struct{})
}

// finishIfDrainedLocked is the only transition that gives the runtime to the
// terminal release goroutine. Waiters must have observed Closing first.
func (s *DurableCommandService) finishIfDrainedLocked() *storage.CommandRuntime {
	if s.gate.state != gateClosing || s.active != 0 || s.gate.occurrences != 0 || s.gate.waiters != 0 || s.runtime == nil {
		return nil
	}
	runtime := s.runtime
	s.runtime = nil
	return runtime
}

// enterGate counts a new occurrence or dispatch before its first callback or
// command ID claim. A dispatch within an already counted occurrence may pass
// Pausing/Paused; it is still separately counted until its durable result.
func (s *DurableCommandService) enterGate(ctx context.Context, inside *internalOccurrence, occurrence bool) (*storage.CommandRuntime, error) {
	if s == nil || nilInterface(ctx) || s.router == nil || s.store == nil || s.closed == nil {
		return nil, ErrInvalidInput
	}
	if s.router.host.MQTTReadOnlyObservation() {
		return nil, ErrCommandAuthorizationDenied
	}
	waiting := false
	defer func() {
		if !waiting {
			return
		}
		s.mu.Lock()
		s.gate.waiters--
		runtime := s.finishIfDrainedLocked()
		s.mu.Unlock()
		if runtime != nil {
			go s.finishClose(runtime)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if inside != nil && (inside.port == nil || inside.port.service != s || !inside.live.Load() || s.gate.occurrences == 0) {
			s.mu.Unlock()
			return nil, ErrCommandAuthorizationDenied
		}
		if s.gate.state == gateClosing || s.runtime == nil {
			s.mu.Unlock()
			return nil, ErrDurableLedgerUnavailable
		}
		if s.gate.state == gateOpen || (inside != nil && !occurrence) {
			if occurrence {
				s.gate.occurrences++
			} else {
				s.active++
			}
			runtime := s.runtime
			s.mu.Unlock()
			return runtime, nil
		}
		if !waiting {
			if s.gate.limits.waiters == 0 || s.gate.waiters >= s.gate.limits.waiters {
				s.mu.Unlock()
				return nil, ErrDurableAdmissionCapacity
			}
			s.gate.waiters++
			waiting = true
		}
		changed := s.gate.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *DurableCommandService) begin(ctx context.Context, inside *internalOccurrence) (*storage.CommandRuntime, error) {
	return s.enterGate(ctx, inside, false)
}

func (s *DurableCommandService) admitOccurrence(ctx context.Context) error {
	_, err := s.enterGate(ctx, nil, true)
	return err
}

func (s *DurableCommandService) endOccurrence() {
	s.mu.Lock()
	s.gate.occurrences--
	s.gateChangedLocked()
	runtime := s.finishIfDrainedLocked()
	s.mu.Unlock()
	if runtime != nil {
		go s.finishClose(runtime)
	}
}

// PauseAndDrain closes new admissions, then waits for every occurrence and
// last result-persistence attempt. Timeout before any C4 transaction restores
// Open atomically; it does not cancel a running automation or driver.
func (s *DurableCommandService) PauseAndDrain(ctx context.Context) (*ActivationPause, error) {
	if s == nil || nilInterface(ctx) || s.router == nil || s.store == nil || s.closed == nil || !s.gate.limits.valid() {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.gate.limits.drain)
	defer cancel()
	s.mu.Lock()
	if s.gate.state == gateClosing {
		s.mu.Unlock()
		return nil, ErrDurableLedgerUnavailable
	}
	if s.gate.state != gateOpen {
		s.mu.Unlock()
		return nil, ErrDurablePauseConflict
	}
	s.gate.cycle++
	handle := &ActivationPause{service: s, cycle: s.gate.cycle}
	s.gate.pause = handle
	s.gate.state = gatePausing
	s.gateChangedLocked()
	s.mu.Unlock()
	for {
		if err := waitCtx.Err(); err != nil {
			s.cancelUncommittedPause(handle)
			return nil, err
		}
		s.mu.Lock()
		if s.gate.state == gateClosing {
			s.mu.Unlock()
			return nil, ErrDurableLedgerUnavailable
		}
		if s.gate.pause != handle || s.gate.state != gatePausing {
			s.mu.Unlock()
			return nil, ErrDurablePauseHandle
		}
		if s.gate.occurrences == 0 && s.active == 0 {
			s.gate.state = gatePaused
			s.gateChangedLocked()
			s.mu.Unlock()
			return handle, nil
		}
		changed := s.gate.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-waitCtx.Done():
		}
	}
}

func (s *DurableCommandService) cancelUncommittedPause(handle *ActivationPause) {
	s.mu.Lock()
	if s.gate.pause == handle && s.gate.state == gatePausing {
		s.gate.pause = nil
		s.gate.state = gateOpen
		s.gateChangedLocked()
	}
	s.mu.Unlock()
}

// Quarantine retains the same handle while a lost commit ACK is reconciled.
// It never decides the SQL outcome or consumes the capability.
func (s *DurableCommandService) Quarantine(handle *ActivationPause) error {
	if s == nil || handle == nil {
		return ErrDurablePauseHandle
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.validPauseLocked(handle) || s.gate.state != gatePaused {
		return ErrDurablePauseHandle
	}
	s.gate.state = gateQuarantined
	s.gateChangedLocked()
	return nil
}

// ResumeOld requires C4 to have proved the activation attempt absent.
func (s *DurableCommandService) ResumeOld(handle *ActivationPause) error {
	return s.resolvePause(handle)
}

// PublishNew requires C4 to have proved commit and coherent publication.
func (s *DurableCommandService) PublishNew(handle *ActivationPause) error {
	return s.resolvePause(handle)
}

func (s *DurableCommandService) resolvePause(handle *ActivationPause) error {
	if s == nil || handle == nil {
		return ErrDurablePauseHandle
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.validPauseLocked(handle) || (s.gate.state != gatePaused && s.gate.state != gateQuarantined) {
		return ErrDurablePauseHandle
	}
	s.gate.pause = nil
	s.gate.state = gateOpen
	s.gateChangedLocked()
	return nil
}

func (s *DurableCommandService) validPauseLocked(handle *ActivationPause) bool {
	return handle != nil && handle.service == s && s.gate.pause == handle && handle.cycle == s.gate.cycle
}
