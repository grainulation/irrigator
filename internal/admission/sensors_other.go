//go:build !darwin

package admission

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("host probe is not implemented on this platform")

// Off macOS the host sensors report a failed probe, which their fail-open policy
// treats as no opinion and shows as degraded.
func hostPowerProbe(context.Context) (PowerInfo, error)     { return PowerInfo{}, errUnsupported }
func hostNetworkProbe(context.Context) (NetworkInfo, error) { return NetworkInfo{}, errUnsupported }
