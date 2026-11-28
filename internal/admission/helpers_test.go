package admission

import (
	"context"
	"sync"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}
func (f *fakeClock) Sleep(d time.Duration) { f.Advance(d) }
func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- f.Now().Add(d)
	return ch
}

// fakeSensor returns whatever reading is set, and counts samples.
type fakeSensor struct {
	mu       sync.Mutex
	name     string
	reading  Reading
	calls    int
	observed []Envelope
	block    chan struct{} // when set, Sample waits for it
}

func newFake(name string) *fakeSensor { return &fakeSensor{name: name, reading: OpenReading(name)} }

func (f *fakeSensor) sensorName() string { return f.name }
func (f *fakeSensor) sample(ctx context.Context, _ runSnapshot) Reading {
	f.mu.Lock()
	f.calls++
	r, block := f.reading, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return r
}
func (f *fakeSensor) observeEffective(e Envelope) {
	f.mu.Lock()
	f.observed = append(f.observed, e)
	f.mu.Unlock()
}
func (f *fakeSensor) set(r Reading) {
	f.mu.Lock()
	r.sensor = f.name
	f.reading = r
	f.mu.Unlock()
}
func (f *fakeSensor) sampleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// restricted reports whether a single reading limits anything.
func restricted(r Reading) bool { return Compose([]Reading{r}).restricted() }
