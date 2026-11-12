package artifactstore

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
)

// TestMain serves the re-executed test binary. This package is imported by
// internal/clitest, so it cannot use clitest.ReexecMain without an import cycle.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if os.Args[1] == "_supervise" {
			os.Exit(launch.Supervise())
		}
		fmt.Fprintf(os.Stderr, "artifactstore: test binary cannot serve %q\n", os.Args[1])
		os.Exit(97)
	}
	os.Exit(m.Run())
}
