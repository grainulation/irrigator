//go:build !darwin

package admission

import "context"

func hostMemoryProbe(context.Context) (MemoryInfo, error)     { return MemoryInfo{}, errUnsupported }
func hostPresenceProbe(context.Context) (PresenceInfo, error) { return PresenceInfo{}, errUnsupported }
