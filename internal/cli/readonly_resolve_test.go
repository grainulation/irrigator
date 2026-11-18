package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func initTestRun(t *testing.T, root string) *model.State {
	t.Helper()
	p := model.Plan{
		APIVersion: "swarm/v1",
		Name:       "resolve-test",
		Tasks: []model.Task{
			{
				ID:         "build",
				Prompt:     "Build the artifact",
				Requires:   []string{"answer"},
				MockResult: map[string]string{"answer": "ok"},
			},
		},
	}
	if err := checkpoint.Initialize(p, root, "mock", model.Options{Workers: 1, Timeout: 10 * time.Second, Budget: 0.05}); err != nil {
		t.Fatal(err)
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveReadOnlyRunDirAndCommands(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(ws); err == nil {
		ws = resolved
	}
	if err := os.Chdir(ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", "")

	runAlpha := filepath.Join("runs", "alpha-20261117")
	sAlpha := initTestRun(t, runAlpha)

	runShort := filepath.Join("runs", "r1")
	_ = initTestRun(t, runShort)

	// 1. Exact directory path match (100% backward compatible).
	got, err := checkpoint.ResolveReadOnlyRunDir(runAlpha)
	if err != nil || got != runAlpha {
		t.Fatalf("exact path: got %q, err %v", got, err)
	}

	// 2. Exact directory basename under ./runs/<arg> (even when < 4 chars).
	got, err = checkpoint.ResolveReadOnlyRunDir("r1")
	if err != nil || got != runShort {
		t.Fatalf("short exact basename under ./runs: got %q, err %v", got, err)
	}

	// 3. Prefix match (< 4 chars) when not an exact basename must fail closed.
	if _, err := checkpoint.ResolveReadOnlyRunDir("alp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("short prefix < 4 chars must fail closed with not-exist, got %v", err)
	}

	// 4. Unambiguous directory prefix (>= 4 chars), both bare and under ./runs/.
	for _, query := range []string{"alpha", "alpha-2026", "runs/alpha"} {
		got, err := checkpoint.ResolveReadOnlyRunDir(query)
		if err != nil || got != runAlpha {
			t.Fatalf("prefix %q: got %q, err %v", query, got, err)
		}
	}

	// 5. Unambiguous state.json run_id prefix (>= 4 chars).
	idPrefix := sAlpha.RunID[:8]
	got, err = checkpoint.ResolveReadOnlyRunDir(idPrefix)
	if err != nil || got != runAlpha {
		t.Fatalf("run_id prefix %q: got %q, err %v", idPrefix, got, err)
	}

	// 6. Session registry entry resolution (outside ./runs/).
	extRun := filepath.Join(ws, "external", "custom-session-run")
	sExt := initTestRun(t, extRun)
	regDir := filepath.Join(ws, ".irrigator")
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	regJSON := map[string]any{
		"version":        2,
		"generation":     1,
		"current_run_id": sExt.RunID,
		"updated_at":     time.Now().UTC().Format(time.RFC3339),
		"entries": []map[string]any{
			{
				"run_id":          sExt.RunID,
				"run_path":        extRun,
				"started_at":      time.Now().UTC().Format(time.RFC3339),
				"last_started_at": time.Now().UTC().Format(time.RFC3339),
				"generation":      1,
			},
		},
	}
	rawReg, _ := json.Marshal(regJSON)
	if err := os.WriteFile(filepath.Join(regDir, "current-session.json"), rawReg, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = checkpoint.ResolveReadOnlyRunDir(sExt.RunID[:8])
	if err != nil || got != extRun {
		t.Fatalf("session registry run_id prefix: got %q, err %v", got, err)
	}
	got, err = checkpoint.ResolveReadOnlyRunDir("custom-session")
	if err != nil || got != extRun {
		t.Fatalf("session registry dirname prefix: got %q, err %v", got, err)
	}

	// 7. Wire check: all 7 read-only CLI commands (status, inspect, logs, debug, graph, triage, cost)
	// resolve unambiguous prefixes and project per-attempt last_activity in status --json and inspect.
	if err := checkpoint.Update(runAlpha, func(s *model.State) error {
		s.Tasks["build"].Status = "running"
		s.Tasks["build"].Attempt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runAlpha, "cells", "build", "1.stderr.log")
	if err := os.WriteFile(logPath, []byte("step 1: starting\nstep 2: compiling\x1b[32m module\x07\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"status", []string{"status", "alpha"}, "alpha-20261117"},
		{"inspect", []string{"inspect", "alpha", "build"}, `"last_activity"`},
		{"logs", []string{"logs", "alpha", "--json"}, `"created"`},
		{"debug", []string{"debug", "alpha", "--json"}, sAlpha.RunID},
		{"graph", []string{"graph", "alpha"}, "flowchart TD"},
		{"triage", []string{"triage", "alpha", "--json"}, sAlpha.RunID},
		{"cost", []string{"cost", "alpha", "--json"}, `"cells"`},
	} {
		out := runfixture.CaptureStdout(t, func() {
			code, err := runCommand(tc.args)
			if err != nil || code != 0 {
				t.Fatalf("%s (%v): exit %d, err %v", tc.name, tc.args, code, err)
			}
		})
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s output missing %q:\n%s", tc.name, tc.want, out)
		}
	}

	// Verify status --json includes sanitized per-attempt last_activity.
	statusRaw := runfixture.CaptureStdout(t, func() {
		code, err := runCommand([]string{"status", "alpha", "--json"})
		if err != nil || code != 0 {
			t.Fatalf("status --json: %d %v", code, err)
		}
	})
	var statusDoc map[string]any
	if err := json.Unmarshal([]byte(statusRaw), &statusDoc); err != nil {
		t.Fatalf("unmarshal status --json: %v\n%s", err, statusRaw)
	}
	tasksMap, _ := statusDoc["tasks"].(map[string]any)
	buildCell, _ := tasksMap["build"].(map[string]any)
	lastAct, ok := buildCell["last_activity"].(map[string]any)
	if !ok {
		t.Fatalf("status --json missing tasks.build.last_activity: %v", statusDoc)
	}
	if lastAct["attempt"] != float64(1) || lastAct["line"] != `step 2: compiling\e[32m module\a` || lastAct["truncated"] != false || lastAct["updated_at"] == "" {
		t.Fatalf("unexpected last_activity: %+v", lastAct)
	}

	// 8. Ambiguous prefix (>1 matching runs) must fail closed and list candidates.
	runAlpha2 := filepath.Join("runs", "alpha-20261118")
	_ = initTestRun(t, runAlpha2)
	_, err = checkpoint.ResolveReadOnlyRunDir("alpha")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "alpha-20261117") || !strings.Contains(err.Error(), "alpha-20261118") {
		t.Fatalf("expected ambiguous error listing both candidates, got: %v", err)
	}
	for _, cmdArgs := range [][]string{
		{"status", "alpha"},
		{"inspect", "alpha", "build"},
		{"logs", "alpha"},
		{"debug", "alpha"},
		{"graph", "alpha"},
		{"triage", "alpha"},
		{"cost", "alpha"},
	} {
		code, err := runCommand(cmdArgs)
		if code == 0 || err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("read-only command %v must fail closed on ambiguous prefix: code=%d err=%v", cmdArgs, code, err)
		}
	}

	// 9. Safety invariant: mutating commands NEVER apply prefix/implicit resolution!
	resFile := filepath.Join(ws, "result.json")
	if err := os.WriteFile(resFile, []byte(`{"answer":"ok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(ws, "plan.json")
	if code, err := runCommand([]string{"init", "--out", planFile}); code != 0 || err != nil {
		t.Fatalf("init: %d %v", code, err)
	}
	for _, mutArgs := range [][]string{
		{"resume", "custom-session"},
		{"retry", "custom-session", "build"},
		{"resolve", "custom-session", "build", "--result", resFile},
		{"cancel", "custom-session", "build"},
		{"pause", "custom-session"},
		{"down", "custom-session"},
		{"gc", "custom-session"},
		{"recover", "--run", "custom-session", "--in", resFile, "--out", filepath.Join(ws, "rec-out")},
	} {
		code, err := runCommand(mutArgs)
		if code == 0 || err == nil {
			t.Fatalf("mutating command %v resolved prefix/bare name instead of requiring explicit path", mutArgs)
		}
	}
}
