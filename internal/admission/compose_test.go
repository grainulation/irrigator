package admission

import (
	"strings"
	"testing"
	"time"
)

func TestComposeStrictestWinsAndAttributesEachField(t *testing.T) {
	a := OpenReading("a")
	a.MaxLocalSlots, a.Reason = 4, "a caps local"
	b := OpenReading("b")
	b.MaxLocalSlots, b.allowRemote, b.severity, b.Reason = 2, false, hard, "b is strict"
	c := OpenReading("c")
	c.maxRemoteSlots, c.minPoll, c.modelTier, c.Reason = 3, 5*time.Second, tierReduced, "c is slow"

	env := Compose([]Reading{a, b, c})
	if env.MaxLocalSlots != 2 || env.MaxRemoteSlots != 3 || env.AllowRemote || env.ModelTier != tierReduced || env.MinPoll != 5*time.Second || env.Hold != holdNone {
		t.Fatalf("composition is not strictest-wins: %+v", env)
	}
	bound := map[string]string{}
	for _, b := range env.Bindings {
		bound[b.Field] += b.Sensor
	}
	want := map[string]string{"max_local_slots": "b", "max_remote_slots": "c", "allow_remote": "b", "model_tier": "c", "min_poll": "c"}
	for field, sensor := range want {
		if bound[field] != sensor {
			t.Errorf("%s bound by %q, want %q (all: %v)", field, bound[field], sensor, bound)
		}
	}
}

func TestComposeOpenReadingsRestrictNothing(t *testing.T) {
	env := Compose([]Reading{OpenReading("a"), OpenReading("b")})
	if env.restricted() || len(env.Bindings) != 0 {
		t.Fatalf("no sensor had an opinion but the envelope is restricted: %+v", env)
	}
	if got := Compose(nil); got.restricted() {
		t.Fatal("no sensors must mean no restriction")
	}
}

func TestComposeTiesAreAllAttributed(t *testing.T) {
	a, b := OpenReading("a"), OpenReading("b")
	a.MaxLocalSlots, b.MaxLocalSlots = 1, 1
	env := Compose([]Reading{a, b})
	if len(env.Bindings) != 2 {
		t.Fatalf("both sensors set the limit and both should be named: %+v", env.Bindings)
	}
}

func TestComposeStrongestHoldWinsAndPermanentOutranksTransient(t *testing.T) {
	a, b := OpenReading("a"), OpenReading("b")
	a.hold, b.hold = HoldTransient, HoldPermanent
	if env := Compose([]Reading{a, b}); env.Hold != HoldPermanent {
		t.Fatalf("hold = %v", env.Hold)
	}
	if env := Compose([]Reading{a}); env.Hold != HoldTransient {
		t.Fatalf("hold = %v", env.Hold)
	}
}

func TestComposeListsDegradedSensors(t *testing.T) {
	failed, stale, ok := OpenReading("z-failed"), OpenReading("a-stale"), OpenReading("m-ok")
	failed.probe, stale.probe = probeFailed, probeStale
	env := Compose([]Reading{failed, stale, ok})
	if len(env.Degraded) != 2 || env.Degraded[0] != "a-stale" || env.Degraded[1] != "z-failed" {
		t.Fatalf("degraded = %v", env.Degraded)
	}
}

func TestSummaryNamesWhatBinds(t *testing.T) {
	r := OpenReading("power")
	r.MaxLocalSlots, r.Reason = 2, "thermal serious"
	n := OpenReading("network")
	n.allowRemote, n.Reason = false, "offline"
	got := Compose([]Reading{r, n}).summary(6)
	for _, part := range []string{"local 2/6", "power: thermal serious", "remote paused", "network: offline"} {
		if !strings.Contains(got, part) {
			t.Errorf("summary %q lacks %q", got, part)
		}
	}
	if Compose(nil).summary(6) != "open" {
		t.Error("an unrestricted envelope should read as open")
	}
}
