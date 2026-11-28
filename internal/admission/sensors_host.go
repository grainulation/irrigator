package admission

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// powerInfo is what a power probe learned. Unknown values are -1.
type powerInfo struct {
	onAC         bool
	batteryPct   int  // -1 when there is no battery or it is unknown
	lowPowerMode bool // macOS Low Power Mode
	// thermalLimitPct is the share of full speed the CPU is allowed, 100 when
	// not throttled, -1 when unknown.
	thermalLimitPct int
}

// powerProbe reads the current power and thermal state.
type powerProbe func(ctx context.Context) (powerInfo, error)

// powerThermal limits local admission on battery, low battery, Low Power Mode and
// thermal pressure. Everything it limits is local work except a battery that has
// fallen below its floor, which holds all new admission until power returns.
type powerThermal struct {
	cfg   powerConfig
	probe powerProbe
}

// newPowerThermal returns the sensor over probe.
func newPowerThermal(cfg powerConfig, probe powerProbe) *powerThermal {
	return &powerThermal{cfg: cfg, probe: probe}
}

func (p *powerThermal) sensorName() string { return "power" }

func (p *powerThermal) sample(ctx context.Context, _ runSnapshot) Reading {
	r := OpenReading(p.sensorName())
	info, err := p.probe(ctx)
	if err != nil {
		r.probe, r.probeErr = probeFailed, err.Error()
		return r
	}
	capLocal := func(slots int, why string, sev severity) {
		if s := limit(slots); s < r.MaxLocalSlots {
			r.MaxLocalSlots, r.Reason, r.severity = s, why, sev
		}
	}
	if !info.onAC && info.batteryPct >= 0 && info.batteryPct < p.cfg.BatteryFloorPercent {
		r.hold, r.severity = HoldTransient, hard
		r.Reason = fmt.Sprintf("battery %d%% is below the %d%% floor", info.batteryPct, p.cfg.BatteryFloorPercent)
		return r
	}
	switch {
	case info.thermalLimitPct >= 0 && info.thermalLimitPct < 50:
		if p.cfg.ThermalCriticalHold {
			r.hold, r.severity = HoldTransient, hard
			r.Reason = fmt.Sprintf("thermal critical (CPU limited to %d%%)", info.thermalLimitPct)
			return r
		}
		capLocal(0, fmt.Sprintf("thermal critical (CPU limited to %d%%)", info.thermalLimitPct), hard)
		capTier(&r, p.cfg.ThermalSeriousMaxTier) // at least as strict as serious
	case info.thermalLimitPct >= 0 && info.thermalLimitPct < 80:
		capLocal(p.cfg.ThermalSeriousLocalSlots, fmt.Sprintf("thermal serious (CPU limited to %d%%)", info.thermalLimitPct), soft)
		capTier(&r, p.cfg.ThermalSeriousMaxTier)
	case info.thermalLimitPct >= 0 && info.thermalLimitPct < 100:
		capLocal(p.cfg.ThermalFairLocalSlots, fmt.Sprintf("thermal fair (CPU limited to %d%%)", info.thermalLimitPct), soft)
	}
	if info.lowPowerMode {
		capLocal(p.cfg.LowPowerLocalSlots, "Low Power Mode is on", soft)
	}
	if !info.onAC {
		capLocal(p.cfg.OnBatteryLocalSlots, "running on battery", soft)
		capTier(&r, p.cfg.OnBatteryMaxTier)
	}
	return r
}

// networkInfo is what a network probe learned.
type networkInfo struct {
	online  bool
	metered bool // a phone hotspot or similar
	detail  string
}

// networkProbe reads the current connectivity.
type networkProbe func(ctx context.Context) (networkInfo, error)

// network stops remote admission when there is no route, and limits it on a
// metered link. It never limits local work: an Ollama task needs no network.
type network struct {
	cfg   networkConfig
	probe networkProbe
}

// newNetwork returns the sensor over probe.
func newNetwork(cfg networkConfig, probe networkProbe) *network {
	return &network{cfg: cfg, probe: probe}
}

func (n *network) sensorName() string { return "network" }

func (n *network) sample(ctx context.Context, _ runSnapshot) Reading {
	r := OpenReading(n.sensorName())
	info, err := n.probe(ctx)
	if err != nil {
		r.probe, r.probeErr = probeFailed, err.Error()
		return r
	}
	switch {
	case !info.online:
		r.allowRemote, r.severity, r.Reason = false, hard, "offline"
	case info.metered:
		if s := limit(n.cfg.MeteredRemoteSlots); s == 0 {
			r.allowRemote, r.severity, r.Reason = false, soft, "metered link: "+info.detail
		} else if s < r.maxRemoteSlots {
			r.maxRemoteSlots, r.severity, r.Reason = s, soft, "metered link: "+info.detail
		}
	}
	return r
}

var (
	battSource  = regexp.MustCompile(`Now drawing from '([^']+)'`)
	battPercent = regexp.MustCompile(`(\d+)%`)
	thermLimit  = regexp.MustCompile(`CPU_Speed_Limit\s*=\s*(\d+)`)
	lowPower    = regexp.MustCompile(`(?m)^\s*lowpowermode\s+(\d)`)
	routeGW     = regexp.MustCompile(`gateway:[ \t]*(\S+)`)
	routeIface  = regexp.MustCompile(`interface:[ \t]*(\S+)`)
)

// parsePmsetBatt reads `pmset -g batt`.
func parsePmsetBatt(out string) (onAC bool, pct int) {
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

// parsePmsetTherm reads `pmset -g therm`: the CPU speed limit in percent, 100 when
// the system reports none.
func parsePmsetTherm(out string) int {
	if m := thermLimit.FindStringSubmatch(out); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 100
}

// parseLowPowerMode reads `pmset -g` for the Low Power Mode setting.
func parseLowPowerMode(out string) bool {
	m := lowPower.FindStringSubmatch(out)
	return m != nil && m[1] == "1"
}

// parseDefaultRoute reads `route -n get default`: the interface and gateway.
func parseDefaultRoute(out string) (iface, gateway string) {
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

// isHotspotGateway reports whether gateway is a known tethering gateway.
func isHotspotGateway(gateway string) bool { return hotspotGateways[gateway] }
