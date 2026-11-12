package runops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// rootFakeProcess is a childProcess whose termination the test controls. Unlike
// the integration drain tests it needs no real subprocess, so the timeout and
// escalation arithmetic can be checked against a fake clock.
type rootFakeProcess struct {
	mu        sync.Mutex
	signals   []os.Signal
	kills     int
	signalErr error
	onSignal  func()
	onKill    func()
}

func (p *rootFakeProcess) Signal(s os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, s)
	hook, err := p.onSignal, p.signalErr
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (p *rootFakeProcess) Kill() error {
	p.mu.Lock()
	p.kills++
	hook := p.onKill
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (p *rootFakeProcess) counts() (signals, kills int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.signals), p.kills
}

func rootRequireRealGlobalClock(t *testing.T) {
	t.Helper()
	if _, ok := clock.ActiveClock.Load().(clock.RealClock); !ok {
		t.Fatalf("a test leaked a replaced global clock: %T", clock.ActiveClock.Load())
	}
}

func rootElapsed(c *runfixture.FakeClock) time.Duration { return c.Now().Sub(runfixture.ClockEpoch) }

func rootChild(p *rootFakeProcess) *controllerChild {
	return &controllerChild{process: p, done: make(chan struct{})}
}

func TestDrainEscalatesAndGivesUpOnTheInjectedClock(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	p := &rootFakeProcess{}
	clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)

	err := drainControllerChildrenWithClock(clock, root, map[string]*controllerChild{"a": rootChild(p)}, 100*time.Millisecond, 300*time.Millisecond)

	if err == nil || !strings.Contains(err.Error(), "owned worker cleanup unconfirmed for a") {
		t.Fatalf("missing give-up error: %v", err)
	}
	if strings.Contains(err.Error(), "cannot persist") {
		t.Fatalf("shutdown request should have persisted: %v", err)
	}
	if signals, kills := p.counts(); signals != 1 || kills != 1 {
		t.Fatalf("signals=%d kills=%d, want one SIGTERM then one SIGKILL escalation", signals, kills)
	}
	if got := rootElapsed(clock); got != 300*time.Millisecond {
		t.Fatalf("drain ended after %v of clock time, want exactly the 300ms limit", got)
	}
	for _, d := range clock.Requested() {
		if d != 20*time.Millisecond {
			t.Fatalf("unexpected poll step %v", d)
		}
	}
	if runfixture.Read(t, root).Control != "down" {
		t.Fatal("drain did not persist control=down")
	}
	rootRequireRealGlobalClock(t)
}

func TestDrainReturnsImmediatelyWhenChildExitsOnTerm(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	p := &rootFakeProcess{}
	child := rootChild(p)
	p.onSignal = func() { close(child.done) }
	clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)

	if err := drainControllerChildrenWithClock(clock, root, map[string]*controllerChild{"a": child}, time.Second, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, kills := p.counts(); kills != 0 {
		t.Fatalf("killed a child that exited on SIGTERM (%d kills)", kills)
	}
	if got := clock.Requested(); len(got) != 0 {
		t.Fatalf("drain waited %v although the child was already gone", got)
	}
}

func TestDrainKillsOnlyAfterGraceAndStopsOnceChildExits(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	p := &rootFakeProcess{}
	child := rootChild(p)
	p.onKill = func() { close(child.done) }
	clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)

	if err := drainControllerChildrenWithClock(clock, root, map[string]*controllerChild{"a": child}, 100*time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if signals, kills := p.counts(); signals != 1 || kills != 1 {
		t.Fatalf("signals=%d kills=%d", signals, kills)
	}
	if got := rootElapsed(clock); got < 100*time.Millisecond || got >= time.Second {
		t.Fatalf("kill happened at %v: must be after grace and the loop must stop before limit", got)
	}
}

func TestDrainSignalErrors(t *testing.T) {
	t.Run("real failure is reported", func(t *testing.T) {
		root := runfixture.New(t, runfixture.Task("a", 0))
		p := &rootFakeProcess{signalErr: errors.New("operation not permitted")}
		child := rootChild(p)
		p.onKill = func() { close(child.done) }
		err := drainControllerChildrenWithClock(runfixture.NewAutoFakeClock(runfixture.ClockEpoch), root, map[string]*controllerChild{"a": child}, 0, time.Second)
		if err == nil || !strings.Contains(err.Error(), "signal owned worker: operation not permitted") {
			t.Fatalf("signal failure hidden: %v", err)
		}
	})
	t.Run("already finished process is not an error", func(t *testing.T) {
		root := runfixture.New(t, runfixture.Task("a", 0))
		p := &rootFakeProcess{signalErr: os.ErrProcessDone}
		child := rootChild(p)
		p.onSignal = func() { close(child.done) }
		if err := drainControllerChildrenWithClock(runfixture.NewAutoFakeClock(runfixture.ClockEpoch), root, map[string]*controllerChild{"a": child}, time.Second, 2*time.Second); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDrainWrapperUsesTheGlobalClockForItsDeadlines(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
	defer runfixture.SetClock(clock)()
	p := &rootFakeProcess{}

	// 2s of limit elapses on the fake clock; a wall-clock deadline would make
	// this test sleep for it.
	err := drainControllerChildren(root, map[string]*controllerChild{"a": rootChild(p)}, time.Second, 2*time.Second)

	if err == nil || !strings.Contains(err.Error(), "cleanup unconfirmed for a") {
		t.Fatalf("missing give-up error: %v", err)
	}
	if got := rootElapsed(clock); got != 2*time.Second {
		t.Fatalf("clock advanced %v, want exactly the 2s limit", got)
	}
	if _, kills := p.counts(); kills != 1 {
		t.Fatalf("kills=%d", kills)
	}
}

func TestWaitProcessGoneDeadlineAndPollingShareOneClock(t *testing.T) {
	t.Run("visible until the deadline", func(t *testing.T) {
		clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
		probes := 0
		err := launch.WaitProcessGoneWithClock(clock, func() error { probes++; return nil }, 5*time.Second)
		if !errors.Is(err, launch.ErrProcessCleanup) {
			t.Fatalf("err = %v, want errProcessCleanup", err)
		}
		if got := rootElapsed(clock); got != 5*time.Second {
			t.Fatalf("clock advanced %v, want the 5s timeout", got)
		}
		requested := clock.Requested()
		if probes != len(requested)+1 {
			t.Fatalf("%d probes for %d sleeps: each sleep must be followed by exactly one probe", probes, len(requested))
		}
		for _, d := range requested {
			if d != time.Millisecond {
				t.Fatalf("poll step %v", d)
			}
		}
	})
	t.Run("ESRCH after two sightings", func(t *testing.T) {
		clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
		probes := 0
		err := launch.WaitProcessGoneWithClock(clock, func() error {
			probes++
			if probes < 3 {
				return nil
			}
			return syscall.ESRCH
		}, time.Hour)
		if err != nil || rootElapsed(clock) != 2*time.Millisecond {
			t.Fatalf("err=%v elapsed=%v", err, rootElapsed(clock))
		}
	})
	t.Run("EPERM never proves exit but is retried until the deadline", func(t *testing.T) {
		clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
		err := launch.WaitProcessGoneWithClock(clock, func() error { return syscall.EPERM }, 10*time.Millisecond)
		if !errors.Is(err, launch.ErrProcessCleanup) || rootElapsed(clock) != 10*time.Millisecond {
			t.Fatalf("err=%v elapsed=%v", err, rootElapsed(clock))
		}
	})
	t.Run("transient EPERM then ESRCH succeeds", func(t *testing.T) {
		clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
		probes := 0
		err := launch.WaitProcessGoneWithClock(clock, func() error {
			probes++
			if probes < 4 {
				return syscall.EPERM
			}
			return syscall.ESRCH
		}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unexpected probe error fails without waiting", func(t *testing.T) {
		clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
		err := launch.WaitProcessGoneWithClock(clock, func() error { return errors.New("probe broke") }, time.Hour)
		if !errors.Is(err, launch.ErrProcessCleanup) || len(clock.Requested()) != 0 {
			t.Fatalf("err=%v sleeps=%v", err, clock.Requested())
		}
	})
	t.Run("zero timeout probes exactly once", func(t *testing.T) {
		clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
		probes := 0
		err := launch.WaitProcessGoneWithClock(clock, func() error { probes++; return nil }, 0)
		if !errors.Is(err, launch.ErrProcessCleanup) || probes != 1 || len(clock.Requested()) != 0 {
			t.Fatalf("err=%v probes=%d sleeps=%v", err, probes, clock.Requested())
		}
		if err := launch.WaitProcessGoneWithClock(clock, func() error { return syscall.ESRCH }, 0); err != nil {
			t.Fatalf("already-gone process must succeed even with zero timeout: %v", err)
		}
	})
	rootRequireRealGlobalClock(t)
}

func TestWaitProcessGoneStepsOnlyWhenAnotherGoroutineAdvancesTime(t *testing.T) {
	clock := runfixture.NewFakeClock(runfixture.ClockEpoch)
	var probes int
	var mu sync.Mutex
	result := make(chan error, 1)
	go func() {
		result <- launch.WaitProcessGoneWithClock(clock, func() error {
			mu.Lock()
			probes++
			mu.Unlock()
			return nil
		}, 3*time.Millisecond)
	}()
	for step := 1; step <= 3; step++ {
		clock.WaitForTimers(t, 1)
		select {
		case err := <-result:
			t.Fatalf("returned at step %d before the deadline: %v", step, err)
		default:
		}
		clock.Advance(time.Millisecond)
	}
	if err := runfixture.RecvWithin[error](t, result, "deadline error"); !errors.Is(err, launch.ErrProcessCleanup) {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if probes != 4 {
		t.Fatalf("probes = %d, want 4 (initial + one per advanced millisecond)", probes)
	}
}

func TestWaitProcessGoneWrapperUsesGlobalClock(t *testing.T) {
	clock := runfixture.NewAutoFakeClock(runfixture.ClockEpoch)
	defer runfixture.SetClock(clock)()
	err := launch.WaitProcessGone(func() error { return nil }, time.Minute)
	if !errors.Is(err, launch.ErrProcessCleanup) || rootElapsed(clock) != time.Minute {
		t.Fatalf("err=%v elapsed=%v", err, rootElapsed(clock))
	}
}

// The managed mock adapter reaches time only through the global clock, so this
// test must swap it. It checks that the delay follows the injected clock and
// that context cancellation still wins over a pending timer.
func TestManagedMockDelayFollowsInjectedClockAndHonorsCancellation(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	binding, err := launch.ResolveExecutionCatalog(catalog, "inline", "small")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	request := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RunID: "run", OperationID: "op", RequestID: "request", Attempt: 1, TimeoutMS: 600000,
		Task: model.Task{ID: "node", Prompt: "p", Requires: []string{"answer"}, MockDelay: 3600}}
	type outcome struct {
		response model.WorkResponse
		err      error
	}
	start := func(ctx context.Context, prefix string) <-chan outcome {
		out := make(chan outcome, 1)
		go func() {
			r, e := managedWork(ctx, dir, prefix, request, binding)
			out <- outcome{r, e}
		}()
		return out
	}

	t.Run("cancellation beats a pending delay", func(t *testing.T) {
		clock := runfixture.NewFakeClock(runfixture.ClockEpoch)
		defer runfixture.SetClock(clock)()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, filepath.Join(dir, "cancelled"))
		clock.WaitForTimers(t, 1)
		cancel()
		got := runfixture.RecvWithin(t, out, "cancelled managed work")
		if got.err == nil || !strings.Contains(got.err.Error(), "context canceled") {
			t.Fatalf("err = %v, want context cancellation", got.err)
		}
		if rootElapsed(clock) != 0 {
			t.Fatalf("cancellation moved the fake clock by %v", rootElapsed(clock))
		}
	})
	t.Run("advancing the clock completes the delay", func(t *testing.T) {
		clock := runfixture.NewFakeClock(runfixture.ClockEpoch)
		defer runfixture.SetClock(clock)()
		out := start(context.Background(), filepath.Join(dir, "completed"))
		clock.WaitForTimers(t, 1)
		select {
		case got := <-out:
			t.Fatalf("finished before the delay elapsed: %+v %v", got.response, got.err)
		default:
		}
		clock.Advance(3600 * time.Second)
		got := runfixture.RecvWithin(t, out, "managed work after advance")
		if got.err != nil || got.response.Output["answer"] != "mock answer" {
			t.Fatalf("response=%+v err=%v", got.response, got.err)
		}
	})
	rootRequireRealGlobalClock(t)
}
