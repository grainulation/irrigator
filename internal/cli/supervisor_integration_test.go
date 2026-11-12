//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func TestSupervisorDeathMakesReplaySafeNodeUncertain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho $$ > provider.pid\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	node := task("a", .1)
	node.Provider = "haiku"
	root := fixture(t, node)
	_, done := start(t, root)
	cell := filepath.Join(root, "cells", "a")
	deadline := time.Now().Add(10 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(cell, "provider.pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if pid > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("provider did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	raw, err := os.ReadFile(filepath.Join(cell, "1.process.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker model.ProcessMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(marker.SupervisorPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	finished(t, done, false)
	if row := read(t, root).Tasks["a"]; row.Status != "uncertain" || row.Attempt != 1 {
		t.Fatalf("unsafe recovery state: %+v", row)
	}
	_, done = start(t, root)
	finished(t, done, false)
	if row := read(t, root).Tasks["a"]; row.Status != "uncertain" || row.Attempt != 1 {
		t.Fatalf("resume replayed unresolved process: %+v", row)
	}
}
