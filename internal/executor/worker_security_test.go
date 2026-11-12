package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
)

func TestWorkerSecurityCancelledValidationStillFencesOldAttempts(t *testing.T) {
	for _, field := range []string{"run", "attempt", "operation", "binding"} {
		t.Run(field, func(t *testing.T) {
			row := &model.TaskState{Spec: model.Task{ID: "node"}, Status: "cancelled", CancelRequested: true, Attempt: 2, OperationID: "current", BindingDigest: "binding"}
			s := &model.State{RunID: "run", Tasks: map[string]*model.TaskState{"node": row}}
			fence := graph.FenceForCell(s, row)
			switch field {
			case "run":
				fence.RunID = "old"
			case "attempt":
				fence.Attempt--
			case "operation":
				fence.OperationID = "old"
			case "binding":
				fence.BindingDigest = "old"
			}
			err := recordCellValidation(s, CellValidation{Fence: fence, CleanupUnresolved: true}, time.Unix(100, 0))
			if !errors.Is(err, managed.ErrStaleCellAttempt) {
				t.Fatalf("stale %s accepted: %v", field, err)
			}
			if row.Status != "cancelled" || len(s.Events) != 0 {
				t.Fatal("stale result mutated cancellation")
			}
		})
	}
}

func TestWorkerSecurityCancelledValidationPreservesCleanupUncertainty(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		row := &model.TaskState{Spec: model.Task{ID: "node"}, Status: "cancelled", CancelRequested: true, Attempt: 1, OperationID: "current"}
		s := &model.State{RunID: "run", Tasks: map[string]*model.TaskState{"node": row}}
		err := recordCellValidation(s, CellValidation{Fence: graph.FenceForCell(s, row), CleanupUnresolved: unresolved}, time.Unix(100, 0))
		if err != nil {
			t.Fatalf("current cancelled result escaped as controller failure: %v", err)
		}
		want := "cancelled"
		if unresolved {
			want = "uncertain"
		}
		if row.Status != want {
			t.Fatalf("cleanup unresolved=%v: status %s, want %s", unresolved, row.Status, want)
		}
	}
}

func TestWorkerSecurityPaidLegacyCallsAreNeverImplicitlyReplaySafe(t *testing.T) {
	for _, tc := range []struct {
		backend, provider string
		safe              bool
	}{{"haiku", "", false}, {"mock", "haiku", false}, {"haiku", "mock", true}, {"mock", "", true}} {
		s := &model.State{Backend: tc.backend}
		if got := graph.ReplaySafe(s, model.Task{ID: "node", Provider: tc.provider}); got != tc.safe {
			t.Fatalf("backend=%s provider=%s replaySafe=%v", tc.backend, tc.provider, got)
		}
	}
}

func TestWorkerSecurityClaudeChildrenExcludeAmbientCredentials(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
answer=clean
if [ -n "${AWS_SECRET_ACCESS_KEY+x}" ] || [ -n "${GITHUB_TOKEN+x}" ] || [ -n "${ANTHROPIC_API_KEY+x}" ] || [ -n "${IRRIGATOR_LAUNCH_CONTEXT+x}" ]; then answer=leaked; fi
printf '{"type":"result","total_cost_usd":0,"structured_output":{"answer":"%s"}}\n' "$answer"
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "IRRIGATOR_LAUNCH_CONTEXT"} {
		t.Setenv(name, "synthetic-review-canary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, _, err := callHaiku(ctx, dir, 1, model.Task{ID: "test", Prompt: "fixture", Requires: []string{"answer"}}, nil, .01)
	if err != nil {
		t.Fatal(err)
	}
	if output["answer"] != "clean" {
		t.Fatal("builtin child inherited forbidden ambient credentials")
	}
	slot := &provider.ProviderSlot{}
	if err := slot.Start(provider.ProviderPoolOptions{Claude: script, Dir: dir, Model: "fixture"}, "key", `{"type":"object"}`, "low", 10000); err != nil {
		t.Fatal(err)
	}
	defer slot.Stop()
	select {
	case line := <-slot.Lines:
		if !strings.Contains(string(line), `"answer":"clean"`) {
			t.Fatal("pool child inherited forbidden ambient credentials")
		}
	case <-ctx.Done():
		t.Fatal("fixture child timed out")
	}
}
