package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestProviderMigrationRetainsPastAndRunsFuture(t *testing.T) {
	root, old, replacement := runfixture.MigrationFixture(t)
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	if before.Tasks["a"].Status != "verified" {
		t.Fatalf("first task: %+v", before.Tasks["a"])
	}
	receiptPath := filepath.Join(root, "cells", "a", "1.managed.json")
	originalReceipt, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(old); err != nil {
		t.Fatal(err)
	}
	proposalPath := filepath.Join(t.TempDir(), "proposal.json")
	if code, err := executionProfilesCLI([]string{"migrate-prepare", "--run", root, "--profile", "small", "--executable", replacement, "--out", proposalPath}); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if !reflect.DeepEqual(before, runfixture.Read(t, root)) {
		t.Fatal("prepare changed run")
	}
	raw, _ := os.ReadFile(proposalPath)
	var p model.ProviderMigrationProposal
	if err := provider.DecodeMigrationProposal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if code, err := executionProfilesCLI([]string{"migrate-apply", "--run", root, "--proposal", proposalPath, "--approve", p.Digest, "--confirm-stopped"}); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	after := runfixture.Read(t, root)
	if !reflect.DeepEqual(before.Plan, after.Plan) || before.PlanHash != after.PlanHash || !reflect.DeepEqual(before.Execution.Bindings, after.Execution.Bindings) || !reflect.DeepEqual(before.Tasks["a"], after.Tasks["a"]) || !reflect.DeepEqual(before.Execution.Reservations, after.Execution.Reservations) {
		t.Fatal("migration rewrote historical evidence")
	}
	if after.Tasks["b"].BindingDigest == before.Tasks["b"].BindingDigest || after.Control != "pause" {
		t.Fatal("future binding not migrated safely")
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "b", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	final := runfixture.Read(t, root)
	if final.Tasks["b"].Status != "verified" || final.Tasks["a"].Attempt != 1 || final.Tasks["b"].Attempt != 1 {
		t.Fatalf("migration execution failed %+v", final.Tasks)
	}
	tampered, err := graph.CloneState(final)
	if err != nil {
		t.Fatal(err)
	}
	tampered.Tasks["b"].BindingDigest = before.Tasks["b"].BindingDigest
	if err := checkpoint.ValidateManagedState(tampered); err == nil {
		t.Fatal("current receipt allowed a different historical binding")
	}
	retained, _ := os.ReadFile(receiptPath)
	if string(retained) != string(originalReceipt) {
		t.Fatal("past receipt changed")
	}
	next := final.NewTask(model.Task{ID: "future", Profile: "small"})
	if next.BindingDigest != p.Replacement.Digest {
		t.Fatal("future generated task lost binding")
	}
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "backup.json")
	if err := backup.BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.BackupRead(bundle); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := backup.BackupRestore(bundle, restored); err != nil {
		t.Fatal(err)
	}
	if s, err := checkpoint.ReadState(filepath.Join(restored, "evidence")); err != nil || len(s.Execution.Migrations) != 1 {
		t.Fatal("migration backup provenance", err)
	}
}
