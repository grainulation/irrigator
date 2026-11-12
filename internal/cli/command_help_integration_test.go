//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/sizing"
)

// Public dispatch and top-level help are separate contracts. Keep them in sync
// so packaged consumers can discover every registered command before execution.
func TestPublicCommandsListedInHelp(t *testing.T) {
	for name, command := range commands() {
		if command.Hidden || name == "help" {
			continue
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(Usage) {
			t.Errorf("public command %q is missing from --help", name)
		}
	}
}

func TestCommandMetadataUsesActualFlagsAndHasNoEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	oldSizing := sizing.ActiveSizingPath
	sizing.ActiveSizingPath = "keep-existing-sizing"
	t.Cleanup(func() { sizing.ActiveSizingPath = oldSizing })
	definitions, err := commandDefinitions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if sizing.ActiveSizingPath != "keep-existing-sizing" {
		t.Fatal("metadata mutated active execution configuration")
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("help wrote home files: %v %v", entries, err)
	}
	flags := map[string]map[string]commandflags.CommandFlag{}
	for _, command := range definitions {
		if strings.HasPrefix(command.Name, "_") {
			t.Fatal("internal dispatch leaked")
		}
		flags[command.Name] = map[string]commandflags.CommandFlag{}
		for _, f := range command.Flags {
			flags[command.Name][f.Name] = f
		}
	}
	for _, test := range [][2]string{{"profiles", "catalog"}, {"profiles", "run"}, {"profiles", "executable"}, {"profiles", "out"}, {"profiles", "proposal"}, {"profiles", "approve"}, {"profiles", "confirm-stopped"}, {"serve", "claude"}, {"serve", "model"}, {"apply", "call-budget"}, {"billing", "attest-external-billing"}, {"compose", "out"}, {"turn-shape", "json"}} {
		if _, ok := flags[test[0]][test[1]]; !ok {
			t.Fatalf("actual flag absent: %v", test)
		}
	}
	if len(flags["panel"]) != 0 {
		t.Fatal("removed panel command still advertises server flags")
	}
	if flags["apply"]["backend"].Default != "" {
		t.Fatal("implicit paid backend metadata")
	}
	if _, err := commandDefinitions([]string{"_supervise"}); err == nil {
		t.Fatal("help dispatched internal supervisor")
	}
}
func TestCommandMetadataSnapshotMatchesRuntime(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "verification", "cli-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Schema   string                           `json:"schema"`
		Commands []commandflags.CommandDefinition `json:"commands"`
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	actual, err := commandDefinitions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Schema != "irrigator.cli/v1" || !reflect.DeepEqual(snapshot.Commands, actual) {
		t.Fatal("generated CLI contract is stale; rebuild CLI and run irrigator help --json > verification/cli-contract.json")
	}
}
func TestCancelAlreadyVerifiedPreservesCheckpoint(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	before := runfixture.Read(t, root).Tasks["a"].OperationID
	code, err := runCommand([]string{"cancel", root, "a"})
	if code != 0 || err != nil {
		t.Fatalf("cancel verified: %d %v", code, err)
	}
	after := runfixture.Read(t, root).Tasks["a"]
	if after.Status != "verified" || after.OperationID != before || after.CancelRequested {
		t.Fatalf("accepted task changed: %+v", after)
	}
}
