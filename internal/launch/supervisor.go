package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

type ProcessLeaseKey struct{}

func ProcessMarkerPaths(cell string, attempt int) ([]string, error) {
	prefix := filepath.Join(cell, strconv.Itoa(attempt))
	paths, err := filepath.Glob(prefix + ".check-*.process.json")
	return append(paths, prefix+".process.json"), err
}

// acknowledgeProcessCleanup is only called after an explicit user attestation
// that surviving processes have stopped, while the caller holds the node lease.
// It neither inspects nor kills a saved PID, and preserves the original bytes
// before changing recovery evidence. Oversized/unreadable evidence fails closed.
func AcknowledgeProcessCleanup(cell string, attempt int) error {
	paths, err := ProcessMarkerPaths(cell, attempt)
	if err != nil {
		return err
	}
	for _, path := range paths {
		raw, err := model.ReadProcessEvidence(path, 65536)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var prior model.ProcessMarker
		parseErr := json.Unmarshal(raw, &prior)
		if parseErr == nil && prior.Status == "stopped" {
			continue
		}
		previous := prior.Status
		if parseErr != nil || previous == "" {
			previous = "invalid"
		}
		auditPath := path + ".unresolved.json"
		var audit struct {
			Original    []byte `json:"original_bytes"`
			ConfirmedBy string `json:"confirmed_by"`
		}
		if saved, readErr := model.ReadProcessEvidence(auditPath, 128*1024); readErr == nil {
			if json.Unmarshal(saved, &audit) != nil || !bytes.Equal(audit.Original, raw) {
				return fmt.Errorf("existing process audit differs from unresolved marker: %s", auditPath)
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		} else {
			audit.Original, audit.ConfirmedBy = raw, "explicit-user"
			if err := jsonio.AtomicJSON(auditPath, audit); err != nil {
				return err
			}
		}
		if err := jsonio.AtomicJSON(path, model.ProcessMarker{Status: "stopped", SupervisorPID: prior.SupervisorPID, ConfirmedBy: "explicit-user", PreviousStatus: previous}); err != nil {
			return err
		}
	}
	return nil
}

// Missing markers are compatible with mock and pre-marker runs. Any present
// active or unreadable marker fails closed: a supervisor may have died while
// its provider survived. PIDs are diagnostic only, never recovery kill targets.
func UnresolvedProcess(cell string, attempt int) (bool, error) {
	paths, err := ProcessMarkerPaths(cell, attempt)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		raw, err := model.ReadProcessEvidence(path, 65536)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		var marker model.ProcessMarker
		if err := json.Unmarshal(raw, &marker); err != nil {
			return false, fmt.Errorf("invalid process marker %s: %w", path, err)
		}
		switch marker.Status {
		case "active":
			return true, nil
		case "stopped":
		default:
			return false, fmt.Errorf("unknown process marker status in %s", path)
		}
	}
	return false, nil
}

// The supervisor owns the provider lifetime and a duplicate of the node lease.
// EOF on the private liveness pipe means the worker has gone away. No durable
// PID is used for cleanup, so recovery cannot kill a recycled process ID.
type processSpec struct {
	Path     string    `json:"path"`
	Args     []string  `json:"args"`
	Env      []string  `json:"env"`
	Dir      string    `json:"dir"`
	Prefix   string    `json:"prefix"`
	Input    []byte    `json:"input"`
	Deadline time.Time `json:"deadline"`
	Lease    bool      `json:"lease"`
}

func RunProcess(ctx context.Context, cmd *exec.Cmd, prefix string, input []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) > model.MaxInputBytes {
		return nil, errors.New("input exceeds 1 MiB limit; narrow task context")
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	defer writer.Close()
	spec := processSpec{Path: cmd.Path, Args: cmd.Args, Env: cmd.Environ(), Dir: cmd.Dir, Prefix: prefix, Input: input}
	spec.Deadline, _ = ctx.Deadline()
	supervisor := exec.Command(binary, "_supervise")
	// Keep cleanup alive even when the worker's entire process group is killed.
	supervisor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	supervisor.ExtraFiles = []*os.File{reader}
	if lease, ok := ctx.Value(ProcessLeaseKey{}).(*os.File); ok {
		spec.Lease = true
		supervisor.ExtraFiles = append(supervisor.ExtraFiles, lease)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	// Credentials travel through this pipe only, never through a checkpoint or
	// command-line argument. Provider stdout remains in its bounded artifact.
	supervisor.Stdin = bytes.NewReader(encoded)
	var response bytes.Buffer
	supervisor.Stdout = &response
	if err := supervisor.Start(); err != nil {
		return nil, err
	}
	_ = reader.Close()
	done := make(chan error, 1)
	go func() { done <- supervisor.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = writer.Close()
		<-done //nolint:errcheck // cancelled: wait for the exit only; ctx.Err() is the result
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("provider supervisor failed: %w", err)
	}
	var status struct {
		Error      string `json:"error"`
		NotStarted bool   `json:"not_started,omitempty"`
	}
	if err := json.Unmarshal(response.Bytes(), &status); err != nil {
		return nil, fmt.Errorf("invalid supervisor response: %w", err)
	}
	raw, readErr := os.ReadFile(prefix + ".stdout.json")
	if status.Error != "" {
		err := errors.New(status.Error)
		if status.NotStarted {
			return raw, model.MarkNotStarted(err)
		}
		return raw, err
	}
	if status.NotStarted {
		return raw, errors.New("supervisor not_started requires error")
	}
	return raw, readErr
}

// waitProcessGone observes a launched child or its process group until the
// kernel confirms disappearance. Only ESRCH establishes completion; bounded
// observation tolerates transient permission errors without authorizing replay.
// Callers probe live launch identities, never persisted recovery PIDs.
func WaitProcessGone(probe func() error, timeout time.Duration) error {
	return WaitProcessGoneWithClock(clock.ActiveClock, probe, timeout)
}

// waitProcessGoneWithClock computes the deadline and paces its probes with one
// clock. Every iteration probes first, so a zero or already-expired timeout
// still observes the process once, and the loop ends when the clock reaches
// the deadline.
func WaitProcessGoneWithClock(clock clock.Clock, probe func() error, timeout time.Duration) error {
	deadline := clock.Now().Add(timeout)
	for {
		err := probe()
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		// Host restrictions can transiently report EPERM while a killed
		// process is reaped. Permission errors never establish termination.
		if (err != nil && !errors.Is(err, syscall.EPERM)) || !clock.Now().Before(deadline) {
			return fmt.Errorf("%w: process remains visible (%v)", ErrProcessCleanup, err)
		}
		clock.Sleep(time.Millisecond)
	}
}

func Supervise() int {
	var spec processSpec
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 4*model.MaxInputBytes)).Decode(&spec); err != nil {
		return 1
	}
	if spec.Path == "" || len(spec.Args) == 0 {
		return 1
	}
	liveness := os.NewFile(3, "worker-liveness")
	defer liveness.Close()
	// Do not LOCK_UN: flock is shared with the worker's open file description.
	// Closing our copy releases it only after every remaining owner is gone.
	if spec.Lease {
		lease := os.NewFile(4, "node-lease")
		defer lease.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !spec.Deadline.IsZero() {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, spec.Deadline)
		defer deadlineCancel()
	}
	go func() { _, _ = io.Copy(io.Discard, liveness); cancel() }()
	markerPath := spec.Prefix + ".process.json"
	if raw, readErr := os.ReadFile(markerPath); readErr == nil {
		var prior model.ProcessMarker
		if json.Unmarshal(raw, &prior) != nil || prior.Status != "stopped" {
			return 1
		}
	} else if !os.IsNotExist(readErr) {
		return 1
	}
	// Persist intent before launching: even SIGKILL in the launch/commit gap
	// leaves an explicit ambiguity instead of authorizing overlapping replay.
	if err := jsonio.AtomicJSON(markerPath, model.ProcessMarker{Status: "active", SupervisorPID: os.Getpid()}); err != nil {
		return 1
	}
	cmd := &exec.Cmd{Path: spec.Path, Args: spec.Args, Env: spec.Env, Dir: spec.Dir}
	_, err := directProcess(ctx, cmd, spec.Prefix, spec.Input)
	if errors.Is(err, ErrProcessCleanup) {
		_ = jsonio.AtomicJSON(markerPath, model.ProcessMarker{Status: "active", SupervisorPID: os.Getpid(), CleanupError: err.Error()})
		return 1
	}
	// Keep the lease and active marker until both the group cleanup and the
	// leader's disappearance have been confirmed. Failure stays ambiguous.
	if cmd.Process != nil {
		if cleanupErr := WaitProcessGone(func() error { return syscall.Kill(cmd.Process.Pid, 0) }, 2*time.Second); cleanupErr != nil {
			_ = jsonio.AtomicJSON(markerPath, model.ProcessMarker{Status: "active", SupervisorPID: os.Getpid(), CleanupError: cleanupErr.Error()})
			return 1
		}
	}
	if err := jsonio.AtomicJSON(markerPath, model.ProcessMarker{Status: "stopped", SupervisorPID: os.Getpid()}); err != nil {
		return 1
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Error      string `json:"error"`
		NotStarted bool   `json:"not_started,omitempty"`
	}{message, model.KnownNotStarted(err) && cmd.Process == nil}); err != nil {
		return 1
	}
	return 0
}
