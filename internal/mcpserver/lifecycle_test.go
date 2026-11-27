package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExplicitMCPProfilesBoundInventoriesAndControlGate(t *testing.T) {
	for _, tc := range []struct {
		profile string
		control bool
		count   int
	}{
		{"inspection", false, 10}, {"inspection", true, 10}, {"execution", true, 15}, {"delivery", false, 7}, {"delivery", true, 9}, {"legacy", true, 15},
	} {
		t.Run(tc.profile+map[bool]string{false: "-read", true: "-control"}[tc.control], func(t *testing.T) {
			cs := connectDefinitionTest(t, McpConfig{Workspace: t.TempDir(), Profile: tc.profile, AllowControl: tc.control})
			result, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Tools) != tc.count {
				t.Fatalf("got %d tools, want %d", len(result.Tools), tc.count)
			}
			for _, tool := range result.Tools {
				if tool.Description == "" || tool.OutputSchema == nil {
					t.Fatal("incomplete contract", tool.Name)
				}
				if tc.profile == "execution" && (tool.Name == "review_list" || tool.Name == "inspect_review") {
					t.Fatal("review tools leaked into execution")
				}
				if !tc.control || tc.profile == "inspection" {
					if !tool.Annotations.ReadOnlyHint {
						t.Fatal("mutation advertised in inspection", tool.Name)
					}
				}
			}
		})
	}
}

func TestMCPCancelCellFencesRequestAndNeverRepeatsEvents(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("cell", 0))
	s := runfixture.Read(t, root)
	cs := connectDefinitionTest(t, McpConfig{Workspace: t.TempDir(), Profile: "execution", AllowControl: true})
	args := map[string]any{"run": root, "request_id": "cancel-one", "expected_run_id": s.RunID, "expected_plan_sha256": s.PlanHash, "expected_graph_revision": 0, "cell": "cell", "expected_operation_id": s.Tasks["cell"].OperationID}
	first := callParameterFixture(t, cs, "cancel_cell", args)
	if first["changed"] != true || first["cleanup_confirmed"] != false || first["dispatched"] != false {
		t.Fatal(first)
	}
	before, _ := os.ReadFile(filepath.Join(root, "state.json"))
	callParameterFixture(t, cs, "cancel_cell", args)
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if string(before) != string(after) {
		t.Fatal("duplicate MCP cancellation changed checkpoint")
	}
	args["expected_operation_id"] = "later-operation"
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cancel_cell", Arguments: args})
	if err == nil && !result.IsError {
		t.Fatal("reused request identity changed target")
	}
	readonly := connectDefinitionTest(t, McpConfig{Workspace: t.TempDir(), Profile: "inspection", AllowControl: true})
	if _, err := readonly.CallTool(context.Background(), &mcp.CallToolParams{Name: "cancel_cell", Arguments: args}); err == nil {
		t.Fatal("inspection exposed cancellation")
	}
}

func TestNewMCPSchemasRequireExplicitIdentity(t *testing.T) {
	for _, name := range []string{"cancel_cell", "stop_run", "assemble_delivery", "check_delivery"} {
		schema := mcpInputSchema(name)
		raw, _ := json.Marshal(schema)
		var parsed struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, field := range parsed.Required {
			seen[field] = true
		}
		for _, field := range []string{"request_id", "expected_run_id", "expected_plan_sha256", "expected_graph_revision"} {
			if !seen[field] {
				t.Fatal(name, "missing required identity", field)
			}
		}
	}
}
