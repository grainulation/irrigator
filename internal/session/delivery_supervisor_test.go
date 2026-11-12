package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestDeliverySupervisorRetiredLiveRunConsumesCapacity(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "registry.json")
	old, id := runfixture.V2Run(t, dir, "old")
	if err := PublishSession(reg, old, id); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireSession(reg, id, "superseded", ""); err != nil {
		t.Fatal(err)
	}
	lock, err := checkpoint.FileLock(filepath.Join(old, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lock)
	for i := 0; i < registryMaxActive-1; i++ {
		r, rid := runfixture.V2Run(t, dir, fmt.Sprintf("other%d", i))
		if err := PublishSession(reg, r, rid); err != nil {
			t.Fatal(err)
		}
	}
	r, rid := runfixture.V2Run(t, dir, "overflow")
	if err := PublishSession(reg, r, rid); !errors.Is(err, errRegistryActiveFull) {
		t.Fatalf("live retired run excluded from capacity: %v", err)
	}
}

func TestDeliverySupervisorPendingDeliveryIsNotLocalOnly(t *testing.T) {
	dir := t.TempDir()
	root, id := runfixture.V2Run(t, dir, "pending")
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonio.AtomicJSON(filepath.Join(root, "delivery.json"), graph.DeliveryRecord{RunID: id, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if got := graph.ProjectDelivery(root, s); got.Recovery == "resume_local" {
		t.Fatalf("pending provider work mislabeled local-only: %+v", got)
	}
}

func TestDeliverySupervisorRetirementPreservesCheckpointAndReactivates(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "registry.json")
	root, id := runfixture.V2Run(t, dir, "paused")
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := PublishSession(reg, root, id); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RetireSession(reg, id, "superseded", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("retirement mutated checkpoint")
	}
	if err := PublishSession(reg, root, id); err != nil {
		t.Fatal(err)
	}
	rr, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Entries[IndexOfRun(rr.Entries, id)].Retirement != nil {
		t.Fatal("explicit publish remains hidden")
	}
}
