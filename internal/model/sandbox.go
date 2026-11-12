package model

// SandboxSpec optionally runs a command provider inside a container. It is a
// convenience boundary for filesystem and network reach, not a hardened one.
type SandboxSpec struct {
	Runtime      string            `json:"runtime,omitempty"`
	Image        string            `json:"image"`
	Network      string            `json:"network,omitempty"`
	Mounts       []SandboxMount    `json:"mounts,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	ReadOnlyRoot bool              `json:"read_only_root,omitempty"`
	Env          []string          `json:"env,omitempty"`
	Resources    *SandboxResources `json:"resources,omitempty"`
}

type SandboxMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly *bool  `json:"read_only,omitempty"`
}
