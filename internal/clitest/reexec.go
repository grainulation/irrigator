package clitest

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
)

// Hooks says what a re-executed test binary may serve.
type Hooks struct {
	// CLI, when set (internal/cli tests pass cli.Main), serves every CLI
	// command, including resume and _worker, exactly like the root main package.
	CLI func(args []string) int
	// Fixtures are extra fake subcommands (e.g. "_protocol_fixture").
	Fixtures map[string]func()
}

// ReexecMain is called from TestMain. If the binary was re-executed with a
// command as its first argument it serves it and exits; otherwise it runs the
// tests. Unknown commands exit 97 instead of re-running the suite recursively.
func ReexecMain(m *testing.M, h Hooks) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if f, ok := h.Fixtures[os.Args[1]]; ok {
			f()
			return
		}
		if h.CLI != nil {
			os.Exit(h.CLI(os.Args))
		}
		if os.Args[1] == "_supervise" {
			os.Exit(launch.Supervise())
		}
		fmt.Fprintf(os.Stderr, "clitest: test binary cannot serve %q (no CLI hook in this package)\n", os.Args[1])
		os.Exit(97)
	}
	os.Exit(m.Run())
}
