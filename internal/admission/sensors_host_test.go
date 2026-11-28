package admission

import (
	"context"
	"errors"
	"testing"
)

const battOnAC = `Now drawing from 'AC Power'
 -InternalBattery-0 (id=4653155)	100%; charged; 0:00 remaining present: true
`
const battLow = `Now drawing from 'Battery Power'
 -InternalBattery-0 (id=4653155)	12%; discharging; 0:41 remaining present: true
`
const thermNone = `Note: No thermal warning level has been recorded
Note: No performance warning level has been recorded
Note: No CPU power status has been recorded
`
const thermLimited = `CPU Power notify
	CPU_Scheduler_Limit 	= 100
	CPU_Available_CPUs 	= 10
	CPU_Speed_Limit 	= 65
`
const pmsetAll = `System-wide power settings:
Currently in use:
 standby              1
 lowpowermode         1
 powernap             1
`
const routeOK = `   route to: default
destination: default
       mask: default
    gateway: 172.20.10.1
  interface: en0
`

func TestParsePmset(t *testing.T) {
	if ac, pct := parsePmsetBatt(battOnAC); !ac || pct != 100 {
		t.Errorf("on AC: %v %d", ac, pct)
	}
	if ac, pct := parsePmsetBatt(battLow); ac || pct != 12 {
		t.Errorf("on battery: %v %d", ac, pct)
	}
	if ac, pct := parsePmsetBatt("Now drawing from 'AC Power'\n"); !ac || pct != -1 {
		t.Errorf("a desktop with no battery: %v %d", ac, pct)
	}
	if ac, _ := parsePmsetBatt(""); !ac {
		t.Error("an unparseable battery report must not be read as running on battery")
	}
	if got := parsePmsetTherm(thermNone); got != 100 {
		t.Errorf("no throttling: %d", got)
	}
	if got := parsePmsetTherm(thermLimited); got != 65 {
		t.Errorf("limited CPU: %d", got)
	}
	if !parseLowPowerMode(pmsetAll) || parseLowPowerMode("lowpowermode 0\n") {
		t.Error("low power mode parsing")
	}
	iface, gw := parseDefaultRoute(routeOK)
	if iface != "en0" || gw != "172.20.10.1" || !isHotspotGateway(gw) || isHotspotGateway("192.168.1.1") {
		t.Errorf("route: %q %q", iface, gw)
	}
}

func testPowerProbe(info powerInfo, err error) powerProbe {
	return func(context.Context) (powerInfo, error) { return info, err }
}

func TestPowerThermalLimitsLocalWorkAndHoldsOnALowBattery(t *testing.T) {
	cfg := presets["balanced"].Power
	sample := func(info powerInfo) Reading {
		return newPowerThermal(*cfg, testPowerProbe(info, nil)).sample(context.Background(), runSnapshot{})
	}
	if r := sample(powerInfo{onAC: true, batteryPct: 100, thermalLimitPct: 100}); r.MaxLocalSlots != Unlimited || r.hold != holdNone {
		t.Errorf("on AC and cool: %+v", r)
	}
	if r := sample(powerInfo{onAC: false, batteryPct: 10, thermalLimitPct: 100}); r.hold != HoldTransient || r.severity != hard {
		t.Errorf("below the battery floor must hold: %+v", r)
	}
	if r := sample(powerInfo{onAC: true, batteryPct: 10, thermalLimitPct: 100}); r.hold != holdNone {
		t.Errorf("a low battery that is charging must not hold: %+v", r)
	}
	if r := sample(powerInfo{onAC: true, batteryPct: 100, thermalLimitPct: 65}); r.MaxLocalSlots != 1 || r.maxRemoteSlots != Unlimited {
		t.Errorf("serious thermal limits local work only: %+v", r)
	}
	if r := sample(powerInfo{onAC: true, batteryPct: 100, thermalLimitPct: 30}); r.hold != HoldTransient {
		t.Errorf("critical thermal must hold: %+v", r)
	}
	if r := sample(powerInfo{onAC: true, batteryPct: 100, lowPowerMode: true, thermalLimitPct: 100}); r.MaxLocalSlots != 2 {
		t.Errorf("Low Power Mode: %+v", r)
	}
	// The strictest of several local limits wins.
	if r := sample(powerInfo{onAC: true, batteryPct: 100, lowPowerMode: true, thermalLimitPct: 65}); r.MaxLocalSlots != 1 {
		t.Errorf("thermal 1 and low power 2 should give 1: %+v", r)
	}
}

func TestPowerThermalProbeErrorIsAFailedProbe(t *testing.T) {
	r := newPowerThermal(*presets["balanced"].Power, testPowerProbe(powerInfo{}, errors.New("pmset missing"))).sample(context.Background(), runSnapshot{})
	if r.probe != probeFailed || r.probeErr == "" || r.MaxLocalSlots != Unlimited || r.hold != holdNone {
		t.Fatalf("a failed probe should report failure and nothing else: %+v", r)
	}
}

func TestNetworkStopsRemoteWhenOfflineAndLimitsItWhenMeteredButNeverTouchesLocal(t *testing.T) {
	probe := func(info networkInfo) networkProbe {
		return func(context.Context) (networkInfo, error) { return info, nil }
	}
	balanced, quiet := *presets["balanced"].Network, *presets["quiet"].Network
	r := newNetwork(balanced, probe(networkInfo{online: false})).sample(context.Background(), runSnapshot{})
	if r.allowRemote || r.severity != hard || r.MaxLocalSlots != Unlimited {
		t.Errorf("offline: %+v", r)
	}
	r = newNetwork(balanced, probe(networkInfo{online: true, metered: true, detail: "hotspot"})).sample(context.Background(), runSnapshot{})
	if !r.allowRemote || r.maxRemoteSlots != 2 || r.severity != soft {
		t.Errorf("metered under balanced should cap remote at 2: %+v", r)
	}
	r = newNetwork(quiet, probe(networkInfo{online: true, metered: true, detail: "hotspot"})).sample(context.Background(), runSnapshot{})
	if r.allowRemote || r.severity != soft {
		t.Errorf("metered under quiet should pause remote as a soft constraint: %+v", r)
	}
	r = newNetwork(balanced, probe(networkInfo{online: true})).sample(context.Background(), runSnapshot{})
	if !r.allowRemote || r.maxRemoteSlots != Unlimited {
		t.Errorf("a normal link: %+v", r)
	}
}
