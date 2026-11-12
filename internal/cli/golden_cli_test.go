package cli

import (
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestGoldenSubcommandFixturesCoverRegistry(t *testing.T) {
	registered := map[string]bool{}
	for name := range commands() {
		if !strings.HasPrefix(name, "_") { // internal worker/supervisor entry points
			registered[name] = true
		}
	}
	fixtures := map[string]bool{}
	for _, name := range runfixture.GoldenFixtureNames(t) {
		fixtures[name] = true
		if !registered[name] {
			t.Errorf("fixture subcommands/%s.golden has no registered command; delete or regenerate it deliberately", name)
		}
	}
	for name := range registered {
		if !fixtures[name] && runfixture.GoldenWithoutFixture[name] == "" {
			t.Errorf("registered command %q has no subcommands/%s.golden fixture", name, name)
		}
	}
	for name := range runfixture.GoldenWithoutFixture {
		if !registered[name] || fixtures[name] {
			t.Errorf("host-dependent command %q must be registered without a machine-specific fixture", name)
		}
	}
}
