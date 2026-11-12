package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRevisionRejectsRetiredCaseAlias(t *testing.T) {
	root := accGraph(t)
	if err := checkpoint.Update(root, func(s *model.State) error {
		p := DefaultAdaptivePolicy()
		p.CooldownMS = 0
		s.Policy = &p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := accSubmit(t, root, accGood("add", 0, append(accActive(t, root), runfixture.Task("helper", .01)))); err != nil {
		t.Fatal(err)
	}
	var without []model.Task
	for _, tk := range accActive(t, root) {
		if tk.ID != "helper" {
			without = append(without, tk)
		}
	}
	if err := accSubmit(t, root, accGood("drop", 1, without)); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	if before.GraphRevision != 2 {
		t.Fatal("setup did not retire")
	}
	old := filepath.Join(root, "cells", "helper")
	if err := os.WriteFile(filepath.Join(old, "review-sentinel"), []byte("archived evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := accSubmit(t, root, accGood("alias", 2, append(accActive(t, root), runfixture.Task("HELPER", .01)))); err != nil {
		t.Fatal(err)
	}
	after := runfixture.Read(t, root)
	if after.Tasks["HELPER"] != nil || after.GraphRevision != 2 {
		t.Fatalf("retired alias admitted revision=%d", after.GraphRevision)
	}
	data, err := os.ReadFile(filepath.Join(old, "review-sentinel"))
	if err != nil || string(data) != "archived evidence" {
		t.Fatal("retired evidence changed", err)
	}
	if len(after.ArchivedTasks) != len(before.ArchivedTasks) {
		t.Fatal("rejected proposal changed archive")
	}
	if len(after.Decisions) == 0 || !strings.Contains(after.Decisions[len(after.Decisions)-1].Reason, "case-insensitive") {
		t.Fatal("wrong rejection reason")
	}

}
