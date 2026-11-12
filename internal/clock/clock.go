package clock

import (
	"sync/atomic"
	"time"
)

// Clock abstracts time operations for deterministic, fast testing. Every
// duration calculation in the controller and supervisor (grace windows, drain
// limits, process-gone deadlines) reads Now from the same Clock that supplies
// Sleep and After, so a substituted clock moves deadlines and waits together.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// clockSource is the process-wide default clock. It is a Clock itself so call
// sites such as executor.go keep reading activeClock.After(...), but the
// delegate is held atomically: a test swapping it never races with workers
// that are concurrently reading the time.
type clockSource struct{ current atomic.Pointer[Clock] }

func newClockSource() *clockSource {
	s := &clockSource{}
	s.Swap(RealClock{})
	return s
}

func (s *clockSource) Load() Clock { return *s.current.Load() }

func (s *clockSource) Now() time.Time                         { return s.Load().Now() }
func (s *clockSource) Sleep(d time.Duration)                  { s.Load().Sleep(d) }
func (s *clockSource) After(d time.Duration) <-chan time.Time { return s.Load().After(d) }

// swap installs c (nil means the real clock) and returns a function restoring
// the previous one. Production code never calls it; tests that must exercise
// code reached only through the global (executor, managed adapter) use it,
// while controller and supervisor helpers also accept an explicit Clock.
func (s *clockSource) Swap(c Clock) (restore func()) {
	if c == nil {
		c = RealClock{}
	}
	prev := s.current.Swap(&c)
	return func() { s.current.Store(prev) }
}

var ActiveClock = newClockSource()
