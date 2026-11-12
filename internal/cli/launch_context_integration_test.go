//go:build integration

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func contextProvider(t *testing.T) model.CommandSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return model.CommandSpec{Command: []string{exe, "-test.run=^TestContextProviderHelper$"}, Env: []string{"IRRIGATOR_CONTEXT_TEST_HELPER"}, Replay: "safe"}
}

func TestLaunchContextActualChildHeadlessAndChangedCWD(t *testing.T) {
	capsule, c := runfixture.ContextCapsuleFixture(t)
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", capsule)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	t.Setenv("IRRIGATOR_CONTEXT_TEST_HELPER", "capsule")
	root := filepath.Join(c.Workspace, "run")
	parent := model.Task{ID: "root", Prompt: "Delegate one child", Requires: []string{"answer", "children"}, Context: []string{"allowed"}, Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 1}}
	p := model.Plan{Name: "context headless", Context: map[string]string{"allowed": "chosen", "hidden": "never forwarded"}, Tasks: []model.Task{parent}, Providers: map[string]model.CommandSpec{"local-context": contextProvider(t)}, MaxTasks: 2, MaxDepth: 1}
	if err := checkpoint.Initialize(p, root, "local-context"); err != nil {
		t.Fatal(err)
	}
	// Worker/controller cwd differs from workspace; capsule routes by absolute
	// identity. No observer, browser, model or network service is involved.
	binary, _ := os.Executable()
	cmd := exec.Command(binary, "resume", root, "--workers", "2")
	cmd.Dir = runfixture.CanonicalContextDir(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = checkpoint.SetControl(root, "down") })
	clitest.Finished(t, done, true)
	state := runfixture.Read(t, root)
	if len(state.Tasks) != 2 {
		t.Fatalf("no actual child: %v", state.Tasks)
	}
	for id, row := range state.Tasks {
		if row.Status != "verified" || row.Attempt != 1 {
			t.Fatalf("%s: %s/%d %s", id, row.Status, row.Attempt, row.Error)
		}
		if row.Result["answer"] != fmt.Sprintf("%s|%s|chosen", id, row.Spec.Parent) {
			t.Fatal("provider did not independently consume identity/context")
		}
		if _, err := os.Stat(filepath.Join(root, "cells", id, "1.context.json")); err != nil {
			t.Fatal(err)
		}
	}
	before := state.RunID
	_, again := clitest.Start(t, root)
	clitest.Finished(t, again, true)
	after := runfixture.Read(t, root)
	if after.RunID != before {
		t.Fatal("resume changed identity")
	}
	for _, row := range after.Tasks {
		if row.Attempt != 1 {
			t.Fatal("headless resume replayed work")
		}
	}
}

func TestLaunchCapsuleAdmissionRejectsBeforeInitialize(t *testing.T) {
	capsule, c := runfixture.ContextCapsuleFixture(t)
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", capsule)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	plan := filepath.Join(c.Workspace, "plan.json")
	if err := jsonio.AtomicJSON(plan, model.Plan{Name: "admission", Tasks: []model.Task{runfixture.Task("a", 0)}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, root, registry string }{{"registry", filepath.Join(c.Workspace, "reject-registry"), filepath.Join(c.Workspace, "different.json")}, {"outside", filepath.Join(runfixture.CanonicalContextDir(t), "reject-outside"), c.Registry}} {
		code, err := runCommand([]string{"prepare", plan, "--run", tc.root, "--backend", "mock", "--session-registry", tc.registry})
		if err == nil || code == 0 {
			t.Fatalf("%s accepted", tc.name)
		}
		if _, err := os.Stat(filepath.Join(tc.root, "state.json")); !os.IsNotExist(err) {
			t.Fatalf("%s initialized before rejection: %v", tc.name, err)
		}
	}
}
