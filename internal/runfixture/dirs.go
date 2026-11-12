package runfixture

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func BackupTemp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func BackupFixture(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(New(t, Task("a", 0)))
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func CanonicalContextDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ContextCapsuleFixture(t *testing.T) (string, *model.LaunchCapsule) {
	t.Helper()
	dir := CanonicalContextDir(t)
	file := filepath.Join(dir, "launch.json")
	if code, err := launch.LaunchContextCLI([]string{"create", "--workspace", dir, "--out", file}); err != nil || code != 0 {
		t.Fatalf("create: %v %d", err, code)
	}
	c, err := launch.ReadLaunchCapsule(file)
	if err != nil {
		t.Fatal(err)
	}
	return file, c
}

func CompositionTempDir(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func CompositionRun(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(New(t, Task("a", 0)))
	if e != nil {
		t.Fatal(e)
	}
	return p
}

// MutateStateJSON rewrites state.json through a generic map, the same way the
// tests simulate older checkpoints. It never goes through Update, so it does not
// stamp the schema version or create backups.
func MutateStateJSON(t *testing.T, root string, mutate func(raw map[string]any)) {
	t.Helper()
	statePath := filepath.Join(root, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	mutate(raw)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, out, 0600); err != nil {
		t.Fatal(err)
	}
}

func CaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}
