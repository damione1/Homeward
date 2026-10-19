package automationtriggers

import (
	"time"

	"github.com/damione1/Homeward/internal/sequencer"
)

// Clock separates civil instants from elapsed time. All timers are cancellable
// and service-owned; tests supply a deterministic implementation.
type Clock interface {
	Now() time.Time
	MonoNow() time.Duration
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type RealClock struct{}

func (RealClock) Now() time.Time                 { return time.Now() }
func (RealClock) MonoNow() time.Duration         { return sequencer.RealClock{}.MonoNow() }
func (RealClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }
