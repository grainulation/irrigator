package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

const (
	gcKillHelperLimit = 60 * time.Second
)

// TestGCKillHelperProcess is a subprocess body, inert unless the role marker
// is set by the parent test.
func TestGCKillHelperProcess(t *testing.T) {
	role := os.Getenv(gcKillRoleEnv)
	if role == "" {
		t.Skip("subprocess helper")
	}
	markers := os.Getenv(gcKillMarkersEnv)
	deadline := time.Now().Add(gcKillHelperLimit)
	switch role {
	case "controller":
		lock, err := FileLock(filepath.Join(os.Getenv(gcKillRunEnv), "controller.lock"), true)
		if err != nil {
			_ = gcKillPublish(filepath.Join(markers, "controller.err"), err.Error())
			os.Exit(3)
		}
		// Held until the kernel releases it at process death (KeepAlive below
		// prevents a finalizer from closing it early). It is close-on-exec, so
		// the child never inherits it.
		child := exec.Command(os.Args[0], gcKillHelperRun)
		child.Dir = os.Getenv(gcKillWorktreeEnv)
		child.Env = append(os.Environ(), gcKillRoleEnv+"=child")
		if err := child.Start(); err != nil {
			_ = gcKillPublish(filepath.Join(markers, "controller.err"), err.Error())
			os.Exit(4)
		}
		if _, ok := gcKillWaitFile(filepath.Join(markers, "child.ready"), 15*time.Second); !ok {
			_ = child.Process.Kill()
			_ = gcKillPublish(filepath.Join(markers, "controller.err"), "child not ready")
			os.Exit(5)
		}
		if err := gcKillPublish(filepath.Join(markers, "controller.ready"), strconv.Itoa(child.Process.Pid)); err != nil {
			os.Exit(6)
		}
		for time.Now().Before(deadline) { // bounded: the parent kills us long before
			time.Sleep(50 * time.Millisecond)
		}
		runtime.KeepAlive(lock)
		os.Exit(0)
	case "child":
		wd, err := os.Getwd()
		if err != nil {
			os.Exit(7)
		}
		if err := os.WriteFile(filepath.Join(wd, gcKillSentinel), []byte("owned by a continuing worker descendant\n"), 0o600); err != nil {
			os.Exit(8)
		}
		if err := gcKillPublish(filepath.Join(markers, "child.ready"), strconv.Itoa(os.Getpid())); err != nil {
			os.Exit(9)
		}
		for beat := 1; time.Now().Before(deadline); beat++ {
			if _, err := os.Stat(filepath.Join(markers, "stop")); err == nil {
				os.Exit(0)
			}
			if err := gcKillPublish(filepath.Join(markers, "heartbeat"), strconv.Itoa(beat)); err != nil {
				os.Exit(0) // markers removed: the parent test is gone
			}
			time.Sleep(20 * time.Millisecond)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}
