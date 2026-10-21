package recordermaintenance

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
	"reflect"
	"sync"
	"time"
)

const CycleTimeout = 10 * time.Second
const CycleInterval = time.Minute

// Clock injects the ordinary scheduler, not a second execution path. Tests use
// the same tick channel and owner loop as the real minute ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}
type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}
type RealClock struct{}
type realTicker struct{ ticker *time.Ticker }

func (RealClock) Now() time.Time                   { return time.Now() }
func (RealClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }
func (t realTicker) C() <-chan time.Time           { return t.ticker.C }
func (t realTicker) Stop()                         { t.ticker.Stop() }

type Boundary struct {
	epoch    string
	sequence uint64
	at       time.Time
	tick     time.Duration
	valid    bool
}

func NewBoundary(epoch string, sequence uint64, at time.Time, tick time.Duration) (Boundary, error) {
	// Point performs the same epoch admission without conflating cursor zero
	// (valid before the first observation) with an observation sequence.
	p, err := NewPoint(PointInput{Epoch: epoch, Sequence: 1, At: at, Availability: domain.AvailabilityAvailable, Freshness: domain.FreshnessFresh, Provenance: domain.ProvenanceObserved})
	if err != nil || !p.Valid() || tick < 0 {
		return Boundary{}, ErrInvalid
	}
	return Boundary{epoch, sequence, at, tick, true}, nil
}
func (b Boundary) Components() (string, uint64, time.Time, time.Duration, error) {
	if !b.valid {
		return "", 0, time.Time{}, 0, ErrInvalid
	}
	return b.epoch, b.sequence, b.at, b.tick, nil
}

type CycleResult struct {
	Processed, DeletedRaw, DeletedTraces, DeletedAggregates, DeletedAudit int64
	More                                                                  bool
}
type Port interface {
	Maintain(context.Context, time.Time, Boundary) (CycleResult, error)
}
type BoundarySource interface{ Boundary() (Boundary, error) }
type Status struct {
	Cycles    uint64
	LastAsOf  time.Time
	Result    CycleResult
	LastError error
	Active    bool
}

type Worker struct {
	mu      sync.Mutex
	port    Port
	source  BoundarySource
	clock   Clock
	status  Status
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(port Port, source BoundarySource) (*Worker, error) {
	return NewWithClock(port, source, RealClock{})
}
func NewWithClock(port Port, source BoundarySource, clock Clock) (*Worker, error) {
	if nilValue(port) || nilValue(source) || nilValue(clock) {
		return nil, ErrInvalid
	}
	return &Worker{port: port, source: source, clock: clock, done: make(chan struct{})}, nil
}
func nilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return r.IsNil()
	default:
		return false
	}
}
func (w *Worker) Start() error {
	if w == nil || w.done == nil {
		return ErrInvalid
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return ErrInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.started = true
	w.cancel = cancel
	go w.run(ctx)
	return nil
}
func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	ticker := w.clock.NewTicker(CycleInterval)
	if nilValue(ticker) {
		w.mu.Lock()
		w.status.LastError = ErrInvalid
		w.mu.Unlock()
		return
	}
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		w.cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticker.C():
			if !ok {
				w.mu.Lock()
				w.status.LastError = ErrInvalid
				w.mu.Unlock()
				return
			}
		}
	}
}
func (w *Worker) cycle(ctx context.Context) {
	bounded, cancel := context.WithTimeout(ctx, CycleTimeout)
	defer cancel()
	b, err := w.source.Boundary()
	asOf := w.clock.Now().UTC().Truncate(time.Microsecond)
	w.mu.Lock()
	w.status.Active = true
	w.mu.Unlock()
	var result CycleResult
	if err == nil {
		result, err = w.port.Maintain(bounded, asOf, b)
	}
	w.mu.Lock()
	w.status.Active = false
	w.status.Cycles++
	w.status.LastAsOf = asOf
	w.status.LastError = err
	w.status.Result = result
	w.mu.Unlock()
}
func (w *Worker) Done() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.done
}
func (w *Worker) Status() Status {
	if w == nil {
		return Status{LastError: ErrInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// BeginClose stops new cycles immediately. Done remains the actual owner
// completion signal even if the caller's shared shutdown budget expires.
func (w *Worker) BeginClose() <-chan struct{} {
	if w == nil || w.done == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		w.started = true
		close(w.done)
	} else if w.cancel != nil {
		w.cancel()
	}
	return w.done
}
func (w *Worker) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if nilValue(ctx) || w.done == nil {
		return ErrInvalid
	}
	w.BeginClose()
	select {
	case <-w.done:
		return nil
	default:
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
