//go:build !darwin

package admission

import "context"

func hostMemoryProbe(context.Context) (memoryInfo, error)     { return memoryInfo{}, errUnsupported }
func hostPresenceProbe(context.Context) (presenceInfo, error) { return presenceInfo{}, errUnsupported }
