package admission

import (
	"testing"
	"time"
)

func target(local int) Envelope {
	e := open()
	e.MaxLocalSlots = local
	return e
}

func TestSmootherTightensAtOnceAndRelaxesOneSlotPerThrottleAfterTheDwell(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, 30*time.Second, 10*time.Second)
	if got := s.apply(open(), 6); got.MaxLocalSlots != Unlimited {
		t.Fatalf("first evaluation must not ramp: %d", got.MaxLocalSlots)
	}
	clk.Advance(time.Second)
	if got := s.apply(target(1), 6); got.MaxLocalSlots != 1 {
		t.Fatalf("a tightening limit must apply at once, got %d", got.MaxLocalSlots)
	}
	clk.Advance(time.Second)
	// Relaxed from now on: nothing changes during the dwell.
	for i := 0; i < 29; i++ {
		clk.Advance(time.Second)
		if got := s.apply(open(), 6); got.MaxLocalSlots != 1 {
			t.Fatalf("relaxed after only %ds of a 30s dwell: %d slots", i+1, got.MaxLocalSlots)
		}
	}
	var seen []int
	for i := 0; i < 70; i++ {
		clk.Advance(time.Second)
		got := s.apply(open(), 6).MaxLocalSlots
		if len(seen) == 0 || seen[len(seen)-1] != got {
			seen = append(seen, got)
		}
	}
	want := []int{1, 2, 3, 4, 5, Unlimited}
	if len(seen) != len(want) {
		t.Fatalf("recovery went through %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("recovery went through %v, want %v", seen, want)
		}
	}
}

func TestSmootherADroppedLimitDoesNotFlapOnASpike(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, 30*time.Second, 10*time.Second)
	s.apply(open(), 4)
	clk.Advance(time.Second)
	s.apply(target(0), 4) // a one-second thermal spike
	clk.Advance(time.Second)
	changes, last := 0, 0
	for i := 0; i < 25; i++ {
		clk.Advance(time.Second)
		got := s.apply(open(), 4).MaxLocalSlots
		if got != last {
			changes++
			last = got
		}
	}
	if changes != 0 || last != 0 {
		t.Fatalf("admission came back during the dwell after a one-second spike (changes=%d last=%d)", changes, last)
	}
}

func TestSmootherHoldCountsAsZeroSlotsAndRecoversByRamping(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, 5*time.Second, 2*time.Second)
	s.apply(open(), 3)
	hold := open()
	hold.Hold = HoldTransient
	got := s.apply(hold, 3)
	if got.MaxLocalSlots != 0 || got.MaxRemoteSlots != 0 || got.Hold != HoldTransient {
		t.Fatalf("a hold must stop admission at once: %+v", got)
	}
	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		if got = s.apply(open(), 3); got.MaxLocalSlots != 0 {
			t.Fatalf("admission resumed before the dwell: %d", got.MaxLocalSlots)
		}
	}
	clk.Advance(time.Second)
	if got = s.apply(open(), 3); got.MaxLocalSlots != 1 || got.Hold != holdNone {
		t.Fatalf("first step after the dwell should be one slot with no hold: %+v", got)
	}
	clk.Advance(time.Second)
	if got = s.apply(open(), 3); got.MaxLocalSlots != 1 {
		t.Fatalf("the ramp throttle was ignored: %d", got.MaxLocalSlots)
	}
	clk.Advance(time.Second)
	if got = s.apply(open(), 3); got.MaxLocalSlots != 2 {
		t.Fatalf("expected the second slot after the throttle: %d", got.MaxLocalSlots)
	}
}

func TestSmootherRemoteAccessClosesAtOnceAndOpensAfterTheDwell(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, 10*time.Second, time.Second)
	s.apply(open(), 4)
	off := open()
	off.AllowRemote = false
	if got := s.apply(off, 4); got.AllowRemote {
		t.Fatal("remote must close at once")
	}
	clk.Advance(5 * time.Second)
	if got := s.apply(open(), 4); got.AllowRemote {
		t.Fatal("remote reopened before the dwell")
	}
	clk.Advance(10 * time.Second)
	if got := s.apply(open(), 4); !got.AllowRemote {
		t.Fatal("remote should have reopened after the dwell")
	}
	// A blip back to closed restarts the dwell.
	s.apply(off, 4)
	clk.Advance(8 * time.Second)
	s.apply(open(), 4)
	clk.Advance(8 * time.Second)
	if got := s.apply(open(), 4); got.AllowRemote {
		t.Fatal("the dwell should restart from the last time the signal cleared")
	}
}

func TestSmootherNamesTheRampWhileRecovering(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, time.Second, time.Second)
	s.apply(open(), 4)
	s.apply(target(0), 4)
	clk.Advance(2 * time.Second)
	s.apply(open(), 4)
	clk.Advance(2 * time.Second)
	got := s.apply(open(), 4)
	found := false
	for _, b := range got.Bindings {
		found = found || (b.Field == "max_local_slots" && b.Sensor == "ramp")
	}
	if got.MaxLocalSlots >= 4 || !found {
		t.Fatalf("a recovering limit should say so: %+v", got)
	}
}

func TestSmootherTierTightensAtOnceAndRecoversOneStepPerDwell(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, 10*time.Second, time.Second)
	s.apply(open(), 4)
	low := open()
	low.ModelTier = tierMinimal
	if got := s.apply(low, 4); got.ModelTier != tierMinimal {
		t.Fatalf("tier = %d", got.ModelTier)
	}
	clk.Advance(11 * time.Second)
	if got := s.apply(open(), 4); got.ModelTier != tierMinimal {
		t.Fatal("the dwell starts when the signal clears, so it cannot have elapsed yet")
	}
	clk.Advance(11 * time.Second)
	if got := s.apply(open(), 4); got.ModelTier != tierReduced {
		t.Fatalf("expected one step up, got %d", got.ModelTier)
	}
	clk.Advance(11 * time.Second)
	if got := s.apply(open(), 4); got.ModelTier != tierFull {
		t.Fatalf("expected full tier, got %d", got.ModelTier)
	}
}

func TestSmootherReportsALimitAtOrAboveTheWorkerCountAsNoLimit(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, time.Second, time.Second)
	for _, limit := range []int{4, 9, 1000} {
		if got := s.apply(target(limit), 4); got.MaxLocalSlots != Unlimited {
			t.Fatalf("a limit of %d with 4 workers restricts nothing, reported as %d", limit, got.MaxLocalSlots)
		}
	}
	if got := s.apply(target(3), 4); got.MaxLocalSlots != 3 {
		t.Fatalf("a limit below the worker count must still apply: %d", got.MaxLocalSlots)
	}
}
