package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryEnvironmentPrecedence(t *testing.T) {
	inherited := filepath.Join(t.TempDir(), "native", "session.json")
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", inherited)
	got, err := ResolveRegistryPath("")
	if err != nil || got != inherited {
		t.Fatalf("inherited: got %q, %v", got, err)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.json")
	got, err = ResolveRegistryPath(explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit override: got %q, %v", got, err)
	}
}

func TestRegistryEnvironmentInvalidFailsClosed(t *testing.T) {
	for _, invalid := range []string{"relative/session.json", "../session.json", " ", "~/session.json"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("IRRIGATOR_SESSION_REGISTRY", invalid)
			if got, err := ResolveRegistryPath(""); err == nil || got != "" {
				t.Fatalf("invalid environment silently resolved to %q: %v", got, err)
			}
			explicit := filepath.Join(t.TempDir(), "override.json")
			if got, err := ResolveRegistryPath(explicit); err != nil || got != explicit {
				t.Fatalf("explicit override blocked by environment: %q %v", got, err)
			}
		})
	}
}

func TestRegistryEnvironmentEmptyUsesLegacyCWD(t *testing.T) {
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, ".irrigator", "current-session.json")
	if got, err := ResolveRegistryPath(""); err != nil || got != want {
		t.Fatalf("legacy cwd: %q %v, want %q", got, err, want)
	}
	// An explicit relative flag retains its established cwd-relative semantics.
	want = filepath.Join(cwd, "chosen.json")
	if got, err := ResolveRegistryPath("chosen.json"); err != nil || got != want {
		t.Fatalf("explicit relative flag: %q %v", got, err)
	}
}
