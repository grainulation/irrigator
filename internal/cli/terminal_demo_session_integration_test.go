//go:build integration

package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/session"
)

func TestTerminalDemoPublishesAppSession(t *testing.T) {
	file, c := runfixture.ContextCapsuleFixture(t)
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", file)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	t.Setenv("IRRIGATOR_UI", "plain")
	savedPlan := terminalDemoPlan
	terminalDemoPlan, _ = json.Marshal(model.Plan{APIVersion: "swarm/v1", Name: "app session demo", Tasks: []model.Task{task("demo", 0)}})
	defer func() { terminalDemoPlan = savedPlan }()
	savedRegistry := session.ActiveSessionRegistry
	session.ActiveSessionRegistry = "prior-registry"
	defer func() { session.ActiveSessionRegistry = savedRegistry }()
	root := filepath.Join(c.Workspace, "runs", "demo")
	code, err := terminalDemo([]string{"--run", root})
	if err != nil || code != 0 {
		t.Fatalf("demo execution: %d %v", code, err)
	}
	if session.ActiveSessionRegistry != "prior-registry" {
		t.Fatal("demo leaked active registry")
	}
	state := read(t, root)
	registry, err := session.ReadRegistryAny(c.Registry)
	if err != nil || registry.CurrentRunID != state.RunID || len(registry.Entries) != 1 || registry.Entries[0].RunPath != root {
		t.Fatalf("demo invisible to app registry: %+v %v", registry, err)
	}
	if state.Control != "complete" || state.Tasks["demo"].Status != "verified" {
		t.Fatal("mock demo did not complete")
	}
}
