package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPSanitizeRedactsCredentialFields(t *testing.T) {
	got, _ := mcpSanitize(map[string]any{"api_key": "k", "nested": map[string]any{"Authorization": "Bearer x"}, "env": []string{"A=b"}, "ok": "fine"})
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), `"k"`) || strings.Contains(string(raw), "A=b") || !strings.Contains(string(raw), "fine") {
		t.Fatalf("sanitized: %s", raw)
	}
	if s, cut := mcpTruncate(strings.Repeat("é", 10), 5); !cut || !strings.HasPrefix(s, "éé") {
		t.Fatalf("rune-boundary truncation: %q", s)
	}
}

func TestMCPToolAnnotationsAndSchemas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := NewMCPServer(McpConfig{Workspace: t.TempDir(), AllowControl: true})
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	type wantHints struct {
		readOnly, destructive, idempotent, openWorld bool
	}
	specs := map[string]wantHints{
		"list_runs":       {true, false, true, false},
		"run_status":      {true, false, true, false},
		"run_logs":        {true, false, true, false},
		"inspect_cell":    {true, false, true, false},
		"review_list":     {true, false, true, false},
		"validate_plan":   {true, false, true, false},
		"apply":           {false, true, false, false},
		"pause":           {false, false, true, false},
		"resume":          {false, false, false, false},
		"retry_cell":      {false, true, false, false},
		"decide_approval": {false, true, false, false},
	}

	if len(res.Tools) != len(specs) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(specs))
	}
	seen := map[string]bool{}
	for _, tool := range res.Tools {
		seen[tool.Name] = true
		want, ok := specs[tool.Name]
		if !ok {
			t.Fatalf("unexpected tool %q", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Fatalf("tool %q missing OutputSchema", tool.Name)
		}
		ann := tool.Annotations
		if ann == nil || ann.DestructiveHint == nil || ann.OpenWorldHint == nil {
			t.Fatalf("tool %q has incomplete ToolAnnotations: %+v", tool.Name, ann)
		}
		if ann.ReadOnlyHint && *ann.DestructiveHint {
			t.Fatalf("tool %q has contradictory readOnlyHint=true and destructiveHint=true", tool.Name)
		}
		if ann.ReadOnlyHint != want.readOnly || *ann.DestructiveHint != want.destructive || ann.IdempotentHint != want.idempotent || *ann.OpenWorldHint != want.openWorld {
			t.Fatalf("tool %q annotations = (readOnly=%v, destructive=%v, idempotent=%v, openWorld=%v), want %+v",
				tool.Name, ann.ReadOnlyHint, *ann.DestructiveHint, ann.IdempotentHint, *ann.OpenWorldHint, want)
		}
	}
	for name := range specs {
		if !seen[name] {
			t.Fatalf("missing expected tool %q", name)
		}
	}
}

func TestMCPRunStatusAndInspectCellLastActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status, s.Tasks["a"].Attempt = "running", 2
		s.Tasks["b"].Status, s.Tasks["b"].Attempt = "pending", 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "cells", "a", "2.stderr.log")
	if err := os.WriteFile(logPath, []byte("earlier progress\nprovider streaming \x1b[33mchunk 7\x07\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := NewMCPServer(McpConfig{Workspace: t.TempDir()})
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	statusRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "run_status", Arguments: map[string]any{"run": root}})
	if err != nil || statusRes.IsError {
		t.Fatalf("run_status failed: %v %+v", err, statusRes)
	}
	statusMap, _ := statusRes.StructuredContent.(map[string]any)
	cells, _ := statusMap["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("expected 2 cells, got %v", cells)
	}
	cellA, _ := cells[0].(map[string]any)
	cellB, _ := cells[1].(map[string]any)
	actA, ok := cellA["last_activity"].(map[string]any)
	if !ok {
		t.Fatalf("run_status cell a missing last_activity: %v", cellA)
	}
	if actA["attempt"] != float64(2) || actA["line"] != `provider streaming \e[33mchunk 7\a` || actA["truncated"] != false || actA["updated_at"] == "" {
		t.Fatalf("unexpected run_status last_activity: %+v", actA)
	}
	if _, exists := cellB["last_activity"]; exists {
		t.Fatalf("cell b without log should not have last_activity: %v", cellB)
	}

	inspectRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "inspect_cell", Arguments: map[string]any{"run": root, "cell": "a"}})
	if err != nil || inspectRes.IsError {
		t.Fatalf("inspect_cell failed: %v %+v", err, inspectRes)
	}
	inspectMap, _ := inspectRes.StructuredContent.(map[string]any)
	inspectAct, ok := inspectMap["last_activity"].(map[string]any)
	if !ok || inspectAct["line"] != `provider streaming \e[33mchunk 7\a` {
		t.Fatalf("inspect_cell missing or wrong last_activity: %v", inspectMap)
	}
}
