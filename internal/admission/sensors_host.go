package admission

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// PowerInfo is what a power probe learned. Unknown values are -1.
type PowerInfo struct {
	OnAC         bool
	BatteryPct   int  // -1 when there is no battery or it is unknown
	LowPowerMode bool // macOS Low Power Mode
	// ThermalLimitPct is the share of full speed the CPU is allowed, 100 when
	// not throttled, -1 when unknown.
	ThermalLimitPct int
}

// PowerProbe reads the current power and thermal state.
type PowerProbe func(ctx context.Context) (PowerInfo, error)

// PowerThermal limits local admission on battery, low battery, Low Power Mode and
// thermal pressure. Everything it limits is local work except a battery that has
// fallen below its floor, which holds all new admission until power returns.
type PowerThermal struct {
	cfg   PowerConfig
	probe PowerProbe
}

// NewPowerThermal returns the sensor over probe.
func NewPowerThermal(cfg PowerConfig, probe PowerProbe) *PowerThermal {
	return &PowerThermal{cfg: cfg, probe: probe}
}

func (p *PowerThermal) Name() string { return "power" }

func (p *PowerThermal) Sample(ctx context.Context, _ RunSnapshot) Reading {
	r := OpenReading(p.Name())
	info, err := p.probe(ctx)
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	capLocal := func(slots int, why string, sev Severity) {
		if s := limit(slots); s < r.MaxLocalSlots {
			r.MaxLocalSlots, r.Reason, r.Severity = s, why, sev
		}
	}
	if !info.OnAC && info.BatteryPct >= 0 && info.BatteryPct < p.cfg.BatteryFloorPercent {
		r.Hold, r.Severity = HoldTransient, Hard
		r.Reason = fmt.Sprintf("battery %d%% is below the %d%% floor", info.BatteryPct, p.cfg.BatteryFloorPercent)
		return r
	}
	switch {
	case info.ThermalLimitPct >= 0 && info.ThermalLimitPct < 50:
		if p.cfg.ThermalCriticalHold {
			r.Hold, r.Severity = HoldTransient, Hard
			r.Reason = fmt.Sprintf("thermal critical (CPU limited to %d%%)", info.ThermalLimitPct)
			return r
		}
		capLocal(0, fmt.Sprintf("thermal critical (CPU limited to %d%%)", info.ThermalLimitPct), Hard)
	case info.ThermalLimitPct >= 0 && info.ThermalLimitPct < 80:
		capLocal(p.cfg.ThermalSeriousLocalSlots, fmt.Sprintf("thermal serious (CPU limited to %d%%)", info.ThermalLimitPct), Soft)
		capTier(&r, p.cfg.ThermalSeriousMaxTier)
	case info.ThermalLimitPct >= 0 && info.ThermalLimitPct < 100:
		capLocal(p.cfg.ThermalFairLocalSlots, fmt.Sprintf("thermal fair (CPU limited to %d%%)", info.ThermalLimitPct), Soft)
	}
	if info.LowPowerMode {
		capLocal(p.cfg.LowPowerLocalSlots, "Low Power Mode is on", Soft)
	}
	if !info.OnAC {
		capLocal(p.cfg.OnBatteryLocalSlots, "running on battery", Soft)
		capTier(&r, p.cfg.OnBatteryMaxTier)
	}
	return r
}

// NetworkInfo is what a network probe learned.
type NetworkInfo struct {
	Online  bool
	Metered bool // a phone hotspot or similar
	Detail  string
}

// NetworkProbe reads the current connectivity.
type NetworkProbe func(ctx context.Context) (NetworkInfo, error)

// Network stops remote admission when there is no route, and limits it on a
// metered link. It never limits local work: an Ollama task needs no network.
type Network struct {
	cfg   NetworkConfig
	probe NetworkProbe
}

// NewNetwork returns the sensor over probe.
func NewNetwork(cfg NetworkConfig, probe NetworkProbe) *Network {
	return &Network{cfg: cfg, probe: probe}
}

func (n *Network) Name() string { return "network" }

func (n *Network) Sample(ctx context.Context, _ RunSnapshot) Reading {
	r := OpenReading(n.Name())
	info, err := n.probe(ctx)
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	switch {
	case !info.Online:
		r.AllowRemote, r.Severity, r.Reason = false, Hard, "offline"
	case info.Metered:
		if s := limit(n.cfg.MeteredRemoteSlots); s == 0 {
			r.AllowRemote, r.Severity, r.Reason = false, Soft, "metered link: "+info.Detail
		} else if s < r.MaxRemoteSlots {
			r.MaxRemoteSlots, r.Severity, r.Reason = s, Soft, "metered link: "+info.Detail
		}
	}
	return r
}

var (
	battSource  = regexp.MustCompile(`Now drawing from '([^']+)'`)
	battPercent = regexp.MustCompile(`(\d+)%`)
	thermLimit  = regexp.MustCompile(`CPU_Speed_Limit\s*=\s*(\d+)`)
	lowPower    = regexp.MustCompile(`(?m)^\s*lowpowermode\s+(\d)`)
	routeGW     = regexp.MustCompile(`gateway:\s*(\S+)`)
	routeIface  = regexp.MustCompile(`interface:\s*(\S+)`)
)

// ParsePmsetBatt reads `pmset -g batt`.
func ParsePmsetBatt(out string) (onAC bool, pct int) {
	pct = -1
	if m := battSource.FindStringSubmatch(out); m != nil {
		onAC = m[1] == "AC Power"
	} else {
		onAC = true // no source line: assume mains rather than throttle on a guess
	}
	if m := battPercent.FindStringSubmatch(out); m != nil {
		pct, _ = strconv.Atoi(m[1])
	}
	return onAC, pct
}

// ParsePmsetTherm reads `pmset -g therm`: the CPU speed limit in percent, 100 when
// the system reports none.
func ParsePmsetTherm(out string) int {
	if m := thermLimit.FindStringSubmatch(out); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 100
}

// ParseLowPowerMode reads `pmset -g` for the Low Power Mode setting.
func ParseLowPowerMode(out string) bool {
	m := lowPower.FindStringSubmatch(out)
	return m != nil && m[1] == "1"
}

// ParseDefaultRoute reads `route -n get default`: the interface and gateway.
func ParseDefaultRoute(out string) (iface, gateway string) {
	if m := routeIface.FindStringSubmatch(out); m != nil {
		iface = m[1]
	}
	if m := routeGW.FindStringSubmatch(out); m != nil {
		gateway = m[1]
	}
	return iface, gateway
}

// hotspotGateways are the default gateways of phone tethering, which is metered.
var hotspotGateways = map[string]bool{"172.20.10.1": true, "192.168.43.1": true}

// IsHotspotGateway reports whether gateway is a known tethering gateway.
func IsHotspotGateway(gateway string) bool { return hotspotGateways[gateway] }
