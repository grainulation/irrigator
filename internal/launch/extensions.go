package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

var ErrProcessCleanup = errors.New("provider process cleanup could not be confirmed")

func stopProcessGroup(pid int) error {
	return stopProcessGroupWith(syscall.Kill, pid, 2*time.Second)
}

// stopProcessGroupWith kills the process group and requires a positive "no such
// process" observation within timeout. EPERM from the kill can be reported
// transiently while a dying group is reaped; like in waitProcessGone it never
// establishes termination, so it falls through to the probe instead of ending
// the wait early. Any other kill error is a cleanup failure.
func stopProcessGroupWith(kill func(pid int, sig syscall.Signal) error, pid int, timeout time.Duration) error {
	err := kill(-pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("%w: %v", ErrProcessCleanup, err)
	}
	return WaitProcessGone(func() error { return kill(-pid, 0) }, timeout)
}

// Output is streamed to bounded files, not accumulated without limits in memory.
type cappedWriter struct {
	file      *os.File
	remaining int
	overflow  bool
	discard   bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.remaining {
		w.overflow = true
		if w.remaining > 0 {
			if _, err := w.file.Write(p[:w.remaining]); err != nil {
				return 0, err
			}
			w.remaining = 0
		}
		if w.discard {
			return n, nil
		}
		return 0, errors.New("provider output exceeded limit")
	}
	written, err := w.file.Write(p)
	w.remaining -= written
	return written, err
}
func directProcess(ctx context.Context, cmd *exec.Cmd, prefix string, input []byte) ([]byte, error) {
	raw, err := runDirectProcess(ctx, cmd, prefix, input)
	// cmd.Process is set only by a successful Start: its absence proves the command never started.
	if err != nil && cmd.Process == nil {
		err = model.MarkNotStarted(err)
	}
	return raw, err
}
func runDirectProcess(ctx context.Context, cmd *exec.Cmd, prefix string, input []byte) ([]byte, error) {
	if len(input) > model.MaxInputBytes {
		return nil, errors.New("input exceeds 1 MiB limit; narrow task context")
	}
	if err := os.WriteFile(prefix+".input.json", input, 0600); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(prefix+".stdout.json", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	stderr, err := os.OpenFile(prefix+".stderr.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	output := &cappedWriter{file: out, remaining: model.MaxOutputBytes}
	diagnostics := &cappedWriter{file: stderr, remaining: 1024 * 1024, discard: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), output, diagnostics
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		// A provider must not leave descendants behind after completing its call.
		if cleanupErr := stopProcessGroup(cmd.Process.Pid); cleanupErr != nil {
			return nil, cleanupErr
		}
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done: //nolint:errcheck // cancelled: wait for the exit only; ctx.Err() is the result
			if cleanupErr := stopProcessGroup(cmd.Process.Pid); cleanupErr != nil {
				return nil, cleanupErr
			}
		case <-time.After(2 * time.Second):
			if cleanupErr := stopProcessGroup(cmd.Process.Pid); cleanupErr != nil {
				// Give Wait time to reap a killed leader even if descendants could
				// not be confirmed gone. A failed kill must not block forever.
				select {
				case <-done: //nolint:errcheck // cancelled: wait for the exit only; ctx.Err() is the result
				case <-time.After(2 * time.Second):
				}
				return nil, cleanupErr
			}
			<-done //nolint:errcheck // cancelled: wait for the exit only; ctx.Err() is the result
		}
		return nil, ctx.Err()
	}
	if output.overflow {
		return nil, errors.New("provider output exceeded 4 MiB limit")
	}
	raw, readErr := os.ReadFile(prefix + ".stdout.json")
	if readErr != nil {
		return nil, readErr
	}
	return raw, err
}
