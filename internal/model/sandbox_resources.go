package model

// SandboxResources configures the container boundary, not a total CPU-time or
// host-wide memory budget. 1000 CPU millis is one CPU of bandwidth for Linux
// runtimes, or one guest vCPU for Apple container. Optional PID bounds are only
// supported on Docker/Podman. Nil applies no explicit resource limits.
type SandboxResources struct {
	CPUMillis   int   `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
	PidsLimit   int   `json:"pids_limit,omitempty"`
}
