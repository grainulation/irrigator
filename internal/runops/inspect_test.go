package runops

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func acceptedFixture(t *testing.T, outputs map[string]string) string {
	t.Helper()
	node := runfixture.Task("a", 0)
	node.Outputs = outputs
	root := runfixture.New(t, node)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status = "verified"
		s.Tasks["a"].Attempt = 3
		s.Tasks["a"].Result = map[string]string{"answer": "accepted content"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestExportPreservesExistingAndAcceptedArtifacts(t *testing.T) {
	root := acceptedFixture(t, map[string]string{"answer": "src/answer.txt"})
	dest := filepath.Join(t.TempDir(), "export")
	if err := ExportArtifacts(root, "", dest); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dest, "src/answer.txt"))
	if err != nil || string(content) != "accepted content" {
		t.Fatalf("export: %q %v", content, err)
	}
	if err := ExportArtifacts(root, "", dest); err == nil {
		t.Fatal("overwrote existing export directory")
	}
	out := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(out, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ExportArtifacts(root, out, ""); err == nil || !strings.Contains(err.Error(), "export file already exists") || !strings.Contains(err.Error(), "choose a new --out path") {
		t.Fatalf("missing actionable existing-export error: %v", err)
	}
	content, _ = os.ReadFile(out)
	if string(content) != "keep" {
		t.Fatal("existing file changed")
	}
}
func TestExportRejectsReservedAndOverlappingPaths(t *testing.T) {
	for _, paths := range []map[string]string{{"answer": "swarm-export.json"}, {"answer": "swarm-export.json/nested"}, {"answer": "src", "other": "src/file"}} {
		t.Run(strings.Join(mapValues(paths), "-"), func(t *testing.T) {
			root := acceptedFixture(t, nil)
			// Test the export boundary independently of plan validation.
			s := runfixture.Read(t, root)
			s.Plan.Tasks[0].Outputs = paths
			s.Tasks["a"].Spec.Outputs = paths
			encoded, _ := json.Marshal(s.Plan)
			s.PlanHash = fmt.Sprintf("%x", sha256.Sum256(encoded))
			if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
				t.Fatal(err)
			}
			if err := ExportArtifacts(root, "", filepath.Join(t.TempDir(), "out")); err == nil {
				t.Fatal("accepted colliding output paths")
			}
		})
	}
}
func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
func TestGCPreservesCurrentArtifactsAndRefusesLeases(t *testing.T) {
	root := acceptedFixture(t, nil)
	cell := filepath.Join(root, "cells", "a")
	names := []string{"1.stdout.json", "2.check-0.stderr.log", "3.stdout.json", "3.input.json", "unrecognized.txt"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(cell, name), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := runfixture.Read(t, root)
	report, err := GarbageCollect(root, 1, false)
	if err != nil || len(report.Files) != 2 || report.Applied {
		t.Fatalf("preview: %+v %v", report, err)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(cell, name)); err != nil {
			t.Fatal("preview deleted file", name)
		}
	}
	lease, err := checkpoint.FileLock(filepath.Join(cell, "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = GarbageCollect(root, 1, true)
	checkpoint.Unlock(lease)
	if err == nil {
		t.Fatal("GC accepted active worker lease")
	}
	controller, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = GarbageCollect(root, 1, true)
	checkpoint.Unlock(controller)
	if err == nil {
		t.Fatal("GC accepted active controller")
	}
	report, err = GarbageCollect(root, 1, true)
	if err != nil || !report.Applied {
		t.Fatalf("apply: %+v %v", report, err)
	}
	for _, name := range names[:2] {
		if _, err := os.Stat(filepath.Join(cell, name)); !os.IsNotExist(err) {
			t.Fatal("old file retained", name, err)
		}
	}
	for _, name := range names[2:] {
		if _, err := os.Stat(filepath.Join(cell, name)); err != nil {
			t.Fatal("current or unknown file deleted", name, err)
		}
	}
	after := runfixture.Read(t, root)
	if !reflect.DeepEqual(before.Tasks, after.Tasks) {
		t.Fatal("GC changed canonical task state")
	}
	if len(after.Events) != len(before.Events)+1 || after.Events[len(after.Events)-1].Kind != "gc" {
		t.Fatal("GC did not preserve history and append event")
	}
}

func TestGraphUsesSafeMermaidIdentifiers(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("end", 0), runfixture.Task("child-node", 0, "end"))
	output := GraphMermaid(runfixture.Read(t, root))
	if !strings.Contains(output, `n0["end: pending"]`) || !strings.Contains(output, "n0 --> n1") {
		t.Fatalf("unsafe or missing Mermaid nodes: %s", output)
	}
}
