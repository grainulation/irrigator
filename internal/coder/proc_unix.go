//go:build unix

package coder

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the agent in its own process group so a timeout stops the
// agent and everything it started.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
