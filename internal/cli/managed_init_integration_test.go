//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedInitCLIRequiresExplicitProfile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "plan.json")
	for _, args := range [][]string{{"init", "--managed", "--out", out}, {"init", "--profile", "claude:sonnet", "--out", out}} {
		_, err := runCommand(args)
		if err == nil || !strings.Contains(err.Error(), "--profile") {
			t.Fatalf("missing explicit selector gate: %v", err)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("invalid request wrote plan")
		}
	}
}
