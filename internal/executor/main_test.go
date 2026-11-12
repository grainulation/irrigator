package executor

import (
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
)

func TestMain(m *testing.M) {
	clitest.ReexecMain(m, clitest.Hooks{Fixtures: map[string]func(){"_protocol_fixture": protocolFixture}})
}
