//go:build darwin

package admission

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// hostPowerProbe reads battery, Low Power Mode and thermal limit with pmset.
func hostPowerProbe(ctx context.Context) (PowerInfo, error) {
	batt, err := run(ctx, "pmset", "-g", "batt")
	if err != nil {
		return PowerInfo{}, err
	}
	onAC, pct := ParsePmsetBatt(batt)
	info := PowerInfo{OnAC: onAC, BatteryPct: pct, ThermalLimitPct: -1}
	if out, err := run(ctx, "pmset", "-g", "therm"); err == nil {
		info.ThermalLimitPct = ParsePmsetTherm(out)
	}
	if out, err := run(ctx, "pmset", "-g"); err == nil {
		info.LowPowerMode = ParseLowPowerMode(out)
	}
	return info, nil
}

// hostNetworkProbe treats a missing default route as offline, and a tethering
// gateway as metered.
func hostNetworkProbe(ctx context.Context) (NetworkInfo, error) {
	out, err := run(ctx, "route", "-n", "get", "default")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return NetworkInfo{Online: false, Detail: "no default route"}, nil
		}
		return NetworkInfo{}, err
	}
	iface, gateway := ParseDefaultRoute(out)
	return NetworkInfo{Online: true, Metered: IsHotspotGateway(gateway), Detail: iface + " via " + gateway}, nil
}
