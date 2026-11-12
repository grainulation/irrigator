//go:build integration

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/session"
)

// retTwo registers an "old" run then a "new" run (new is current).
func retTwo(t *testing.T) (dir, reg, rA, idA, rB, idB string) {
	t.Helper()
	dir = t.TempDir()
	reg = filepath.Join(dir, ".irrigator", "current-session.json")
	rA, idA = runfixture.V2Run(t, dir, "old")
	rB, idB = runfixture.V2Run(t, dir, "new")
	for _, x := range [][2]string{{rA, idA}, {rB, idB}} {
		if err := session.PublishSession(reg, x[0], x[1]); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestRetireCLIUsageAndSuccess(t *testing.T) {
	_, reg, _, idA, _, idB := retTwo(t)
	if !strings.Contains(Usage, "retire RUN_ID") {
		t.Fatal("usage must document retire")
	}
	if code, err := runCommand([]string{"retire", idA, "--session-registry", reg}); code != 2 || err == nil {
		t.Fatalf("missing reason must be a usage error, got %d %v", code, err)
	}
	if code, err := runCommand([]string{"retire", "--reason", "x", "--session-registry", reg}); code != 2 || err == nil {
		t.Fatalf("missing run id must be a usage error, got %d %v", code, err)
	}
	if code, err := runCommand([]string{"retire", idA, "--reason", "replaced", "--superseded-by", idB, "--session-registry", reg}); code != 0 || err != nil {
		t.Fatalf("retire failed: %d %v", code, err)
	}
	got, err := session.ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	if e := got.Entries[session.IndexOfRun(got.Entries, idA)]; e.Retirement == nil || e.Retirement.SupersededBy != idB || e.Retirement.Reason != "replaced" {
		t.Fatalf("cli retirement not recorded: %+v", e)
	}
}
