package launch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

const defaultSandboxWorkdir = "/work"

var (
	sandboxEnvName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sandboxImageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	sandboxNameClean = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
)

func validSandboxRuntime(name string) bool {
	switch name {
	case "", "auto", "container", "docker", "podman":
		return true
	}
	return false
}

func validateSandboxPath(kind, p string) error {
	if p == "" || !path.IsAbs(p) {
		return fmt.Errorf("%s %q must be an absolute path", kind, p)
	}
	if strings.ContainsAny(p, ":,\x00\n") {
		return fmt.Errorf("%s %q must not contain ':', ',' or control characters", kind, p)
	}
	return nil
}

func ValidateSandbox(provider string, s *model.SandboxSpec) error {
	if s == nil {
		return nil
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("provider %s sandbox: %s", provider, fmt.Sprintf(format, args...))
	}
	if !validSandboxRuntime(s.Runtime) {
		return fail("runtime %q must be auto, container, docker or podman", s.Runtime)
	}
	if s.Image == "" {
		return fail("image is required")
	}
	if !sandboxImageName.MatchString(s.Image) {
		return fail("image %q is not a valid image reference", s.Image)
	}
	if err := validateSandboxResources(s); err != nil {
		return fail("%v", err)
	}
	if s.Network != "" && s.Network != "none" && s.Network != "default" {
		return fail("network %q must be none or default", s.Network)
	}
	workdir := s.Workdir
	if workdir == "" {
		workdir = defaultSandboxWorkdir
	}
	if err := validateSandboxPath("workdir", workdir); err != nil {
		return fail("%v", err)
	}
	targets := map[string]bool{path.Clean(workdir): true}
	for i, m := range s.Mounts {
		if m.Source == "" {
			return fail("mounts[%d].source is required", i)
		}
		if strings.ContainsAny(m.Source, ":,\x00\n") {
			return fail("mounts[%d].source %q must not contain ':', ',' or control characters", i, m.Source)
		}
		if err := validateSandboxPath(fmt.Sprintf("mounts[%d].target", i), m.Target); err != nil {
			return fail("%v", err)
		}
		t := path.Clean(m.Target)
		if targets[t] {
			return fail("mounts[%d].target %q duplicates the workdir or another mount", i, m.Target)
		}
		targets[t] = true
	}
	for _, name := range s.Env {
		if !sandboxEnvName.MatchString(name) {
			return fail("env entry %q must be a variable name, never NAME=value", name)
		}
	}
	return nil
}

// resolveSandboxPaths makes relative mount sources absolute against the plan
// file's directory (or the working directory for a plan read from stdin), so
// the admitted plan never depends on where a later run starts.
func ResolveSandboxPaths(p *model.Plan, planPath string) {
	base := "."
	if planPath != "-" {
		base = filepath.Dir(planPath)
	}
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	for name, spec := range p.Providers {
		if spec.Sandbox == nil || len(spec.Sandbox.Mounts) == 0 {
			continue
		}
		mounts := append([]model.SandboxMount(nil), spec.Sandbox.Mounts...)
		for i := range mounts {
			if mounts[i].Source != "" && !filepath.IsAbs(mounts[i].Source) {
				mounts[i].Source = filepath.Join(base, mounts[i].Source)
			}
		}
		sb := *spec.Sandbox
		sb.Mounts = mounts
		spec.Sandbox = &sb
		p.Providers[name] = spec
	}
}

// sandboxRuntimePath picks the container runtime binary. Absence is positive
// evidence that nothing was dispatched.
func SandboxRuntimePath(name string) (string, error) {
	candidates := []string{name}
	switch name {
	case "", "auto":
		candidates = nil
		if runtime.GOOS == "darwin" {
			candidates = append(candidates, "container")
		}
		candidates = append(candidates, "podman", "docker")
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", model.MarkNotStarted(fmt.Errorf("sandbox runtime unavailable: none of %s found on PATH", strings.Join(candidates, ", ")))
}

func sandboxContainerName(req model.WorkRequest) string {
	run := sandboxNameClean.ReplaceAllString(req.RunID, "")
	if len(run) > 8 {
		run = run[:8]
	}
	cell := sandboxNameClean.ReplaceAllString(req.Task.ID, "-")
	if len(cell) > 40 {
		cell = cell[:40]
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("irrigator-%s-%s-%d-%s", run, cell, req.Attempt, hex.EncodeToString(b[:]))
}

// sandboxArgv builds the runtime argv. Environment values are never included:
// `-e NAME` makes the runtime read the value from its own environment.
func sandboxArgv(s model.SandboxSpec, name, cell string, command []string) []string {
	network := s.Network
	if network == "" {
		network = "none"
	}
	workdir := s.Workdir
	if workdir == "" {
		workdir = defaultSandboxWorkdir
	}
	argv := []string{"run", "--rm", "-i", "--name", name, "--network", network, "-v", cell + ":" + workdir, "-w", workdir}
	for _, m := range s.Mounts {
		v := m.Source + ":" + m.Target
		if m.ReadOnly == nil || *m.ReadOnly {
			v += ":ro"
		}
		argv = append(argv, "-v", v)
	}
	for _, e := range s.Env {
		argv = append(argv, "-e", e)
	}
	if s.ReadOnlyRoot {
		argv = append(argv, "--read-only")
	}
	argv = append(argv, sandboxResourceArgv(s)...)
	argv = append(argv, s.Image)
	return append(argv, command...)
}

// sandboxCommand returns the host command that runs spec inside its container,
// the runtime binary, and the container name.
func SandboxCommand(spec model.CommandSpec, cell string, req model.WorkRequest) (*exec.Cmd, string, string, error) {
	sb := spec.Sandbox
	if err := ValidateSandbox("dispatch", sb); err != nil {
		return nil, "", "", model.MarkNotStarted(err)
	}
	bin, err := SandboxRuntimePath(sb.Runtime)
	if err != nil {
		return nil, "", "", err
	}
	if err := probeSandboxResources(bin, spec); err != nil {
		return nil, "", "", model.MarkNotStarted(err)
	}
	abs, err := filepath.Abs(cell)
	if err != nil {
		return nil, "", "", model.MarkNotStarted(err)
	}
	for _, m := range sb.Mounts {
		if !filepath.IsAbs(m.Source) {
			return nil, "", "", model.MarkNotStarted(fmt.Errorf("sandbox mount source %q must be absolute", m.Source))
		}
	}
	name := sandboxContainerName(req)
	cmd := exec.Command(bin, sandboxArgv(*sb, name, abs, spec.Command)...)
	return cmd, bin, name, nil
}

// killSandboxContainer stops a possibly still-running container. Killing the
// runtime client does not necessarily stop it; --rm removes it afterwards.
// A missing container is the normal outcome after a clean exit.
func KillSandboxContainer(bin, name string, env []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "kill", name)
	cmd.Env = env
	_ = cmd.Run()
}
