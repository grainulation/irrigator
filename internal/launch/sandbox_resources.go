package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

const SandboxMiB int64 = 1024 * 1024

func validateSandboxResources(s *model.SandboxSpec) error {
	if s == nil || s.Resources == nil {
		return nil
	}
	r := s.Resources
	if s.Runtime != "docker" && s.Runtime != "podman" && s.Runtime != "container" {
		return errors.New("resource limits require explicit runtime docker, podman or container; auto is not allowed")
	}
	if r.CPUMillis < 10 || r.CPUMillis > 1024000 {
		return errors.New("resources.cpu_millis must be 10..1024000")
	}
	if r.MemoryBytes < 6*SandboxMiB || r.MemoryBytes > 1<<40 {
		return errors.New("resources.memory_bytes must be 6 MiB..1 TiB")
	}
	if r.PidsLimit < 0 || r.PidsLimit > 1048576 {
		return errors.New("resources.pids_limit must be 1..1048576 when provided")
	}
	if s.Runtime == "container" {
		if r.CPUMillis%1000 != 0 {
			return errors.New("Apple container resources.cpu_millis must allocate whole guest vCPUs")
		}
		if r.MemoryBytes%SandboxMiB != 0 {
			return errors.New("Apple container resources.memory_bytes must be a whole MiB")
		}
		if r.PidsLimit != 0 {
			return errors.New("Apple container does not support the pids_limit contract")
		}
	}
	return nil
}
func sandboxResourceArgv(s model.SandboxSpec) []string {
	if s.Resources == nil {
		return nil
	}
	r := s.Resources
	cpu := strconv.Itoa(r.CPUMillis/1000) + "." + fmt.Sprintf("%03d", r.CPUMillis%1000)
	memory := strconv.FormatInt(r.MemoryBytes, 10)
	if s.Runtime == "container" {
		cpu = strconv.Itoa(r.CPUMillis / 1000)
		memory = strconv.FormatInt(r.MemoryBytes/SandboxMiB, 10) + "M"
	}
	args := []string{"--cpus", cpu, "--memory", memory}
	if s.Runtime == "docker" || s.Runtime == "podman" {
		// Equal memory+swap and memory disallows additional swap allowance.
		args = append(args, "--memory-swap", memory)
		if r.PidsLimit > 0 {
			args = append(args, "--pids-limit", strconv.Itoa(r.PidsLimit))
		}
	}
	return args
}

// planHostResourceLimits is a declaration about explicitly selected provider
// configurations. Dispatch still probes the actual runtime and fails closed.
// No default backend is inferred from a plan: every task needs a named provider.
func PlanHostResourceLimits(p model.Plan) bool {
	if len(p.Tasks) == 0 || IsManagedPlanVersion(p.APIVersion) {
		return false
	}
	for _, t := range p.Tasks {
		provider, ok := p.Providers[t.Provider]
		if !ok || provider.Sandbox == nil || provider.Sandbox.Resources == nil || validateSandboxResources(provider.Sandbox) != nil {
			return false
		}
	}
	return true
}

func sandboxProbeEnv(spec model.CommandSpec) []string {
	names := append([]string{"PATH", "HOME", "TMPDIR", "TEMP", "SystemRoot"}, spec.Env...)
	if spec.Sandbox != nil {
		names = append(names, spec.Sandbox.Env...)
	}
	env := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		if !seen[name] {
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
			seen[name] = true
		}
	}
	return env
}

// Refuse unverified or unavailable runtime controller support before issuing
// run. The runtime/daemon remains trusted; this cannot attest a malicious host.
func probeSandboxResources(bin string, spec model.CommandSpec) error {
	s := spec.Sandbox
	if s == nil || s.Resources == nil {
		return nil
	}
	if err := validateSandboxResources(s); err != nil {
		return err
	}
	args := []string{"info", "--format", "{{json .}}"}
	if s.Runtime == "podman" {
		args = []string{"info", "--format", "json"}
	}
	if s.Runtime == "container" {
		args = []string{"run", "--help"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = sandboxProbeEnv(spec)
	cmd.WaitDelay = 100 * time.Millisecond
	var out AuthStatusBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil || ctx.Err() != nil || out.Overflow {
		return errors.New("sandbox resource preflight failed; refusing unbounded dispatch")
	}
	if s.Runtime == "container" {
		help := out.String()
		if !strings.Contains(help, "--cpus") || !strings.Contains(help, "--memory") {
			return errors.New("Apple container resource flags unavailable; refusing dispatch")
		}
		return nil
	}
	if s.Runtime == "docker" {
		var info struct {
			OSType        string
			CgroupVersion string
			CPUCfsQuota   bool
			MemoryLimit   bool
			SwapLimit     bool
			PidsLimit     bool
		}
		if json.Unmarshal(out.Bytes(), &info) != nil || info.OSType != "linux" || info.CgroupVersion != "2" || !info.CPUCfsQuota || !info.MemoryLimit || !info.SwapLimit || (s.Resources.PidsLimit > 0 && !info.PidsLimit) {
			return errors.New("Docker does not report required Linux cgroup v2 CPU/memory/swap/PID controllers; refusing dispatch")
		}
		return nil
	}
	var info struct {
		Host struct {
			CgroupVersion string   `json:"cgroupVersion"`
			Controllers   []string `json:"cgroupControllers"`
		} `json:"host"`
	}
	if json.Unmarshal(out.Bytes(), &info) != nil || info.Host.CgroupVersion != "v2" {
		return errors.New("Podman does not report cgroup v2; refusing dispatch")
	}
	controllers := map[string]bool{}
	for _, c := range info.Host.Controllers {
		controllers[c] = true
	}
	if !controllers["cpu"] || !controllers["memory"] || (s.Resources.PidsLimit > 0 && !controllers["pids"]) {
		return errors.New("Podman required resource controllers are unavailable; refusing dispatch")
	}
	return nil
}
