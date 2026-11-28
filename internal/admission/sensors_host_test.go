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
	if ac, pct := ParsePmsetBatt(battOnAC); !ac || pct != 100 {
		t.Errorf("on AC: %v %d", ac, pct)
	}
	if ac, pct := ParsePmsetBatt(battLow); ac || pct != 12 {
		t.Errorf("on battery: %v %d", ac, pct)
	}
	if ac, pct := ParsePmsetBatt("Now drawing from 'AC Power'\n"); !ac || pct != -1 {
		t.Errorf("a desktop with no battery: %v %d", ac, pct)
	}
	if ac, _ := ParsePmsetBatt(""); !ac {
		t.Error("an unparseable battery report must not be read as running on battery")
	}
	if got := ParsePmsetTherm(thermNone); got != 100 {
		t.Errorf("no throttling: %d", got)
	}
	if got := ParsePmsetTherm(thermLimited); got != 65 {
		t.Errorf("limited CPU: %d", got)
	}
	if !ParseLowPowerMode(pmsetAll) || ParseLowPowerMode("lowpowermode 0\n") {
		t.Error("low power mode parsing")
	}
	iface, gw := ParseDefaultRoute(routeOK)
	if iface != "en0" || gw != "172.20.10.1" || !IsHotspotGateway(gw) || IsHotspotGateway("192.168.1.1") {
		t.Errorf("route: %q %q", iface, gw)
	}
}

func powerProbe(info PowerInfo, err error) PowerProbe {
	return func(context.Context) (PowerInfo, error) { return info, err }
}

func TestPowerThermalLimitsLocalWorkAndHoldsOnALowBattery(t *testing.T) {
	cfg := presets["balanced"].Power
	sample := func(info PowerInfo) Reading {
		return NewPowerThermal(*cfg, powerProbe(info, nil)).Sample(context.Background(), RunSnapshot{})
	}
	if r := sample(PowerInfo{OnAC: true, BatteryPct: 100, ThermalLimitPct: 100}); r.MaxLocalSlots != Unlimited || r.Hold != HoldNone {
		t.Errorf("on AC and cool: %+v", r)
	}
	if r := sample(PowerInfo{OnAC: false, BatteryPct: 10, ThermalLimitPct: 100}); r.Hold != HoldTransient || r.Severity != Hard {
		t.Errorf("below the battery floor must hold: %+v", r)
	}
	if r := sample(PowerInfo{OnAC: true, BatteryPct: 10, ThermalLimitPct: 100}); r.Hold != HoldNone {
		t.Errorf("a low battery that is charging must not hold: %+v", r)
	}
	if r := sample(PowerInfo{OnAC: true, BatteryPct: 100, ThermalLimitPct: 65}); r.MaxLocalSlots != 1 || r.MaxRemoteSlots != Unlimited {
		t.Errorf("serious thermal limits local work only: %+v", r)
	}
	if r := sample(PowerInfo{OnAC: true, BatteryPct: 100, ThermalLimitPct: 30}); r.Hold != HoldTransient {
		t.Errorf("critical thermal must hold: %+v", r)
	}
	if r := sample(PowerInfo{OnAC: true, BatteryPct: 100, LowPowerMode: true, ThermalLimitPct: 100}); r.MaxLocalSlots != 2 {
		t.Errorf("Low Power Mode: %+v", r)
	}
	// The strictest of several local limits wins.
	if r := sample(PowerInfo{OnAC: true, BatteryPct: 100, LowPowerMode: true, ThermalLimitPct: 65}); r.MaxLocalSlots != 1 {
		t.Errorf("thermal 1 and low power 2 should give 1: %+v", r)
	}
}

func TestPowerThermalProbeErrorIsAFailedProbe(t *testing.T) {
	r := NewPowerThermal(*presets["balanced"].Power, powerProbe(PowerInfo{}, errors.New("pmset missing"))).Sample(context.Background(), RunSnapshot{})
	if r.Probe != ProbeFailed || r.ProbeErr == "" || r.MaxLocalSlots != Unlimited || r.Hold != HoldNone {
		t.Fatalf("a failed probe should report failure and nothing else: %+v", r)
	}
}

func TestNetworkStopsRemoteWhenOfflineAndLimitsItWhenMeteredButNeverTouchesLocal(t *testing.T) {
	probe := func(info NetworkInfo) NetworkProbe {
		return func(context.Context) (NetworkInfo, error) { return info, nil }
	}
	balanced, quiet := *presets["balanced"].Network, *presets["quiet"].Network
	r := NewNetwork(balanced, probe(NetworkInfo{Online: false})).Sample(context.Background(), RunSnapshot{})
	if r.AllowRemote || r.Severity != Hard || r.MaxLocalSlots != Unlimited {
		t.Errorf("offline: %+v", r)
	}
	r = NewNetwork(balanced, probe(NetworkInfo{Online: true, Metered: true, Detail: "hotspot"})).Sample(context.Background(), RunSnapshot{})
	if !r.AllowRemote || r.MaxRemoteSlots != 2 || r.Severity != Soft {
		t.Errorf("metered under balanced should cap remote at 2: %+v", r)
	}
	r = NewNetwork(quiet, probe(NetworkInfo{Online: true, Metered: true, Detail: "hotspot"})).Sample(context.Background(), RunSnapshot{})
	if r.AllowRemote || r.Severity != Soft {
		t.Errorf("metered under quiet should pause remote as a soft constraint: %+v", r)
	}
	r = NewNetwork(balanced, probe(NetworkInfo{Online: true})).Sample(context.Background(), RunSnapshot{})
	if !r.AllowRemote || r.MaxRemoteSlots != Unlimited {
		t.Errorf("a normal link: %+v", r)
	}
}
