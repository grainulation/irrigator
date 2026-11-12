package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func resourceRuntimeFixture(t *testing.T, rt, info string) (string, string) {
	t.Helper()
	argv, kill := fakeRuntime(t, []string{rt}, false)
	script := "#!/bin/sh\nif [ \"$1\" = info ]; then\ncat <<'IRR_INFO'\n" + info + "\nIRR_INFO\nelif [ \"$1\" = run ] && [ \"$2\" = --help ]; then\nprintf '%s\\n' '--cpus <cpus> --memory <memory>'\nelif [ \"$1\" = run ]; then\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done > '" + argv + "'\nid=$(sed -n 's/.*\"request_id\":\"\\([^\"]*\\)\".*/\\1/p')\nprintf '{\"protocol\":\"swarm.worker/v1\",\"request_id\":\"%s\",\"output\":{\"ok\":\"yes\"}}' \"$id\"\nelse\nprintf '%s\\n' \"$@\" >> '" + kill + "'\nfi\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(argv), rt), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return argv, kill
}

const resourceDockerInfo = `{"OSType":"linux","CgroupVersion":"2","CPUCfsQuota":true,"MemoryLimit":true,"SwapLimit":true,"PidsLimit":true}`
const resourcePodmanInfo = `{"host":{"cgroupVersion":"v2","cgroupControllers":["cpu","memory","pids"]}}`

func TestSandboxResourceValidation(t *testing.T) {
	cases := []struct {
		runtime string
		r       model.SandboxResources
		want    string
	}{
		{"auto", model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: 0}, "explicit runtime"},
		{"docker", model.SandboxResources{CPUMillis: 0, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: 0}, "cpu_millis"},
		{"docker", model.SandboxResources{CPUMillis: 1000, MemoryBytes: 0, PidsLimit: 0}, "memory_bytes"},
		{"docker", model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: -1}, "pids_limit"},
		{"container", model.SandboxResources{CPUMillis: 500, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: 0}, "whole guest"},
		{"container", model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64*launch.SandboxMiB + 1, PidsLimit: 0}, "whole MiB"},
		{"container", model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: 64}, "pids_limit"},
	}
	for _, c := range cases {
		spec := &model.SandboxSpec{Runtime: c.runtime, Image: "fixture", Resources: &c.r}
		if err := launch.ValidateSandbox("box", spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v wanted %q, got %v", c, c.want, err)
		}
	}
	for _, rt := range []string{"docker", "podman", "container"} {
		if err := launch.ValidateSandbox("box", &model.SandboxSpec{Runtime: rt, Image: "fixture", Resources: &model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64 * launch.SandboxMiB}}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestSandboxResourcesRuntimeArgvAndControllerPreflight(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container"} {
		t.Run(rt, func(t *testing.T) {
			info := resourceDockerInfo
			if rt == "podman" {
				info = resourcePodmanInfo
			}
			argv, _ := resourceRuntimeFixture(t, rt, info)
			r := &model.SandboxResources{CPUMillis: 1500, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: 64}
			if rt == "container" {
				r.CPUMillis = 2000
				r.PidsLimit = 0
			}
			spec := model.CommandSpec{Command: []string{"fixture-worker"}, Sandbox: &model.SandboxSpec{Runtime: rt, Image: "fixture", Resources: r}}
			cell := t.TempDir()
			response, err := External(context.Background(), cell, filepath.Join(cell, "attempt"), spec, sandboxRequest())
			if err != nil || response.Output["ok"] != "yes" {
				t.Fatalf("resource execution: %+v %v", response, err)
			}
			got := strings.Join(readLines(t, argv), "\n")
			want := []string{"--cpus\n1.500", "--memory\n67108864", "--memory-swap\n67108864", "--pids-limit\n64"}
			if rt == "container" {
				want = []string{"--cpus\n2", "--memory\n64M"}
				if strings.Contains(got, "--pids-limit") || strings.Contains(got, "--memory-swap") {
					t.Fatal("unsupported Apple flag emitted")
				}
			}
			for _, s := range want {
				if !strings.Contains(got, s) {
					t.Fatalf("resource flag %q missing from %q", s, got)
				}
			}
		})
	}
}
func TestSandboxResourcesRefuseMissingControllersBeforeRun(t *testing.T) {
	for _, c := range []struct{ rt, info string }{{"docker", `{}`}, {"docker", strings.Replace(resourceDockerInfo, `"MemoryLimit":true`, `"MemoryLimit":false`, 1)}, {"docker", strings.Replace(resourceDockerInfo, `"PidsLimit":true`, `"PidsLimit":false`, 1)}, {"podman", `{"host":{"cgroupVersion":"v1","cgroupControllers":["cpu","memory","pids"]}}`}, {"podman", `{"host":{"cgroupVersion":"v2","cgroupControllers":["cpu","memory"]}}`}} {
		t.Run(c.rt+c.info, func(t *testing.T) {
			argv, _ := resourceRuntimeFixture(t, c.rt, c.info)
			cell := t.TempDir()
			spec := model.CommandSpec{Command: []string{"fixture-worker"}, Sandbox: &model.SandboxSpec{Runtime: c.rt, Image: "fixture", Resources: &model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64 * launch.SandboxMiB, PidsLimit: 64}}}
			_, err := External(context.Background(), cell, filepath.Join(cell, "attempt"), spec, sandboxRequest())
			if err == nil || !model.KnownNotStarted(err) {
				t.Fatalf("missing controllers did not fail before dispatch: %v", err)
			}
			if _, err = os.Stat(argv); !os.IsNotExist(err) {
				t.Fatal("container run invoked despite failed preflight")
			}
		})
	}
}
func TestSandboxResourceDeclarationRequiresEveryNamedProvider(t *testing.T) {
	p := model.Plan{Tasks: []model.Task{{ID: "a", Provider: "box"}}, Providers: map[string]model.CommandSpec{"box": {Command: []string{"worker"}, Sandbox: &model.SandboxSpec{Runtime: "docker", Image: "fixture", Resources: &model.SandboxResources{CPUMillis: 1000, MemoryBytes: 64 * launch.SandboxMiB}}}}}
	if !launch.PlanHostResourceLimits(p) {
		t.Fatal("valid explicit container resources not declared")
	}
	p.Tasks = append(p.Tasks, model.Task{ID: "b"})
	if launch.PlanHostResourceLimits(p) {
		t.Fatal("unbounded default provider accepted")
	}
	p.Tasks = p.Tasks[:1]
	p.APIVersion = launch.ArtifactPlanVersion
	if launch.PlanHostResourceLimits(p) {
		t.Fatal("managed plan falsely declared container containment")
	}
}
