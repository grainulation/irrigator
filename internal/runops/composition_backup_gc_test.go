package runops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/composition"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Fixtures deliberately enter through on-disk JSON.
func compositionPlanFixture(t *testing.T, mutate func(map[string]any, map[string]any)) string {
	t.Helper()
	dir := runfixture.CompositionTempDir(t)
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	template := map[string]any{"schema": "irrigator.workflow/v1", "version": "1", "inputs": map[string]any{"question": "string"}, "nodes": []any{map[string]any{"id": "work", "prompt": "Answer the question", "profile": "small", "inputs": map[string]any{"question": map[string]any{"input": "question"}}, "outputs": map[string]any{"answer": "string"}}}, "exports": map[string]any{"answer": map[string]any{"node": "work", "port": "answer"}}}
	config := map[string]any{"schema": "irrigator.composition/v1", "name": "review", "execution": model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, RunBudgetUSD: "0", CostPolicy: "retain_unknown"}, "max_tasks": 2, "max_input_bytes": 1024, "max_output_bytes": 1024, "calls": []any{map[string]any{"id": "first", "template": map[string]any{"path": "workflow.json", "version": "1", "sha256": ""}, "inputs": map[string]any{"question": map[string]any{"literal": "What is safe?"}}}}, "exports": map[string]any{"answer": map[string]any{"call": "first", "port": "answer"}}}
	if mutate != nil {
		mutate(config, template)
	}
	raw, e := json.Marshal(template)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(raw)
	pin := config["calls"].([]any)[0].(map[string]any)["template"].(map[string]any)
	if pin["sha256"] == "" {
		pin["sha256"] = hex.EncodeToString(h[:])
	}
	if e = os.WriteFile(filepath.Join(dir, "workflow.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	raw, e = json.Marshal(config)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "composition.json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestCompositionDeterministic(t *testing.T) {
	path := compositionPlanFixture(t, nil)
	p, a, e := composition.CompileComposition(path)
	if e != nil {
		t.Fatal(e)
	}
	q, b, e := composition.CompileComposition(path)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p, q) || !reflect.DeepEqual(a, b) {
		t.Fatal("identical inputs compiled differently")
	}
	if len(p.Tasks) != 1 {
		t.Fatalf("unexpected tasks: %+v", p.Tasks)
	}
}

func TestCompositionRejectsInvalidContracts(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"pin": func(c, w map[string]any) {
			c["calls"].([]any)[0].(map[string]any)["template"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
		},
		"version":          func(c, w map[string]any) { w["version"] = "different" },
		"unsupported_type": func(c, w map[string]any) { w["inputs"].(map[string]any)["question"] = "json-object" },
		"unknown_input": func(c, w map[string]any) {
			c["calls"].([]any)[0].(map[string]any)["inputs"].(map[string]any)["unknown"] = map[string]any{"literal": "injected"}
		},
		"unknown_export": func(c, w map[string]any) {
			c["exports"].(map[string]any)["answer"].(map[string]any)["port"] = "missing"
		},
		"authority": func(c, w map[string]any) { w["nodes"].([]any)[0].(map[string]any)["profile"] = "unapproved" },
		"node_cycle": func(c, w map[string]any) {
			w["nodes"].([]any)[0].(map[string]any)["inputs"].(map[string]any)["question"] = map[string]any{"node": "work", "port": "answer"}
		},
		"call_cycle": func(c, w map[string]any) {
			c["calls"].([]any)[0].(map[string]any)["inputs"].(map[string]any)["question"] = map[string]any{"call": "first", "port": "answer"}
		},
		"input_limit":  func(c, w map[string]any) { c["max_input_bytes"] = 1 },
		"output_limit": func(c, w map[string]any) { c["max_output_bytes"] = 1 },
		"expanded_limit": func(c, w map[string]any) {
			c["max_tasks"] = 1
			nodes := w["nodes"].([]any)
			extra := map[string]any{}
			for k, v := range nodes[0].(map[string]any) {
				extra[k] = v
			}
			extra["id"] = "second"
			w["nodes"] = append(nodes, extra)
		},
		"unused_invalid_input": func(c, w map[string]any) {
			w["nodes"].([]any)[0].(map[string]any)["inputs"] = map[string]any{}
			c["calls"].([]any)[0].(map[string]any)["inputs"].(map[string]any)["question"] = map[string]any{"call": "missing", "port": "missing"}
		},
		"parent_escape": func(c, w map[string]any) {
			c["calls"].([]any)[0].(map[string]any)["template"].(map[string]any)["path"] = "../workflow.json"
		},
		"unknown_field": func(c, w map[string]any) { w["grant_budget"] = 100 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := compositionPlanFixture(t, mutate)
			if _, _, e := composition.CompileComposition(path); e == nil {
				t.Fatal("invalid composition accepted")
			}
		})
	}
}

func TestBackupTamperRejected(t *testing.T) {
	root := runfixture.CompositionRun(t)
	dir := runfixture.CompositionTempDir(t)
	bundle := filepath.Join(dir, "snapshot.json")
	if e := backup.BackupCreate(root, bundle); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(bundle)
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	files, ok := doc["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatal("backup lacks evidence")
	}
	files[0].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	raw, e = json.Marshal(doc)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(bundle, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = backup.BackupRestore(bundle, filepath.Join(dir, "clone")); e == nil {
		t.Fatal("tampered evidence accepted")
	}
}

func TestBackupSymlinkDoesNotReadOutside(t *testing.T) {
	root := runfixture.CompositionRun(t)
	dir := runfixture.CompositionTempDir(t)
	secret := filepath.Join(dir, "secret")
	if e := os.WriteFile(secret, []byte("outside-private-canary"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(secret, filepath.Join(root, "outside")); e != nil {
		t.Fatal(e)
	}
	bundle := filepath.Join(dir, "snapshot.json")
	if e := backup.BackupCreate(root, bundle); e == nil {
		t.Fatal("symlink in source accepted")
	}
}

func TestBackupActiveLeaseRejected(t *testing.T) {
	for _, lock := range []string{"controller.lock", "cells/a/lease"} {
		t.Run(lock, func(t *testing.T) {
			root := runfixture.CompositionRun(t)
			lease, e := checkpoint.FileLock(filepath.Join(root, lock), true)
			if e != nil {
				t.Fatal(e)
			}
			defer checkpoint.Unlock(lease)
			if e = backup.BackupCreate(root, filepath.Join(runfixture.CompositionTempDir(t), "snapshot.json")); e == nil {
				t.Fatal("active source snapshot accepted")
			}
		})
	}
}

func TestBackupCorruptStateAndDestinationRejected(t *testing.T) {
	root := runfixture.CompositionRun(t)
	dir := runfixture.CompositionTempDir(t)
	bundle := filepath.Join(dir, "snapshot.json")
	if e := backup.BackupCreate(root, bundle); e != nil {
		t.Fatal(e)
	}
	destination := filepath.Join(dir, "occupied")
	if e := os.Mkdir(destination, 0700); e != nil {
		t.Fatal(e)
	}
	sentinel := filepath.Join(destination, "sentinel")
	if e := os.WriteFile(sentinel, []byte("preserve"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := backup.BackupRestore(bundle, destination); e == nil {
		t.Fatal("existing destination overwritten")
	}
	b, e := os.ReadFile(sentinel)
	if e != nil || string(b) != "preserve" {
		t.Fatal("existing destination changed")
	}
	if e = os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"version":`), 0600); e != nil {
		t.Fatal(e)
	}
	if e = backup.BackupCreate(root, filepath.Join(dir, "corrupt.json")); e == nil {
		t.Fatal("corrupt checkpoint backed up")
	}
}

func TestBackupArchiveTraversalRejected(t *testing.T) {
	root := runfixture.CompositionRun(t)
	dir := runfixture.CompositionTempDir(t)
	bundle := filepath.Join(dir, "snapshot.json")
	if e := backup.BackupCreate(root, bundle); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(bundle)
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"../escape", "/absolute", "evidence/../../escape", "RESTORE_QUARANTINE.json"} {
		t.Run(bad, func(t *testing.T) {
			var doc map[string]any
			if e := json.Unmarshal(raw, &doc); e != nil {
				t.Fatal(e)
			}
			doc["files"].([]any)[0].(map[string]any)["path"] = bad
			altered, e := json.Marshal(doc)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(dir, "altered.json")
			if e = os.WriteFile(path, altered, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = backup.BackupRead(path); e == nil {
				t.Fatal("unsafe archive path accepted")
			}
		})
	}
}

func TestCompositionCLIExclusiveOutput(t *testing.T) {
	path := compositionPlanFixture(t, nil)
	out := filepath.Join(filepath.Dir(path), "plan.json")
	if code, e := composition.CompositionCLI([]string{path, "--out", out}); e != nil || code != 0 {
		t.Fatalf("compose CLI: %d %v", code, e)
	}
	before, e := os.ReadFile(out)
	if e != nil {
		t.Fatal(e)
	}
	if code, e := composition.CompositionCLI([]string{path, "--out", out}); e == nil || code == 0 {
		t.Fatal("compose overwrote existing plan")
	}
	after, e := os.ReadFile(out)
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("existing plan modified")
	}
	if _, e := checkpoint.LoadPlan(out); e != nil {
		t.Fatalf("compiled CLI output is not runnable plan: %v", e)
	}
}

func TestCompositionConfinedSymlinkAndFIFO(t *testing.T) {
	path := compositionPlanFixture(t, nil)
	template := filepath.Join(filepath.Dir(path), "workflow.json")
	raw, e := os.ReadFile(template)
	if e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(runfixture.CompositionTempDir(t), "outside.json")
	if e = os.WriteFile(outside, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(template); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, template); e != nil {
		t.Fatal(e)
	}
	if _, _, e = composition.CompileComposition(path); e == nil {
		t.Fatal("external symlink template accepted")
	}
	if e = os.Remove(template); e != nil {
		t.Fatal(e)
	}
	if e = syscall.Mkfifo(template, 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = composition.CompileComposition(path); e == nil {
		t.Fatal("FIFO template accepted")
	}
}

func TestQuarantineGCAndExportPreserveFiles(t *testing.T) {
	root, e := filepath.EvalSymlinks(acceptedFixture(t, nil))
	if e != nil {
		t.Fatal(e)
	}
	victim := filepath.Join("cells", "a", "1.stdout.json")
	if e = os.WriteFile(filepath.Join(root, victim), []byte("must retain attempt evidence"), 0600); e != nil {
		t.Fatal(e)
	}
	dir := runfixture.CompositionTempDir(t)
	bundle := filepath.Join(dir, "backup.json")
	clone := filepath.Join(dir, "clone")
	if e = backup.BackupCreate(root, bundle); e != nil {
		t.Fatal(e)
	}
	if e = backup.BackupRestore(bundle, clone); e != nil {
		t.Fatal(e)
	}
	evidence := filepath.Join(clone, "evidence")
	before := map[string][]byte{}
	for _, p := range []string{filepath.Join(evidence, victim), filepath.Join(evidence, "state.json"), filepath.Join(clone, checkpoint.BackupQuarantine)} {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		before[p] = b
	}
	if _, e = GarbageCollect(evidence, 1, true); e == nil {
		t.Fatal("quarantine gc apply accepted")
	}
	for _, p := range []string{filepath.Join(evidence, "state.json"), filepath.Join(clone, checkpoint.BackupQuarantine)} {
		if e = ExportArtifacts(root, p, ""); e == nil {
			t.Fatal("export wrote into quarantine")
		}
	}
	for p, b := range before {
		after, e := os.ReadFile(p)
		if e != nil || !reflect.DeepEqual(after, b) {
			t.Fatalf("protected file changed: %s %v", p, e)
		}
	}
	if e = ExportArtifacts(evidence, filepath.Join(dir, "external-export.json"), ""); e != nil {
		t.Fatalf("read-only restored evidence export failed: %v", e)
	}
}

func TestGCRetainsLegacyUncertainty(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if e := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; s.Tasks["a"].Attempt = 3; return nil }); e != nil {
		t.Fatal(e)
	}
	old := filepath.Join(root, "cells", "a", "1.stdout.json")
	if e := os.WriteFile(old, []byte("uncertain history"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := GarbageCollect(root, 1, true); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(old)
	if e != nil || string(b) != "uncertain history" {
		t.Fatalf("legacy uncertainty history erased: %v", e)
	}
}
