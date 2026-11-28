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
	for _, name := range PresetNames() {
		c, err := Resolve(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err = c.Validate(); err != nil {
			t.Fatalf("%s is invalid: %v", name, err)
		}
	}
	c, _ := Resolve("unbounded")
	if c.Active() {
		t.Fatal("unbounded must not be active: a run without --admission has to behave exactly as before")
	}
	if b, _ := Resolve("balanced"); !b.Active() || b.Power == nil || b.Network == nil {
		t.Fatal("balanced should enable the power and network sensors")
	}
}

func TestAllPresetsBuildTheirSensorsAndOffpeakHasAWindow(t *testing.T) {
	for _, name := range PresetNames() {
		cfg, _ := Resolve(name)
		if !cfg.Active() {
			continue
		}
		e, err := NewRunEvaluator(t.TempDir(), cfg, 6, newFakeClock(), true, Deps{DiagnoseLimit: func(string, string, int, time.Time) (LimitEvidence, error) { return LimitEvidence{}, nil }})
		if err != nil || e == nil {
			t.Fatalf("%s: %v", name, err)
		}
		e.Close()
	}
	o, _ := Resolve("offpeak")
	if o.OffPeak == nil || o.OffPeak.Window != "22:00-07:00" {
		t.Fatalf("offpeak preset: %+v", o.OffPeak)
	}
	if q, _ := Resolve("quiet"); q.Presence == nil {
		t.Fatal("quiet should yield to a user who is present")
	}
}

func TestRateLimitSensorIsNotRegisteredWithoutADiagnoser(t *testing.T) {
	cfg, _ := Resolve("balanced")
	e, err := NewRunEvaluator(t.TempDir(), cfg, 6, newFakeClock(), true, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range e.entries {
		if _, ok := en.reg.Sensor.(*RateLimit); ok {
			t.Fatal("the rate-limit sensor needs a diagnoser and must not be registered without one")
		}
	}
}

func TestPhase3AndTierConfigValidation(t *testing.T) {
	good, _ := Resolve("offpeak")
	bad := map[string]func(c *Config){
		"window":       func(c *Config) { c.OffPeak.Window = "late" },
		"signal path":  func(c *Config) { c.OffPeak.SignalFile = "relative.json" },
		"disk order":   func(c *Config) { c.Disk = &DiskConfig{LowFreeMB: 100, CriticalFreeMB: 500} },
		"swap percent": func(c *Config) { c.Memory = &MemoryConfig{SwapUsedPercentLimit: 150} },
		"presence":     func(c *Config) { c.Presence = &PresenceConfig{ActiveWithinSeconds: -1} },
		"breaker alpha": func(c *Config) {
			c.Breaker = &BreakerConfig{Alpha: 2, Threshold: .5, MinOutcomes: 1, IntervalSeconds: 1, OpenSeconds: 1, HalfLifeSeconds: 1}
		},
		"rate cooldown": func(c *Config) { c.RateLimit = &RateLimitConfig{DefaultCooldownSeconds: 100, MaxCooldownSeconds: 10} },
		"profile tier":  func(c *Config) { c.ProfileTiers = map[string]string{"p": "huge"} },
		"battery tier":  func(c *Config) { c.Power.OnBatteryMaxTier = "tiny" },
		"empty profile": func(c *Config) { c.ProfileTiers = map[string]string{"": "full"} },
	}
	for name, mutate := range bad {
		c := good
		power := *good.Power
		off := *good.OffPeak
		c.Power, c.OffPeak = &power, &off
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: an invalid config was accepted", name)
		}
	}
	ok := good
	ok.ProfileTiers = map[string]string{"heavy": "full", "light": "minimal"}
	if err := ok.Validate(); err != nil {
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
	if c, err := Resolve(ok); err != nil || c.Preset != "custom" || !c.Override {
		t.Fatalf("valid file: %+v %v", c, err)
	}
	for name, body := range map[string]string{
		"unknown": `{"schema":"irrigator.admission/v1","typo_field":1}`,
		"schema":  `{"schema":"other/v9"}`,
		"dwell":   `{"schema":"irrigator.admission/v1","exit_dwell_seconds":-1}`,
		"floor":   `{"schema":"irrigator.admission/v1","power":{"battery_floor_percent":150}}`,
		"slots":   `{"schema":"irrigator.admission/v1","power":{"on_battery_local_slots":-5}}`,
	} {
		if _, err := Resolve(write(name+".json", body)); err == nil {
			t.Errorf("%s: an invalid config was accepted", name)
		}
	}
	if _, err := Resolve("no-such-preset-or-file"); err == nil || !strings.Contains(err.Error(), "preset") {
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
	var saved Config
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
	o := NewOverride(path)
	sample := func() Reading { return o.Sample(nil, RunSnapshot{}) } //nolint:staticcheck
	if r := sample(); r.Hold != HoldNone || r.Probe != ProbeOK || r.MaxLocalSlots != Unlimited {
		t.Fatalf("no file means no opinion: %+v", r)
	}
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"hold":"transient","reason":"demo in progress"}`)
	if r := sample(); r.Hold != HoldTransient || r.Reason != "demo in progress" || r.Severity != Hard {
		t.Fatalf("hold: %+v", r)
	}
	write(`{"max_local_slots":2,"allow_remote":false}`)
	if r := sample(); r.MaxLocalSlots != 2 || r.AllowRemote || r.Hold != HoldNone {
		t.Fatalf("limits: %+v", r)
	}
	write(`{"hold":"permanent"}`)
	if r := sample(); r.Hold != HoldPermanent {
		t.Fatalf("permanent: %+v", r)
	}
	for _, body := range []string{`{"hold":"sometimes"}`, `{"nope":1}`, `not json`} {
		write(body)
		if r := sample(); r.Probe != ProbeFailed {
			t.Fatalf("%q should fail the probe (and so fail closed), got %+v", body, r)
		}
	}
}
