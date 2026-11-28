//go:build !darwin

package admission

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("host probe is not implemented on this platform")

// Off macOS the host sensors report a failed probe, which their fail-open policy
// treats as no opinion and shows as degraded.
func hostPowerProbe(context.Context) (powerInfo, error)     { return powerInfo{}, errUnsupported }
func hostNetworkProbe(context.Context) (networkInfo, error) { return networkInfo{}, errUnsupported }
