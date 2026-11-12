package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestInvalidPlans(t *testing.T) {
	for _, tasks := range [][]model.Task{{runfixture.Task("a", 0, "a")}, {runfixture.Task("a", 0, "missing")}, {runfixture.Task("a", 0), runfixture.Task("a", 0)}, {runfixture.Task("../escape", 0)}, nil} {
		if err := checkpoint.Validate(model.Plan{Tasks: tasks}); err == nil {
			t.Fatal("accepted invalid plan")
		}
	}
}

func TestCheckpointConcurrentUpdates(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .1))
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			done <- checkpoint.Update(root, func(s *model.State) error { s.Event("parallel", "", nil); return nil })
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for _, e := range runfixture.Read(t, root).Events {
		if e.Kind == "parallel" {
			count++
		}
	}
	if count != 20 {
		t.Fatalf("lost updates: %d", count)
	}
}

func TestHaikuCancellationStopsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho $$ > model.pid\nsleep 30 &\necho $! > child.pid\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	cell := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := callHaiku(ctx, cell, 1, runfixture.Task("a", .1), nil, .25)
		done <- err
	}()
	// Wait for the process handshake, not a machine-speed-dependent startup delay.
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(cell, "child.pid"))
		if err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake model did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	for _, file := range []string{"model.pid", "child.pid"} {
		data, err := os.ReadFile(filepath.Join(cell, file))
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		if _, err = fmt.Sscan(string(data), &pid); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			t.Fatalf("process %d survived cancellation", pid)
		}
	}
}
