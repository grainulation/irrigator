package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// fakeRuntime installs executables named names first on PATH. "run" records
// argv, one argument per line, and echoes a valid swarm.worker/v1 response;
// any other verb is recorded in killFile. With hang set, "run" sleeps instead.
func fakeRuntime(t *testing.T, names []string, hang bool) (argvFile, killFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	killFile = filepath.Join(dir, "kill")
	run := `id=$(sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
printf '{"protocol":"swarm.worker/v1","request_id":"%s","output":{"ok":"yes"}}' "$id"`
	if hang {
		run = "sleep 30"
	}
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then\n  for a in \"$@\"; do echo \"$a\"; done > '" + argvFile + "'\n  " + run + "\nelse\n  echo \"$@\" >> '" + killFile + "'\nfi\n"
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return argvFile, killFile
}

func sandboxRequest() model.WorkRequest {
	return model.WorkRequest{RunID: "abcdef0123456789", Protocol: model.ProtocolVersion, Kind: "task", RequestID: "req-1", OperationID: "op-1",
		Task: model.Task{ID: "build", Requires: []string{"ok"}}, Attempt: 2, TimeoutMS: 5000, MaxCostUSD: 1}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func TestSandboxArgvShapePerRuntime(t *testing.T) {
	for _, rt := range []string{"container", "docker", "podman"} {
		t.Run(rt, func(t *testing.T) {
			argvFile, _ := fakeRuntime(t, []string{rt}, false)
			cell := t.TempDir()
			mount := t.TempDir()
			ro := false
			spec := model.CommandSpec{Command: []string{"python3", "worker.py"}, Env: []string{"HOSTONLY"}, Sandbox: &model.SandboxSpec{
				Runtime: rt, Image: "python:3.13-alpine", ReadOnlyRoot: true, Env: []string{"API_KEY"},
				Mounts: []model.SandboxMount{{Source: mount, Target: "/data"}, {Source: mount, Target: "/scratch", ReadOnly: &ro}},
			}}
			t.Setenv("API_KEY", "s3cret-value")
			resp, err := External(context.Background(), cell, filepath.Join(cell, "1"), spec, sandboxRequest())
			if err != nil {
				t.Fatal(err)
			}
			if resp.Output["ok"] != "yes" {
				t.Fatalf("response not decoded: %+v", resp)
			}
			argv := readLines(t, argvFile)
			if len(argv) < 12 || argv[0] != "run" || argv[1] != "--rm" || argv[2] != "-i" || argv[3] != "--name" {
				t.Fatalf("unexpected argv prefix: %q", argv)
			}
			name := argv[4]
			if !strings.HasPrefix(name, "irrigator-abcdef01-build-2-") || len(name) <= len("irrigator-abcdef01-build-2-") {
				t.Fatalf("bad container name %q", name)
			}
			want := []string{"--network", "none", "-v", cell + ":/work", "-w", "/work", "-v", mount + ":/data:ro", "-v", mount + ":/scratch", "-e", "API_KEY", "--read-only", "python:3.13-alpine", "python3", "worker.py"}
			if got := argv[5:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("argv tail\n got %q\nwant %q", got, want)
			}
			if strings.Contains(strings.Join(argv, " "), "s3cret-value") {
				t.Fatal("secret value leaked into argv")
			}
		})
	}
}

func TestSandboxNetworkDefaultsToNoneAndAllowsDefault(t *testing.T) {
	argvFile, _ := fakeRuntime(t, []string{"docker"}, false)
	cell := t.TempDir()
	for network, want := range map[string]string{"": "none", "none": "none", "default": "default"} {
		spec := model.CommandSpec{Command: []string{"w"}, Sandbox: &model.SandboxSpec{Runtime: "docker", Image: "img", Network: network}}
		if _, err := External(context.Background(), cell, filepath.Join(cell, "n"), spec, sandboxRequest()); err != nil {
			t.Fatal(err)
		}
		argv := readLines(t, argvFile)
		if argv[5] != "--network" || argv[6] != want {
			t.Fatalf("network %q: argv %q", network, argv)
		}
	}
}

func TestSandboxEnvValuesReachClientNotArgv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	script := "#!/bin/sh\nprintf '%s|%s' \"$SANDBOX_TOKEN\" \"$NOT_ALLOWED\" > '" + envFile + "'\nid=$(sed -n 's/.*\"request_id\":\"\\([^\"]*\\)\".*/\\1/p')\nprintf '{\"protocol\":\"swarm.worker/v1\",\"request_id\":\"%s\",\"output\":{\"ok\":\"yes\"}}' \"$id\"\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("SANDBOX_TOKEN", "tok-123")
	t.Setenv("NOT_ALLOWED", "x")
	cell := t.TempDir()
	spec := model.CommandSpec{Command: []string{"w"}, Sandbox: &model.SandboxSpec{Runtime: "podman", Image: "img", Env: []string{"SANDBOX_TOKEN"}}}
	if _, err := External(context.Background(), cell, filepath.Join(cell, "e"), spec, sandboxRequest()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(envFile); string(got) != "tok-123|" {
		t.Fatalf("runtime client environment wrong: %q", got)
	}
}

func TestSandboxKillsContainerOnTimeout(t *testing.T) {
	_, killFile := fakeRuntime(t, []string{"docker"}, true)
	cell := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	spec := model.CommandSpec{Command: []string{"w"}, Sandbox: &model.SandboxSpec{Runtime: "docker", Image: "img"}}
	_, err := External(ctx, cell, filepath.Join(cell, "k"), spec, sandboxRequest())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if model.KnownNotStarted(err) || !model.IsUncertain(err) {
		t.Fatalf("interrupted provider call must be uncertain: %v", err)
	}
	kills := readLines(t, killFile)
	if len(kills) != 1 || !strings.HasPrefix(kills[0], "kill irrigator-abcdef01-build-2-") {
		t.Fatalf("expected one kill of the named container, got %q", kills)
	}
}

func TestSandboxAutoSelectionOrder(t *testing.T) {
	cases := []struct {
		have []string
		want string
	}{
		{[]string{"docker", "podman", "container"}, "container"},
		{[]string{"docker", "podman"}, "podman"},
		{[]string{"docker"}, "docker"},
	}
	for _, c := range cases {
		want := c.want
		if want == "container" && runtime.GOOS != "darwin" {
			want = "podman"
		}
		argv, _ := fakeRuntime(t, c.have, false)
		// Only the fakes: a real runtime in /usr/bin (podman ships on Ubuntu runners) must not win.
		t.Setenv("PATH", filepath.Dir(argv))
		got, err := launch.SandboxRuntimePath("auto")
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != want {
			t.Fatalf("have %v: picked %s, want %s", c.have, got, want)
		}
	}
}

func TestSandboxAutoUnavailableIsNotStarted(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cell := t.TempDir()
	spec := model.CommandSpec{Command: []string{"w"}, Sandbox: &model.SandboxSpec{Image: "img"}}
	_, err := External(context.Background(), cell, filepath.Join(cell, "u"), spec, sandboxRequest())
	if err == nil || !model.KnownNotStarted(err) || !strings.Contains(err.Error(), "sandbox runtime unavailable") {
		t.Fatalf("expected not_started unavailable error, got %v", err)
	}
	spec.Sandbox.Runtime = "docker"
	if _, err = External(context.Background(), cell, filepath.Join(cell, "u"), spec, sandboxRequest()); err == nil || !model.KnownNotStarted(err) {
		t.Fatalf("explicit missing runtime must be not_started, got %v", err)
	}
}

func writeSandboxPlan(t *testing.T, providers string) (string, model.Plan, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	plan := `{"name":"sb","providers":` + providers + `,"tasks":[{"id":"a","provider":"box","prompt":"x","requires":["ok"]}]}`
	if err := os.WriteFile(path, []byte(plan), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := checkpoint.LoadPlan(path)
	return path, p, err
}

func TestSandboxValidationErrors(t *testing.T) {
	cases := map[string]string{
		`{}`:                                                          "image is required",
		`{"image":"img","runtime":"lxc"}`:                             "runtime",
		`{"image":"-rm"}`:                                             "not a valid image",
		`{"image":"a b"}`:                                             "not a valid image",
		`{"image":"img","network":"host"}`:                            "network",
		`{"image":"img","workdir":"rel"}`:                             "workdir",
		`{"image":"img","env":["A=b"]}`:                               "variable name",
		`{"image":"img","env":["1BAD"]}`:                              "variable name",
		`{"image":"img","mounts":[{"target":"/t"}]}`:                  "source is required",
		`{"image":"img","mounts":[{"source":"/a","target":"t"}]}`:     "target",
		`{"image":"img","mounts":[{"source":"/a:b","target":"/t"}]}`:  "source",
		`{"image":"img","mounts":[{"source":"/a","target":"/work"}]}`: "duplicates",
		`{"image":"img","mounts":[{"source":"/a","target":"/t"},{"source":"/b","target":"/t/"}]}`: "duplicates",
	}
	for sandbox, want := range cases {
		_, _, err := writeSandboxPlan(t, `{"box":{"command":["w"],"sandbox":`+sandbox+`}}`)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "provider box sandbox") {
			t.Errorf("%s: want error containing %q, got %v", sandbox, want, err)
		}
	}
}

func TestSandboxPlanLoadResolvesRelativeMountsAndKeepsPlainProvidersUnchanged(t *testing.T) {
	path, p, err := writeSandboxPlan(t, `{"box":{"command":["w"],"sandbox":{"image":"img","mounts":[{"source":"data","target":"/d"},{"source":"/abs","target":"/e","read_only":false}]}},"plain":{"command":["w"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	m := p.Providers["box"].Sandbox.Mounts
	if m[0].Source != filepath.Join(filepath.Dir(path), "data") || m[1].Source != "/abs" {
		t.Fatalf("mount sources not resolved against plan dir: %+v", m)
	}
	if m[1].ReadOnly == nil || *m[1].ReadOnly {
		t.Fatalf("explicit read_only=false lost: %+v", m[1])
	}
	raw, _ := json.Marshal(p.Providers["plain"])
	if string(raw) != `{"command":["w"]}` {
		t.Fatalf("plain provider bytes changed: %s", raw)
	}
}
