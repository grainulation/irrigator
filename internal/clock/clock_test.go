package clock

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic, goroutine-safe Clock.
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
type fakeClock struct {
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

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func newAutoFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start, auto: true} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
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

func (f *fakeClock) Sleep(d time.Duration) { <-f.After(d) }

// Advance moves time forward and fires every timer that has come due.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.fireDueLocked()
}

func (f *fakeClock) fireDueLocked() {
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
func (f *fakeClock) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// AdvanceToNext seeks straight to the earliest pending deadline, firing it and
// anything sharing it, and reports whether a timer existed. Seeking is the
// point: a 30s production timeout costs one call, not 30s.
func (f *fakeClock) AdvanceToNext() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.timers) == 0 {
		return false
	}
	next := f.timers[0].when
	for _, tm := range f.timers {
		if tm.when.Before(next) {
			next = tm.when
		}
	}
	if next.After(f.now) {
		f.now = next
	}
	f.fireDueLocked()
	return true
}

// Seek moves time forward to t (never backward: a clock that rewinds would
// deliver timers out of order) and fires everything that came due.
func (f *fakeClock) Seek(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		panic("fakeClock.Seek: cannot rewind")
	}
	f.now = t
	f.fireDueLocked()
}

// Settle fires pending timers earliest-first until none remain or max steps
// have run. It only sees timers already registered: a goroutine woken by one
// registers its next timer asynchronously, so callers that chain timers pair
// Settle with WaitForTimers instead of assuming the system is quiescent.
func (f *fakeClock) Settle(max int) (steps int) {
	for steps < max && f.AdvanceToNext() {
		steps++
	}
	return steps
}

// Requested returns the durations passed to After/Sleep so far, in call order.
func (f *fakeClock) Requested() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.requests...)
}

// WaitForTimers blocks until at least n timers are pending. It wakes on timer
// registration rather than polling; the timeout only guards a hung test.
func (f *fakeClock) WaitForTimers(t *testing.T, n int) {
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

// setClock replaces the process-wide default clock and returns a restore func.
// Use it only for code reachable solely through activeClock; tests that can pass
// a Clock explicitly should do that instead and never touch the global.
func setClock(c Clock) func() { return ActiveClock.Swap(c) }

func recvWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

var clockEpoch = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

func TestClockInjection(t *testing.T) {
	vc := newAutoFakeClock(clockEpoch)
	restore := setClock(vc)
	defer restore()

	if !ActiveClock.Now().Equal(clockEpoch) {
		t.Fatalf("activeClock.Now() = %v, want %v", ActiveClock.Now(), clockEpoch)
	}
	ActiveClock.
		Sleep(5 * time.Second)
	if want := clockEpoch.Add(5 * time.Second); !ActiveClock.Now().Equal(want) {
		t.Fatalf("after Sleep, Now() = %v, want %v", ActiveClock.Now(), want)
	}
	ch := ActiveClock.After(10 * time.Second)
	select {
	case got := <-ch:
		if want := clockEpoch.Add(15 * time.Second); !got.Equal(want) {
			t.Fatalf("After delivered %v, want %v", got, want)
		}
	default:
		t.Fatal("auto clock After did not fire immediately")
	}
}

func TestActiveClockRestoreAndNilMeanReal(t *testing.T) {
	if _, ok := ActiveClock.Load().(RealClock); !ok {
		t.Fatalf("default clock is %T, want realClock", ActiveClock.Load())
	}
	restore := setClock(newFakeClock(clockEpoch))
	if _, ok := ActiveClock.Load().(*fakeClock); !ok {
		t.Fatalf("clock after setClock is %T", ActiveClock.Load())
	}
	restoreNil := setClock(nil)
	if _, ok := ActiveClock.Load().(RealClock); !ok {
		t.Fatalf("setClock(nil) installed %T, want realClock", ActiveClock.Load())
	}
	restoreNil()
	if _, ok := ActiveClock.Load().(*fakeClock); !ok {
		t.Fatalf("nested restore lost the fake clock: %T", ActiveClock.Load())
	}
	restore()
	if _, ok := ActiveClock.Load().(RealClock); !ok {
		t.Fatalf("restore left %T installed", ActiveClock.Load())
	}
}

func TestFakeClockFiresOnlyWhenAdvancedPastDeadline(t *testing.T) {
	c := newFakeClock(clockEpoch)
	ch := c.After(10 * time.Second)
	c.Advance(9 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired before its deadline")
	default:
	}
	c.Advance(time.Second)
	if got := recvWithin(t, ch, "timer"); !got.Equal(clockEpoch.Add(10 * time.Second)) {
		t.Fatalf("timer delivered %v", got)
	}
	if got := c.After(0); len(got) != 1 {
		t.Fatal("zero duration must fire immediately")
	}
}

func TestFakeClockDeliversEachTimersOwnDeadline(t *testing.T) {
	c := newFakeClock(clockEpoch)
	a, b, d := c.After(30*time.Second), c.After(10*time.Second), c.After(20*time.Second)
	c.Advance(time.Hour) // one big step: values must still be the deadlines
	for _, tc := range []struct {
		name string
		ch   <-chan time.Time
		want time.Duration
	}{{"a", a, 30 * time.Second}, {"b", b, 10 * time.Second}, {"d", d, 20 * time.Second}} {
		if got := recvWithin(t, tc.ch, tc.name); !got.Equal(clockEpoch.Add(tc.want)) {
			t.Errorf("%s delivered %v, want +%v", tc.name, got, tc.want)
		}
	}
	if got := c.Now(); !got.Equal(clockEpoch.Add(time.Hour)) {
		t.Errorf("Now() = %v", got)
	}
}

func TestFakeClockConcurrentSleepersWakeTogetherWithoutRaces(t *testing.T) {
	c := newFakeClock(clockEpoch)
	const workers = 8
	woke := make(chan time.Time, workers)
	var wg sync.WaitGroup
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func(d time.Duration) {
			defer wg.Done()
			c.Sleep(d)
			woke <- c.Now()
		}(time.Duration(i) * time.Second)
	}
	c.WaitForTimers(t, workers)
	if len(woke) != 0 {
		t.Fatal("a sleeper returned before time advanced")
	}
	c.Advance(workers * time.Second)
	wg.Wait()
	close(woke)
	for got := range woke {
		if !got.Equal(clockEpoch.Add(workers * time.Second)) {
			t.Errorf("sleeper observed %v after the single advance", got)
		}
	}
}

func TestFakeClockAutoModeSumsConcurrentSleeps(t *testing.T) {
	c := newAutoFakeClock(clockEpoch)
	const workers, steps = 6, 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < steps; j++ {
				c.Sleep(10 * time.Millisecond)
				_ = c.Now()
			}
		}()
	}
	wg.Wait()
	if want := clockEpoch.Add(workers * steps * 10 * time.Millisecond); !c.Now().Equal(want) {
		t.Fatalf("Now() = %v, want %v", c.Now(), want)
	}
	if got := len(c.Requested()); got != workers*steps {
		t.Fatalf("recorded %d requests, want %d", got, workers*steps)
	}
}

func TestActiveClockSwapIsSafeWhileWorkersRead(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = ActiveClock.Now()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		restore := setClock(newAutoFakeClock(clockEpoch))
		restore()
	}
	close(stop)
	wg.Wait()
}

func TestFakeClockSeekSettleAndPending(t *testing.T) {
	c := newFakeClock(clockEpoch)
	a, b := c.After(30*time.Second), c.After(5*time.Second)
	if c.Pending() != 2 {
		t.Fatalf("pending %d", c.Pending())
	}
	if !c.AdvanceToNext() || !c.Now().Equal(clockEpoch.Add(5*time.Second)) {
		t.Fatalf("did not seek to earliest deadline: %v", c.Now())
	}
	if got := recvWithin(t, b, "5s timer"); !got.Equal(clockEpoch.Add(5 * time.Second)) {
		t.Fatalf("5s timer delivered %v", got)
	}
	if steps := c.Settle(10); steps != 1 || c.Pending() != 0 {
		t.Fatalf("settle ran %d steps, %d pending", steps, c.Pending())
	}
	if got := recvWithin(t, a, "30s timer"); !got.Equal(clockEpoch.Add(30 * time.Second)) {
		t.Fatalf("30s timer delivered %v", got)
	}
	if c.AdvanceToNext() {
		t.Fatal("AdvanceToNext reported a timer on an empty clock")
	}
	c.Seek(clockEpoch.Add(time.Hour))
	defer func() {
		if recover() == nil {
			t.Fatal("Seek rewound the clock")
		}
	}()
	c.Seek(clockEpoch)
}
