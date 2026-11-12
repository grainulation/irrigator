//go:build integration

package runops

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	campaignpkg "github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestControllerDrainProcess(t *testing.T) {
	if os.Getenv("IRRIGATOR_DRAIN_FIXTURE") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	lease, err := checkpoint.FileLock(os.Getenv("IRRIGATOR_DRAIN_LEASE"), false)
	if err != nil {
		os.Exit(2)
	}
	defer checkpoint.Unlock(lease)
	if os.WriteFile(os.Getenv("IRRIGATOR_DRAIN_READY"), []byte("ready"), 0600) != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func drainFixtureChild(t *testing.T, root, id string) *controllerChild {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestControllerDrainProcess$")
	cmd.Env = append(os.Environ(), "IRRIGATOR_DRAIN_FIXTURE=1", "IRRIGATOR_DRAIN_READY="+ready, "IRRIGATOR_DRAIN_LEASE="+filepath.Join(root, "cells", id, "lease"))
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &controllerChild{process: cmd.Process, done: make(chan struct{})}
	go func() { child.err = cmd.Wait(); close(child.done) }()
	t.Cleanup(func() {
		_ = child.process.Kill()
		select {
		case <-child.done:
		case <-time.After(3 * time.Second):
			t.Error("fixture did not drain")
		}
	})
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(ready); err == nil {
			return child
		}
		if time.Now().After(until) {
			t.Fatal("drain fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControllerDrainUsesOnlyOwnedHandles(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("owned", 0), runfixture.Task("unrelated", 0))
	owned := drainFixtureChild(t, root, "owned")
	unrelated := drainFixtureChild(t, root, "unrelated")
	if err := drainControllerChildren(root, map[string]*controllerChild{"owned": owned}, 50*time.Millisecond, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	runfixture.RequireCellLeaseReleased(t, root, "owned")
	if runfixture.Read(t, root).Control != "down" {
		t.Fatal("fatal cleanup did not request down")
	}
	select {
	case <-unrelated.done:
		t.Fatal("cleanup killed an unowned process")
	default:
	}
}

func TestControllerDrainDoesNotBlockOnStateLock(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("owned", 0))
	child := drainFixtureChild(t, root, "owned")
	lock, err := checkpoint.FileLock(filepath.Join(root, "state.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lock)
	started := time.Now()
	err = drainControllerChildren(root, map[string]*controllerChild{"owned": child}, 50*time.Millisecond, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "cannot persist controller shutdown") {
		t.Fatalf("state lock failure not reported: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("drain blocked on held checkpoint lock")
	}
	runfixture.RequireCellLeaseReleased(t, root, "owned")
}

func TestControllerDrainWaitsForSupervisorLease(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("owned", 0))
	supervisor := drainFixtureChild(t, root, "owned")
	worker := exec.Command("/usr/bin/true")
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()
	done := make(chan struct{})
	close(done)
	child := &controllerChild{process: worker.Process, done: done}
	go func() { time.Sleep(150 * time.Millisecond); _ = supervisor.process.Kill() }()
	started := time.Now()
	if err := drainControllerChildren(root, map[string]*controllerChild{"owned": child}, 50*time.Millisecond, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("returned before supervisor released cell lease")
	}
	runfixture.RequireCellLeaseReleased(t, root, "owned")
}

func TestControllerDrainDoesNotTreatLostSupervisorAsCleanup(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("owned", 0))
	marker := filepath.Join(root, "cells", "owned", "1.process.json")
	if err := jsonio.AtomicJSON(marker, model.ProcessMarker{Status: "active", SupervisorPID: 12345}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	child := &controllerChild{process: cmd.Process, done: done, attempt: 1}
	err := drainControllerChildren(root, map[string]*controllerChild{"owned": child}, 50*time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "provider cleanup unconfirmed") {
		t.Fatalf("missing provider uncertainty: %v", err)
	}
	unresolved, err := launch.UnresolvedProcess(filepath.Join(root, "cells", "owned"), 1)
	if err != nil || !unresolved {
		t.Fatal("controller altered durable cleanup evidence")
	}
}

func TestControllerDrainDoesNotBlockOnCampaignLock(t *testing.T) {
	directory := t.TempDir()
	campaign := filepath.Join(directory, "campaign")
	root := filepath.Join(directory, "run")
	if err := campaignpkg.CampaignInit(campaign, "0.25"); err != nil {
		t.Fatal(err)
	}
	if _, err := campaignpkg.CampaignAdmit(campaign, runfixture.CampaignTestPlan(t, "0.25"), root); err != nil {
		t.Fatal(err)
	}
	child := drainFixtureChild(t, root, "a")
	store, err := checkpoint.OpenCampaign(campaign)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Now()
	err = drainControllerChildren(root, map[string]*controllerChild{"a": child}, 50*time.Millisecond, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "cannot persist controller shutdown") {
		t.Fatalf("authority contention missing: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cleanup blocked on campaign lock")
	}
	runfixture.RequireCellLeaseReleased(t, root, "a")
}
