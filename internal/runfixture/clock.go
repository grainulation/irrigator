package runfixture

import (
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
)

// FakeClock is a deterministic, goroutine-safe Clock.
//
// Manual mode (default): After and Sleep register a timer at now+d and block
// until Advance moves time to or past it. Timers fire in deadline order (ties in
// registration order) and each channel receives its own deadline, so the value
// never depends on how large an Advance step was.
//
// Auto mode: After and Sleep advance time by d themselves and return at once,
// for single-goroutine loops that just need elapsed time to progress. Concurrent
// callers each add their own d, so the total elapsed time is the sum of all
// requested durations whatever the interleaving.
//
// Nothing here polls or reads the wall clock.
type FakeClock struct {
	mu       sync.Mutex
	now      time.Time
	auto     bool
	timers   []*fakeTimer
	watchers []chan struct{}
	requests []time.Duration
}

type fakeTimer struct {
	when time.Time
	ch   chan time.Time
}

func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{now: start} }

func NewAutoFakeClock(start time.Time) *FakeClock { return &FakeClock{now: start, auto: true} }

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, d)
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	if f.auto {
		f.now = f.now.Add(d)
		ch <- f.now
		f.fireDueLocked()
		return ch
	}
	f.timers = append(f.timers, &fakeTimer{when: f.now.Add(d), ch: ch})
	for _, w := range f.watchers {
		close(w)
	}
	f.watchers = nil
	return ch
}

func (f *FakeClock) Sleep(d time.Duration) { <-f.After(d) }

// Advance moves time forward and fires every timer that has come due.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.fireDueLocked()
}

func (f *FakeClock) fireDueLocked() {
	for {
		next := -1
		for i, tm := range f.timers {
			if !tm.when.After(f.now) && (next < 0 || tm.when.Before(f.timers[next].when)) {
				next = i
			}
		}
		if next < 0 {
			return
		}
		tm := f.timers[next]
		f.timers = append(f.timers[:next], f.timers[next+1:]...)
		tm.ch <- tm.when
	}
}

// Pending reports how many timers have not fired yet.
func (f *FakeClock) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// Seek moves time forward to t (never backward: a clock that rewinds would
// deliver timers out of order) and fires everything that came due.
func (f *FakeClock) Seek(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		panic("fakeClock.Seek: cannot rewind")
	}
	f.now = t
	f.fireDueLocked()
}

// Requested returns the durations passed to After/Sleep so far, in call order.
func (f *FakeClock) Requested() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.requests...)
}

// WaitForTimers blocks until at least n timers are pending. It wakes on timer
// registration rather than polling; the timeout only guards a hung test.
func (f *FakeClock) WaitForTimers(t *testing.T, n int) {
	t.Helper()
	for {
		f.mu.Lock()
		if len(f.timers) >= n {
			f.mu.Unlock()
			return
		}
		w := make(chan struct{})
		f.watchers = append(f.watchers, w)
		f.mu.Unlock()
		select {
		case <-w:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %d pending timers", n)
		}
	}
}

// SetClock replaces the process-wide default clock and returns a restore func.
// Use it only for code reachable solely through activeClock; tests that can pass
// a Clock explicitly should do that instead and never touch the global.
func SetClock(c clock.Clock) func() { return clock.ActiveClock.Swap(c) }

func RecvWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

var ClockEpoch = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
