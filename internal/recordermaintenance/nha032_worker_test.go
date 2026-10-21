package recordermaintenance

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type maintenanceTestClock struct {
	ticks   chan time.Time
	stopped atomic.Bool
}

func (c *maintenanceTestClock) Now() time.Time {
	return time.Date(2026, 10, 20, 12, 0, 0, 123456789, time.UTC)
}
func (c *maintenanceTestClock) NewTicker(time.Duration) Ticker { return c }
func (c *maintenanceTestClock) C() <-chan time.Time            { return c.ticks }
func (c *maintenanceTestClock) Stop()                          { c.stopped.Store(true) }

type maintenanceTestBoundary struct{}

func (maintenanceTestBoundary) Boundary() (Boundary, error) {
	return NewBoundary("0123456789abcdef0123456789abcdef", 3, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), time.Second)
}

type maintenanceTestPort struct {
	entered     chan context.Context
	release     chan struct{}
	active, max atomic.Int32
}

func (p *maintenanceTestPort) Maintain(ctx context.Context, asOf time.Time, b Boundary) (CycleResult, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		old := p.max.Load()
		if active <= old || p.max.CompareAndSwap(old, active) {
			break
		}
	}
	if asOf.Nanosecond()%1000 != 0 {
		return CycleResult{}, ErrInvalid
	}
	p.entered <- ctx
	select {
	case <-p.release:
		return CycleResult{Processed: 1}, nil
	case <-ctx.Done():
		return CycleResult{}, ctx.Err()
	}
}
func TestNHA032WorkerUsesOneSchedulerAndJoinsCancellation(t *testing.T) {
	clock := &maintenanceTestClock{ticks: make(chan time.Time, 1)}
	port := &maintenanceTestPort{entered: make(chan context.Context, 2), release: make(chan struct{}, 1)}
	worker, err := NewWithClock(port, maintenanceTestBoundary{}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Start(); err != nil {
		t.Fatal(err)
	}
	var attempt context.Context
	select {
	case attempt = <-port.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter ordinary first cycle")
	}
	deadline, ok := attempt.Deadline()
	if !ok || time.Until(deadline) > CycleTimeout {
		t.Fatal("cycle has no bounded deadline")
	}
	clock.ticks <- clock.Now()
	port.release <- struct{}{}
	select {
	case <-port.entered:
	case <-time.After(time.Second):
		t.Fatal("ordinary ticker did not resume")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = worker.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-worker.Done():
	default:
		t.Fatal("Close returned before Done")
	}
	if port.max.Load() != 1 || !clock.stopped.Load() {
		t.Fatal("worker overlapped cycles or retained ticker")
	}
}
func TestNHA032AdmissionRejectsImpossibleResultsAndCopies(t *testing.T) {
	for _, in := range []ValuesInput{{}, {Duration: time.Second, Reset: ResetNotApplicable}, {HasMeasurement: true, Reset: ResetNotApplicable}, {HasContribution: true, Reset: ResetNotApplicable}, {Contribution: 1, Reset: ResetUnknown}, {Reset: Reset(255)}} {
		if _, err := NewValues(in); err == nil {
			t.Fatalf("invalid result admitted %+v", in)
		}
	}
	v, err := NewValues(ValuesInput{Reset: ResetUnknown})
	if err != nil {
		t.Fatal(err)
	}
	copy := v.Input()
	copy.Contribution = 7
	if v.Input().Contribution != 0 {
		t.Fatal("result aliases DTO")
	}
	var nilPort *maintenanceTestPort
	if _, err = New(nilPort, maintenanceTestBoundary{}); err == nil {
		t.Fatal("typed nil port admitted")
	}
	worker, err := New(&maintenanceTestPort{}, maintenanceTestBoundary{})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = worker.Start(); err == nil {
		t.Fatal("closed worker restarted")
	}
}
