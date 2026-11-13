//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func fixture(t *testing.T, tasks ...model.Task) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(model.Plan{Name: "test", Tasks: tasks}, root, "mock"); err != nil {
		t.Fatal(err)
	}
	return root
}

func task(id string, delay float64, deps ...string) model.Task {
	return model.Task{ID: id, Prompt: "produce answer", Requires: []string{"answer"}, DependsOn: deps, MockDelay: delay}
}

func read(t *testing.T, root string) *model.State {
	t.Helper()
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func waitFor(t *testing.T, root string, check func(*model.State) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check(read(t, root)) {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("timed out waiting for state")
}

func start(t *testing.T, root string, opts ...string) (*exec.Cmd, <-chan error) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping process-spawning integration test in short mode")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"resume", root}, opts...)
	cmd := exec.Command(binary, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = checkpoint.SetControl(root, "down") })
	return cmd, done
}

func finished(t *testing.T, done <-chan error, wantSuccess bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err == nil) != wantSuccess {
			t.Fatalf("exit %v, expected success %v", err, wantSuccess)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not exit")
	}
}

func TestParallelDependenciesAndNoReplay(t *testing.T) {
	root := fixture(t, task("a", .5), task("b", .5), task("c", .1, "a", "b"))
	_, done := start(t, root)
	finished(t, done, true)
	s := read(t, root)
	seq := map[string]int{}
	for i, e := range s.Events {
		seq[e.Task+"/"+e.Kind] = i
	}
	if seq["b/started"] >= seq["a/verified"] {
		t.Fatal("independent tasks did not overlap")
	}
	if seq["c/started"] <= seq["a/verified"] || seq["c/started"] <= seq["b/verified"] {
		t.Fatal("dependency released early")
	}
	_, done = start(t, root)
	finished(t, done, true)
	for _, row := range read(t, root).Tasks {
		if row.Attempt != 1 {
			t.Fatal("replayed accepted task")
		}
	}
}

func TestDownResume(t *testing.T) {
	root := fixture(t, task("a", .1), task("b", 1, "a"))
	_, done := start(t, root)
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["b"].Status == "running" })
	if err := checkpoint.SetControl(root, "down"); err != nil {
		t.Fatal(err)
	}
	finished(t, done, true)
	if read(t, root).Tasks["b"].Status != "pending" {
		t.Fatal("interrupted task not requeued")
	}
	_, done = start(t, root)
	finished(t, done, true)
	s := read(t, root)
	if s.Tasks["a"].Attempt != 1 || s.Tasks["b"].Attempt != 2 {
		t.Fatal("wrong retry scope")
	}
}

func TestPauseDrains(t *testing.T) {
	root := fixture(t, task("a", .5), task("b", .1, "a"))
	_, done := start(t, root)
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	finished(t, done, true)
	s := read(t, root)
	if s.Tasks["a"].Status != "verified" || s.Tasks["b"].Attempt != 0 {
		t.Fatal("pause did not drain")
	}
	_, done = start(t, root)
	finished(t, done, true)
}

func TestCrashRecovery(t *testing.T) {
	for _, killWorker := range []bool{false, true} {
		name := "controller"
		if killWorker {
			name = "controller-and-worker"
		}
		t.Run(name, func(t *testing.T) {
			root := fixture(t, task("a", .1), task("b", 1, "a"))
			cmd, done := start(t, root)
			waitFor(t, root, func(s *model.State) bool { return s.Tasks["b"].Status == "running" })
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			finished(t, done, false)
			if killWorker {
				for _, e := range read(t, root).Events {
					if e.Task == "b" && e.Kind == "started" {
						pid := int(e.Detail.(map[string]any)["pid"].(float64))
						if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
							t.Fatal(err)
						}
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			_, done = start(t, root)
			finished(t, done, true)
			s := read(t, root)
			want := 1
			if killWorker {
				want = 2
			}
			if s.Tasks["a"].Attempt != 1 || s.Tasks["b"].Attempt != want {
				t.Fatal("unexpected replay")
			}
		})
	}
}

func TestProducedCheckpoint(t *testing.T) {
	root := fixture(t, task("a", .1))
	if err := checkpoint.Update(root, func(s *model.State) error {
		r := s.Tasks["a"]
		r.Status = "produced"
		r.Attempt = 1
		r.Result = map[string]string{"answer": "saved"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, done := start(t, root)
	finished(t, done, true)
	if r := read(t, root).Tasks["a"]; r.Attempt != 1 || r.Result["answer"] != "saved" {
		t.Fatal("checkpoint lost")
	}
}

func TestValidationFailureBlocksDependents(t *testing.T) {
	a := task("a", .1)
	a.Equals = map[string]string{"answer": "expected"}
	root := fixture(t, a, task("b", .1, "a"))
	_, done := start(t, root)
	finished(t, done, false)
	s := read(t, root)
	if s.Tasks["a"].Status != "failed" || s.Tasks["b"].Attempt != 0 {
		t.Fatal("bad dependency gate")
	}
}

func TestTimeoutAndRetry(t *testing.T) {
	root := fixture(t, task("a", .5))
	_, done := start(t, root, "--timeout", "100ms")
	finished(t, done, false)
	if read(t, root).Tasks["a"].Status != "failed" {
		t.Fatal("timeout not failed")
	}
	_, done = start(t, root, "--retry-failed", "--timeout", "2s")
	finished(t, done, true)
	if read(t, root).Tasks["a"].Attempt != 2 {
		t.Fatal("retry did not execute")
	}
}

func TestSingleController(t *testing.T) {
	// a must outlast the second controller's startup, which is slow on some hosts (WSL).
	root := fixture(t, task("a", 3))
	_, first := start(t, root)
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	_, second := start(t, root)
	finished(t, second, false)
	finished(t, first, true)
}

func TestHaikuAdapter(t *testing.T) {
	// Exercise actual subprocess IO/flags without spending model tokens in tests.
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	script := `#!/bin/sh
cat > received.json
printf '%s' '{"structured_output":{"answer":"ok"},"total_cost_usd":0.01}'
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	root := fixture(t, task("a", .1))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Backend = "haiku"
		model.SaveCallLimits(s, model.Options{Budget: .25, Timeout: 120 * time.Second})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, done := start(t, root)
	finished(t, done, true)
	row := read(t, root).Tasks["a"]
	if row.Result["answer"] != "ok" || row.Cost != .01 {
		t.Fatal("adapter result missing")
	}
	input, err := os.ReadFile(filepath.Join(root, "cells", "a", "received.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(input) || !strings.Contains(string(input), "dependency_artifacts") {
		t.Fatal("missing structured handoff")
	}
}
