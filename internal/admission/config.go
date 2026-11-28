package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/jsonio"
)

// ConfigFile and OverrideFile are the sidecars in the run directory. Neither is
// part of the checkpoint, so the plan hash and the state schema are untouched.
const (
	ConfigFile   = "admission.json"
	OverrideFile = "admission-override.json"
	configSchema = "irrigator.admission/v1"
)

// ActiveSpec is the --admission value of this process, a preset name or a config
// file path. It follows the same pattern as the sizing path.
var ActiveSpec string

// powerConfig sets what the power and thermal sensor does. A limit of -1 means no
// limit and 0 means none may start. A tier field, when set, caps the model tier
// that may start while that condition holds.
type powerConfig struct {
	BatteryFloorPercent      int    `json:"battery_floor_percent"`
	OnBatteryLocalSlots      int    `json:"on_battery_local_slots"`
	LowPowerLocalSlots       int    `json:"low_power_local_slots"`
	ThermalFairLocalSlots    int    `json:"thermal_fair_local_slots"`
	ThermalSeriousLocalSlots int    `json:"thermal_serious_local_slots"`
	ThermalCriticalHold      bool   `json:"thermal_critical_hold"`
	OnBatteryMaxTier         string `json:"on_battery_max_tier,omitempty"`
	ThermalSeriousMaxTier    string `json:"thermal_serious_max_tier,omitempty"`
}

// networkConfig sets what the network sensor does. MeteredRemoteSlots applies to
// a hotspot or other metered link; an offline link always stops remote work.
type networkConfig struct {
	MeteredRemoteSlots int `json:"metered_remote_slots"`
}

// config is the contents of admission.json.
type config struct {
	Schema              string  `json:"schema"`
	Preset              string  `json:"preset"`
	ExitDwellSeconds    float64 `json:"exit_dwell_seconds"`
	RampThrottleSeconds float64 `json:"ramp_throttle_seconds"`
	// Override makes the controller honor admission-override.json, so an
	// operator can throttle or hold a run by hand.
	Override bool `json:"override"`

	Power    *powerConfig    `json:"power,omitempty"`
	Network  *networkConfig  `json:"network,omitempty"`
	Memory   *memoryConfig   `json:"memory,omitempty"`
	Disk     *diskConfig     `json:"disk,omitempty"`
	Presence *presenceConfig `json:"presence,omitempty"`
	OffPeak  *offPeakConfig  `json:"offpeak,omitempty"`
	// RateLimit and Breaker learn from how this run's own attempts end.
	RateLimit *rateLimitConfig `json:"rate_limit,omitempty"`
	Breaker   *breakerConfig   `json:"failure_breaker,omitempty"`
	// ProfileTiers declares, per managed profile name, the model tier of that
	// profile: minimal, reduced or full. Only declared profiles are ever filtered
	// by tier, and nothing is inferred from model names.
	ProfileTiers map[string]string `json:"profile_tiers,omitempty"`
}

var (
	defaultRateLimit = rateLimitConfig{DefaultCooldownSeconds: 60, MaxCooldownSeconds: 900, Headroom: 1}
	defaultBreaker   = breakerConfig{Alpha: 0.2, Threshold: 0.5, MinOutcomes: 5, IntervalSeconds: 20, OpenSeconds: 60, HalfLifeSeconds: 120}
	defaultDisk      = diskConfig{LowFreeMB: 5120, CriticalFreeMB: 1024, LowLocalSlots: 1}
)

// Presets are the named configurations. unbounded restricts nothing and is the
// default: a run without --admission behaves exactly as it always has.
var presets = map[string]config{
	"unbounded": {Schema: configSchema, Preset: "unbounded"},
	"balanced": {Schema: configSchema, Preset: "balanced", ExitDwellSeconds: 30, RampThrottleSeconds: 10, Override: true,
		Power:     &powerConfig{BatteryFloorPercent: 15, OnBatteryLocalSlots: -1, LowPowerLocalSlots: 2, ThermalFairLocalSlots: -1, ThermalSeriousLocalSlots: 1, ThermalCriticalHold: true},
		Network:   &networkConfig{MeteredRemoteSlots: 2},
		Memory:    &memoryConfig{WarnLocalSlots: 2, CriticalHold: true},
		Disk:      &defaultDisk,
		RateLimit: &defaultRateLimit,
		Breaker:   &defaultBreaker},
	"quiet": {Schema: configSchema, Preset: "quiet", ExitDwellSeconds: 60, RampThrottleSeconds: 15, Override: true,
		Power:     &powerConfig{BatteryFloorPercent: 30, OnBatteryLocalSlots: 2, LowPowerLocalSlots: 1, ThermalFairLocalSlots: 2, ThermalSeriousLocalSlots: 1, ThermalCriticalHold: true},
		Network:   &networkConfig{MeteredRemoteSlots: 0},
		Memory:    &memoryConfig{WarnLocalSlots: 1, CriticalHold: true, SwapUsedPercentLimit: 60, SwapLocalSlots: 1},
		Disk:      &defaultDisk,
		Presence:  &presenceConfig{ActiveWithinSeconds: 120, ActiveLocalSlots: 1},
		RateLimit: &defaultRateLimit,
		Breaker:   &defaultBreaker},
	"offpeak": {Schema: configSchema, Preset: "offpeak", ExitDwellSeconds: 30, RampThrottleSeconds: 10, Override: true,
		Power:     &powerConfig{BatteryFloorPercent: 15, OnBatteryLocalSlots: -1, LowPowerLocalSlots: 2, ThermalFairLocalSlots: -1, ThermalSeriousLocalSlots: 1, ThermalCriticalHold: true},
		Network:   &networkConfig{MeteredRemoteSlots: 2},
		Memory:    &memoryConfig{WarnLocalSlots: 2, CriticalHold: true},
		Disk:      &defaultDisk,
		OffPeak:   &offPeakConfig{Window: "22:00-07:00", OutsideLocalSlots: 1, OutsideRemoteSlots: 1},
		RateLimit: &defaultRateLimit,
		Breaker:   &defaultBreaker},
}

// presetNames lists the built-in presets.
func presetNames() []string { return []string{"unbounded", "balanced", "quiet", "offpeak"} }

func checkSlots(name string, v int) error {
	if v < -1 || v > 1024 {
		return fmt.Errorf("%s must be -1 (no limit) or 0..1024", name)
	}
	return nil
}

func checkTier(name, v string) error {
	if v == "" {
		return nil
	}
	if _, err := parseTier(v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// validate rejects a configuration that cannot be enforced.
func (c config) validate() error {
	if c.Schema != configSchema {
		return fmt.Errorf("admission config schema must be %q", configSchema)
	}
	for name, v := range c.numbers() {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a finite number", name)
		}
	}
	if c.ExitDwellSeconds < 0 || c.ExitDwellSeconds > 3600 || c.RampThrottleSeconds < 0 || c.RampThrottleSeconds > 3600 {
		return errors.New("exit_dwell_seconds and ramp_throttle_seconds must be 0..3600")
	}
	if p := c.Power; p != nil {
		if p.BatteryFloorPercent < 0 || p.BatteryFloorPercent > 100 {
			return errors.New("battery_floor_percent must be 0..100")
		}
		for name, v := range map[string]int{"on_battery_local_slots": p.OnBatteryLocalSlots, "low_power_local_slots": p.LowPowerLocalSlots,
			"thermal_fair_local_slots": p.ThermalFairLocalSlots, "thermal_serious_local_slots": p.ThermalSeriousLocalSlots} {
			if err := checkSlots(name, v); err != nil {
				return err
			}
		}
		if err := checkTier("on_battery_max_tier", p.OnBatteryMaxTier); err != nil {
			return err
		}
		if err := checkTier("thermal_serious_max_tier", p.ThermalSeriousMaxTier); err != nil {
			return err
		}
	}
	if n := c.Network; n != nil {
		if err := checkSlots("metered_remote_slots", n.MeteredRemoteSlots); err != nil {
			return err
		}
	}
	if m := c.Memory; m != nil {
		if err := checkSlots("warn_local_slots", m.WarnLocalSlots); err != nil {
			return err
		}
		if err := checkSlots("swap_local_slots", m.SwapLocalSlots); err != nil {
			return err
		}
		if m.SwapUsedPercentLimit < 0 || m.SwapUsedPercentLimit > 100 {
			return errors.New("swap_used_percent_limit must be 0..100")
		}
	}
	if d := c.Disk; d != nil {
		if d.CriticalFreeMB < 0 || d.LowFreeMB < d.CriticalFreeMB || d.LockWaitMS < 0 || d.LockWaitMS > 60000 {
			return errors.New("disk: critical_free_mb must be >= 0, low_free_mb >= critical_free_mb, and lock_wait_ms 0..60000")
		}
		if d.CriticalFreeMB == 0 && d.LowFreeMB == 0 && d.LockWaitMS == 0 {
			return errors.New("disk: set critical_free_mb, low_free_mb or lock_wait_ms, or leave the section out")
		}
		if err := checkSlots("low_local_slots", d.LowLocalSlots); err != nil {
			return err
		}
		if err := checkSlots("lock_wait_local_slots", d.LockWaitLocalSlots); err != nil {
			return err
		}
	}
	if p := c.Presence; p != nil {
		if p.ActiveWithinSeconds <= 0 || p.ActiveWithinSeconds > 86400 {
			return errors.New("presence: active_within_seconds must be 1..86400")
		}
		if err := checkSlots("active_local_slots", p.ActiveLocalSlots); err != nil {
			return err
		}
	}
	if o := c.OffPeak; o != nil {
		if _, _, err := parseWindow(o.Window); err != nil {
			return err
		}
		if err := checkSlots("outside_local_slots", o.OutsideLocalSlots); err != nil {
			return err
		}
		if err := checkSlots("outside_remote_slots", o.OutsideRemoteSlots); err != nil {
			return err
		}
		if o.SignalFile != "" && !filepath.IsAbs(o.SignalFile) {
			return errors.New("signal_file must be an absolute path")
		}
	}
	if r := c.RateLimit; r != nil {
		if r.DefaultCooldownSeconds <= 0 || r.MaxCooldownSeconds < r.DefaultCooldownSeconds || r.Headroom < 0 || r.Headroom > 1024 {
			return errors.New("rate_limit: default_cooldown_seconds must be > 0 and <= max_cooldown_seconds, headroom 0..1024")
		}
	}
	if b := c.Breaker; b != nil {
		if b.Alpha <= 0 || b.Alpha > 1 || b.Threshold <= 0 || b.Threshold >= 1 || b.MinOutcomes < 1 || b.IntervalSeconds <= 0 || b.OpenSeconds <= 0 || b.HalfLifeSeconds <= 0 {
			return errors.New("failure_breaker: alpha in (0,1], threshold in (0,1), min_outcomes >= 1, interval_seconds, open_seconds and half_life_seconds > 0")
		}
	}
	for profile, tier := range c.ProfileTiers {
		if profile == "" {
			return errors.New("profile_tiers has an empty profile name")
		}
		if _, err := parseTier(tier); err != nil {
			return fmt.Errorf("profile_tiers[%q]: %w", profile, err)
		}
	}
	return nil
}

// numbers lists the fractional settings, which a JSON file cannot make
// non-finite but a program could.
func (c config) numbers() map[string]float64 {
	m := map[string]float64{"exit_dwell_seconds": c.ExitDwellSeconds, "ramp_throttle_seconds": c.RampThrottleSeconds}
	if p := c.Presence; p != nil {
		m["active_within_seconds"] = p.ActiveWithinSeconds
	}
	if r := c.RateLimit; r != nil {
		m["default_cooldown_seconds"], m["max_cooldown_seconds"] = r.DefaultCooldownSeconds, r.MaxCooldownSeconds
	}
	if b := c.Breaker; b != nil {
		m["alpha"], m["threshold"], m["interval_seconds"], m["open_seconds"], m["half_life_seconds"] = b.Alpha, b.Threshold, b.IntervalSeconds, b.OpenSeconds, b.HalfLifeSeconds
	}
	return m
}

// active reports whether the configuration restricts anything.
func (c config) active() bool {
	return c.Power != nil || c.Network != nil || c.Override || c.Memory != nil || c.Disk != nil ||
		c.Presence != nil || c.OffPeak != nil || c.RateLimit != nil || c.Breaker != nil
}

// resolve turns a --admission value into a validated configuration: a preset
// name, or the path of a JSON file with unknown fields rejected.
func resolve(spec string) (config, error) {
	if c, ok := presets[spec]; ok {
		return c, nil
	}
	raw, err := os.ReadFile(spec)
	if err != nil {
		return config{}, fmt.Errorf("--admission %q is neither a preset (%v) nor a readable file: %w", spec, presetNames(), err)
	}
	var c config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return config{}, fmt.Errorf("invalid admission config %s: %w", spec, err)
	}
	if c.Preset == "" {
		c.Preset = "custom"
	}
	if err = c.validate(); err != nil {
		return config{}, err
	}
	if !c.active() && c.Preset != "unbounded" {
		return config{}, fmt.Errorf("admission config %s limits nothing: a preset name (%v) is given on the command line, not as a \"preset\" field; add sections, or use unbounded to run without limits", spec, presetNames())
	}
	return c, nil
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

func limit(v int) int {
	if v < 0 {
		return Unlimited
	}
	return v
}

// Deps are the pieces of the controller's world that the sensors need but that
// this package must not import.
type Deps struct {
	// DiagnoseLimit inspects one finished attempt's captured provider output.
	// Without it the rate-limit sensor is not registered.
	DiagnoseLimit limitDiagnoser
}

// LoadRun opens the admission configuration of a run. An explicit spec is saved
// as RUN/admission.json; without one the saved file is used if there is one. It
// returns nil when the run is unbounded, so the default costs the controller
// nothing.
func LoadRun(root, spec string, workers int, deps Deps) (*Evaluator, error) {
	// The controller that starts holds the run's controller lock, so any status
	// file already here belongs to a controller that is gone. Readers only show
	// a status while the lock is held, which would otherwise make it look current.
	_ = os.Remove(filepath.Join(root, StatusFile))
	side := filepath.Join(root, ConfigFile)
	var cfg config
	if spec != "" {
		c, err := resolve(spec)
		if err != nil {
			return nil, err
		}
		if err = jsonio.AtomicJSON(side, c); err != nil {
			return nil, err
		}
		cfg = c
	} else {
		raw, err := os.ReadFile(side)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", side, err)
		}
		if err = cfg.validate(); err != nil {
			return nil, err
		}
	}
	if !cfg.active() {
		return nil, nil
	}
	return newRunEvaluator(root, cfg, workers, clock.ActiveClock, false, deps)
}

// capTier lowers r's tier ceiling to the named tier when one is configured.
func capTier(r *Reading, name string) {
	if name == "" {
		return
	}
	if t, err := parseTier(name); err == nil && t < r.modelTier {
		r.modelTier = t
	}
}

// lockWaitProbe measures how long the checkpoint lock takes to become available
// to a reader, polling without blocking, up to two seconds.
func lockWaitProbe(root string) lockWaitFn {
	return func(ctx context.Context) (time.Duration, error) {
		f, err := os.OpenFile(filepath.Join(root, "state.lock"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		start := time.Now()
		for {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
			if err == nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				return time.Since(start), nil
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				return 0, err
			}
			if time.Since(start) >= 2*time.Second || ctx.Err() != nil {
				return time.Since(start), nil
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// newRunEvaluator builds the evaluator for cfg. sync samples inline (tests).
func newRunEvaluator(root string, cfg config, workers int, clk clock.Clock, sync bool, deps Deps) (*Evaluator, error) {
	var regs []registration
	add := func(s sensor, p failurePolicy, ttl time.Duration, inline bool) {
		regs = append(regs, registration{sensor: s, policy: p, ttl: ttl, inline: inline})
	}
	add(newRunCap(), failOpen, 0, true)
	if cfg.Power != nil {
		add(newPowerThermal(*cfg.Power, hostPowerProbe), failOpen, 15*time.Second, false)
	}
	if cfg.Network != nil {
		add(newNetwork(*cfg.Network, hostNetworkProbe), failOpen, 10*time.Second, false)
	}
	if cfg.Memory != nil {
		add(newMemory(*cfg.Memory, hostMemoryProbe), failOpen, 10*time.Second, false)
	}
	if cfg.Disk != nil {
		add(newDisk(*cfg.Disk, diskStatfs(root)), failClosed, 30*time.Second, false)
		if cfg.Disk.LockWaitMS > 0 {
			add(newLockWait(*cfg.Disk, lockWaitProbe(root)), failOpen, 10*time.Second, false)
		}
	}
	if cfg.Presence != nil {
		add(newPresence(*cfg.Presence, hostPresenceProbe), failOpen, 5*time.Second, false)
	}
	if cfg.OffPeak != nil {
		sensor, err := newOffPeak(*cfg.OffPeak, clk)
		if err != nil {
			return nil, err
		}
		add(sensor, failOpen, 0, true)
		if cfg.OffPeak.SignalFile != "" {
			add(newOverrideNamed("offpeak-signal", cfg.OffPeak.SignalFile), failOpen, 30*time.Second, false)
		}
	}
	if cfg.RateLimit != nil && deps.DiagnoseLimit != nil {
		add(newRateLimit(root, *cfg.RateLimit, workers, clk, deps.DiagnoseLimit), failOpen, 0, true)
	}
	if cfg.Breaker != nil {
		add(newBreaker(*cfg.Breaker, workers, clk), failOpen, 0, true)
	}
	if cfg.Override {
		add(newOverride(filepath.Join(root, OverrideFile)), failClosed, 2*time.Second, false)
	}
	tiers := map[string]tier{}
	for profile, name := range cfg.ProfileTiers {
		t, err := parseTier(name)
		if err != nil {
			return nil, err
		}
		tiers[profile] = t
	}
	return newEvaluator(options{clock: clk, workers: workers, exitDwell: seconds(cfg.ExitDwellSeconds), rampThrottle: seconds(cfg.RampThrottleSeconds),
		preset: cfg.Preset, statusPath: filepath.Join(root, StatusFile), sync: sync, tier: profileTiers(tiers)}, regs...), nil
}
