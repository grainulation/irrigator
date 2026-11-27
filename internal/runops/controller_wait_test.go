package runops

import (
	"os"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// startWait runs waitForWork on a manual fake clock and reports each skipped
// idle tick and the wait's result on channels.
func startWait(c *runfixture.FakeClock, watch *checkpoint.StateWatch, wakeup chan struct{}, signals chan os.Signal) (<-chan time.Time, <-chan bool) {
	idle := make(chan time.Time, 64)
	done := make(chan bool, 1)
	go func() { done <- waitForWork(c, watch, wakeup, signals, func(now time.Time) { idle <- now }) }()
	return idle, done
}

func waitTick(t *testing.T, c *runfixture.FakeClock) {
	t.Helper()
	c.WaitForTimers(t, 1)
	c.Advance(50 * time.Millisecond)
}

func TestControllerWaitSkipsIdleTicksUntilTheCheckpointIsPublished(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	c := runfixture.NewFakeClock(runfixture.ClockEpoch)
	watch := checkpoint.StateWatch{Root: root, Refresh: controllerIdleRefresh}
	watch.Due(c.Now())
	idle, done := startWait(c, &watch, nil, nil)
	for range 3 {
		waitTick(t, c)
		runfixture.RecvWithin(t, idle, "idle tick")
	}
	// An external control request is a checkpoint publication.
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	waitTick(t, c)
	if runfixture.RecvWithin(t, done, "wait to end") {
		t.Fatal("a checkpoint publication was reported as a signal")
	}
	if len(idle) != 0 {
		t.Fatal("the publishing tick was treated as idle")
	}
}

func TestControllerWaitRunsAPassAfterTheIdleRefresh(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	c := runfixture.NewFakeClock(runfixture.ClockEpoch)
	watch := checkpoint.StateWatch{Root: root, Refresh: controllerIdleRefresh}
	watch.Due(c.Now())
	idle, done := startWait(c, &watch, nil, nil)
	ticks := int(controllerIdleRefresh / (50 * time.Millisecond))
	for range ticks - 1 {
		waitTick(t, c)
		runfixture.RecvWithin(t, idle, "idle tick")
	}
	waitTick(t, c)
	runfixture.RecvWithin(t, done, "refresh pass")
	if got := c.Now().Sub(runfixture.ClockEpoch); got != controllerIdleRefresh {
		t.Fatalf("refresh pass after %v, want %v", got, controllerIdleRefresh)
	}
}

func TestControllerWaitEndsAtOnceOnWakeupOrSignal(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	c := runfixture.NewFakeClock(runfixture.ClockEpoch)
	watch := checkpoint.StateWatch{Root: root, Refresh: controllerIdleRefresh}
	watch.Due(c.Now())

	wakeup := make(chan struct{}, 1)
	wakeup <- struct{}{}
	_, done := startWait(c, &watch, wakeup, nil)
	if runfixture.RecvWithin(t, done, "wakeup") {
		t.Fatal("a child exit was reported as a signal")
	}

	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	_, done = startWait(c, &watch, nil, signals)
	if !runfixture.RecvWithin(t, done, "signal") {
		t.Fatal("a signal was not reported")
	}
	if !c.Now().Equal(runfixture.ClockEpoch) {
		t.Fatal("wakeup or signal waited for a poll tick")
	}
}
