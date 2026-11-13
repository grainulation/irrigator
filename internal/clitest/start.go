package clitest

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
)

// Start runs `resume ROOT OPTS...` by re-executing the test binary (requires
// a TestMain that calls ReexecMain with a CLI hook) and stops it at cleanup.
func Start(t *testing.T, root string, opts ...string) (*exec.Cmd, <-chan error) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping process-spawning integration test in short mode")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, append([]string{"resume", root}, opts...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = checkpoint.SetControl(root, "down") })
	return cmd, done
}

// Finished waits for the controller started by Start.
func Finished(t *testing.T, done <-chan error, wantSuccess bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err == nil) != wantSuccess {
			t.Fatalf("exit %v, expected success %v", err, wantSuccess)
		}
	case <-time.After(60 * time.Second): // a hang guard, not a timing assumption: slow hosts under -race need room
		t.Fatal("controller did not exit")
	}
}
