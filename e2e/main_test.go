package e2e

import (
	"testing"

	"github.com/grainulation/irrigator/internal/cli"
	"github.com/grainulation/irrigator/internal/clitest"
)

func TestMain(m *testing.M) { clitest.ReexecMain(m, clitest.Hooks{CLI: cli.Main}) }
