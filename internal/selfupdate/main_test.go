package selfupdate

import (
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
)

func TestMain(m *testing.M) { clitest.ReexecMain(m, clitest.Hooks{}) }
