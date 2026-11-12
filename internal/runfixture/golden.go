package runfixture

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// GoldenDir locates verification/golden by walking up from the test's working
// directory, so it works from internal/<pkg> and from the root-level e2e package.
func GoldenDir() string {
	dir, err := os.Getwd()
	for err == nil {
		candidate := filepath.Join(dir, "verification", "golden")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join("..", "..", "verification", "golden")
}

// GoldenWithoutFixture lists commands without a golden fixture: doctor ignores -h and probes the host's installed tools. Its machine-specific
// report is not a portable golden fixture; dedicated readiness tests cover it.
var GoldenWithoutFixture = map[string]string{
	"doctor": "host installation/readiness report; not deterministic command help",
}

func GoldenFixtureNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(GoldenDir(), "subcommands"))
	if err != nil {
		t.Fatalf("reading subcommand fixtures: %v", err)
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".golden")
		if e.IsDir() || !ok {
			t.Errorf("unexpected entry %q in verification/golden/subcommands", e.Name())
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
