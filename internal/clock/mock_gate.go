package clock

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// MockGateEnv names a directory that lets integration tests hold mock work
// open. While <dir>/<task id>.hold exists, a mock call for that task keeps
// running; removing the file lets it finish. Tests use it instead of sub-second
// mock delays so "still running" windows do not depend on host speed. It only
// affects the mock backends and is unset in normal use.
const MockGateEnv = "IRRIGATOR_TEST_MOCK_GATE"

// WaitMockGate blocks while the task's hold file exists, or until ctx ends.
func WaitMockGate(ctx context.Context, taskID string) error {
	dir := os.Getenv(MockGateEnv)
	if dir == "" || taskID == "" {
		return nil
	}
	hold := filepath.Join(dir, filepath.Base(taskID)+".hold")
	for {
		if _, err := os.Stat(hold); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
