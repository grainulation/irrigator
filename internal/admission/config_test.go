package admission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPresetsAreValidAndUnboundedRestrictsNothing(t *testing.T) {
	for _, name := range presetNames() {
		c, err := resolve(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err = c.validate(); err != nil {
			t.Fatalf("%s is invalid: %v", name, err)
		}
	}
	c, _ := resolve("unbounded")
	if c.active() {
		t.Fatal("unbounded must not be active: a run without --admission has to behave exactly as before")
	}
	if b, _ := resolve("balanced"); !b.active() || b.Power == nil || b.Network == nil {
		t.Fatal("balanced should enable the power and network sensors")
	}
}

func TestAllPresetsBuildTheirSensorsAndOffpeakHasAWindow(t *testing.T) {
	for _, name := range presetNames() {
		cfg, _ := resolve(name)
		if !cfg.active() {
			continue
		}
		e, err := newRunEvaluator(t.TempDir(), cfg, 6, newFakeClock(), true, Deps{DiagnoseLimit: func(string, string, int, time.Time) (LimitEvidence, error) { return LimitEvidence{}, nil }})
		if err != nil || e == nil {
			t.Fatalf("%s: %v", name, err)
		}
		e.Close()
	}
	o, _ := resolve("offpeak")
	if o.OffPeak == nil || o.OffPeak.Window != "22:00-07:00" {
		t.Fatalf("offpeak preset: %+v", o.OffPeak)
	}
	if q, _ := resolve("quiet"); q.Presence == nil {
		t.Fatal("quiet should yield to a user who is present")
	}
}

func TestRateLimitSensorIsNotRegisteredWithoutADiagnoser(t *testing.T) {
	cfg, _ := resolve("balanced")
	e, err := newRunEvaluator(t.TempDir(), cfg, 6, newFakeClock(), true, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range e.entries {
		if _, ok := en.reg.sensor.(*rateLimit); ok {
			t.Fatal("the rate-limit sensor needs a diagnoser and must not be registered without one")
		}
	}
}

func TestPhase3AndTierConfigValidation(t *testing.T) {
	good, _ := resolve("offpeak")
	bad := map[string]func(c *config){
		"window":       func(c *config) { c.OffPeak.Window = "late" },
		"signal path":  func(c *config) { c.OffPeak.SignalFile = "relative.json" },
		"disk order":   func(c *config) { c.Disk = &diskConfig{LowFreeMB: 100, CriticalFreeMB: 500} },
		"swap percent": func(c *config) { c.Memory = &memoryConfig{SwapUsedPercentLimit: 150} },
		"presence":     func(c *config) { c.Presence = &presenceConfig{ActiveWithinSeconds: -1} },
		"breaker alpha": func(c *config) {
			c.Breaker = &breakerConfig{Alpha: 2, Threshold: .5, MinOutcomes: 1, IntervalSeconds: 1, OpenSeconds: 1, HalfLifeSeconds: 1}
		},
		"rate cooldown": func(c *config) { c.RateLimit = &rateLimitConfig{DefaultCooldownSeconds: 100, MaxCooldownSeconds: 10} },
		"profile tier":  func(c *config) { c.ProfileTiers = map[string]string{"p": "huge"} },
		"battery tier":  func(c *config) { c.Power.OnBatteryMaxTier = "tiny" },
		"empty profile": func(c *config) { c.ProfileTiers = map[string]string{"": "full"} },
	}
	for name, mutate := range bad {
		c := good
		power := *good.Power
		off := *good.OffPeak
		c.Power, c.OffPeak = &power, &off
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: an invalid config was accepted", name)
		}
	}
	ok := good
	ok.ProfileTiers = map[string]string{"heavy": "full", "light": "minimal"}
	if err := ok.validate(); err != nil {
		t.Fatalf("a valid tier map was rejected: %v", err)
	}
}

func TestResolveReadsAConfigFileAndRejectsUnknownFieldsAndBadValues(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.json", `{"schema":"irrigator.admission/v1","exit_dwell_seconds":5,"override":true}`)
	if c, err := resolve(ok); err != nil || c.Preset != "custom" || !c.Override {
		t.Fatalf("valid file: %+v %v", c, err)
	}
	for name, body := range map[string]string{
		"unknown": `{"schema":"irrigator.admission/v1","typo_field":1}`,
		"schema":  `{"schema":"other/v9"}`,
		"dwell":   `{"schema":"irrigator.admission/v1","exit_dwell_seconds":-1}`,
		"floor":   `{"schema":"irrigator.admission/v1","power":{"battery_floor_percent":150}}`,
		"slots":   `{"schema":"irrigator.admission/v1","power":{"on_battery_local_slots":-5}}`,
	} {
		if _, err := resolve(write(name+".json", body)); err == nil {
			t.Errorf("%s: an invalid config was accepted", name)
		}
	}
	if _, err := resolve("no-such-preset-or-file"); err == nil || !strings.Contains(err.Error(), "preset") {
		t.Errorf("an unknown spec should explain the choices: %v", err)
	}
}

func TestLoadRunPersistsAnExplicitSpecAndReloadsIt(t *testing.T) {
	root := t.TempDir()
	if e, err := LoadRun(root, "", 4, Deps{}); e != nil || err != nil {
		t.Fatalf("no spec and no saved config must be nil: %v %v", e, err)
	}
	e, err := LoadRun(root, "quiet", 4, Deps{})
	if err != nil || e == nil {
		t.Fatalf("quiet: %v %v", e, err)
	}
	var saved config
	raw, err := os.ReadFile(filepath.Join(root, ConfigFile))
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Preset != "quiet" {
		t.Fatalf("admission.json was not saved: %v %s", err, raw)
	}
	again, err := LoadRun(root, "", 4, Deps{})
	if err != nil || again == nil {
		t.Fatalf("a saved config should reload without a spec: %v %v", again, err)
	}
	// Choosing unbounded explicitly turns it off and is remembered.
	if off, err := LoadRun(root, "unbounded", 4, Deps{}); off != nil || err != nil {
		t.Fatalf("unbounded: %v %v", off, err)
	}
	if off, err := LoadRun(root, "", 4, Deps{}); off != nil || err != nil {
		t.Fatalf("unbounded should persist: %v %v", off, err)
	}
}

func TestLoadRunRejectsACorruptSavedConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFile), []byte(`{"schema":"irrigator.admission/v1","surprise":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRun(root, "", 4, Deps{}); err == nil {
		t.Fatal("a saved config with an unknown field was accepted")
	}
}

func TestOverrideSensor(t *testing.T) {
	path := filepath.Join(t.TempDir(), OverrideFile)
	o := newOverride(path)
	sample := func() Reading { return o.sample(nil, runSnapshot{}) } //nolint:staticcheck
	if r := sample(); r.hold != holdNone || r.probe != probeOK || r.MaxLocalSlots != Unlimited {
		t.Fatalf("no file means no opinion: %+v", r)
	}
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"hold":"transient","reason":"demo in progress"}`)
	if r := sample(); r.hold != HoldTransient || r.Reason != "demo in progress" || r.severity != hard {
		t.Fatalf("hold: %+v", r)
	}
	write(`{"max_local_slots":2,"allow_remote":false}`)
	if r := sample(); r.MaxLocalSlots != 2 || r.allowRemote || r.hold != holdNone {
		t.Fatalf("limits: %+v", r)
	}
	write(`{"hold":"permanent"}`)
	if r := sample(); r.hold != HoldPermanent {
		t.Fatalf("permanent: %+v", r)
	}
	for _, body := range []string{`{"hold":"sometimes"}`, `{"nope":1}`, `not json`} {
		write(body)
		if r := sample(); r.probe != probeFailed {
			t.Fatalf("%q should fail the probe (and so fail closed), got %+v", body, r)
		}
	}
}
