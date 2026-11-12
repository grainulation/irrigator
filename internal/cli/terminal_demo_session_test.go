package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTerminalDemoSessionRegistryRouting(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", "")
	dir := runfixture.CanonicalContextDir(t)
	target := filepath.Join(dir, "run")
	root, registry, err := terminalDemoDestination(target, "")
	if err != nil || root != target || registry != "" {
		t.Fatalf("headless default: %s %s %v", root, registry, err)
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("routing created a run")
	}
	inherited := filepath.Join(dir, "inherited.json")
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", inherited)
	_, registry, err = terminalDemoDestination(target, "")
	if err != nil || registry != inherited {
		t.Fatalf("inherited registry: %q %v", registry, err)
	}
	explicit := filepath.Join(dir, "explicit.json")
	_, registry, err = terminalDemoDestination(target, explicit)
	if err != nil || registry != explicit {
		t.Fatalf("explicit registry: %q %v", registry, err)
	}
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", "relative/registry.json")
	if _, _, err = terminalDemoDestination(target, ""); err == nil {
		t.Fatal("invalid inherited registry accepted")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("invalid registry created a run")
	}
}

func TestTerminalDemoLaunchConfinement(t *testing.T) {
	file, c := runfixture.ContextCapsuleFixture(t)
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", file)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	root, registry, err := terminalDemoDestination("", "")
	if err != nil || !launch.ContextPathInside(c.Workspace, root) || registry != c.Registry {
		t.Fatalf("default app destination: %s %s %v", root, registry, err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("validation created a run")
	}
	outside := filepath.Join(runfixture.CanonicalContextDir(t), "outside")
	if _, _, err = terminalDemoDestination(outside, ""); err == nil {
		t.Fatal("outside run allowed")
	}
	if _, err = os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("outside run was created")
	}
	if _, _, err = terminalDemoDestination(root, filepath.Join(c.Workspace, "wrong.json")); err == nil {
		t.Fatal("conflicting explicit registry allowed")
	}
	alias := filepath.Join(c.Workspace, "linked")
	if err = os.Symlink(runfixture.CanonicalContextDir(t), alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err = terminalDemoDestination(filepath.Join(alias, "run"), ""); err == nil {
		t.Fatal("linked run destination allowed")
	}
}
