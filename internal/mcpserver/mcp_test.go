package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
